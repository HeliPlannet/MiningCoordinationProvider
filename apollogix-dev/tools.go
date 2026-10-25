package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ---------- health_overview ----------

type verdict struct {
	Label string
	Note  string
}

// classify combines the three independent signals. The docker healthcheck is
// deliberately not trusted on its own: on this host fms-backend-service probes
// port 8081 while the app listens on 8091, so docker reports unhealthy for a
// service that answers 200.
func classify(s Svc, p ProbeResult, probed bool) verdict {
	if s.State != "running" {
		return verdict{"DOWN", fmt.Sprintf("state=%s exit=%d", s.State, s.ExitCode)}
	}
	httpOK := false
	if code, err := strconv.Atoi(p.HTTPCode); err == nil && code >= 200 && code < 400 {
		httpOK = true
	}

	switch {
	case probed && httpOK && s.HasHC && s.HCStatus == "unhealthy":
		return verdict{"SERVING", "signal mismatch: docker healthcheck fails but the port answers " + p.HTTPCode + " - suspect a misconfigured healthcheck"}
	case probed && httpOK:
		if !s.HasHC {
			return verdict{"SERVING", "no docker healthcheck; verdict from host probe only"}
		}
		return verdict{"SERVING", ""}
	case s.HasHC && s.HCStatus == "healthy":
		if probed && p.Skipped == "" && !p.TCPOpen {
			return verdict{"SUSPECT", "docker reports healthy but the published port is not reachable from the host"}
		}
		return verdict{"HEALTHY", "docker healthcheck only"}
	case s.HasHC && s.HCStatus == "unhealthy":
		return verdict{"UNHEALTHY", fmt.Sprintf("failing streak %d; last probe exit %d", s.HCStreak, s.HCLastExit)}
	case s.HasHC && s.HCStatus == "starting":
		return verdict{"STARTING", "healthcheck still in start period"}
	case probed && p.TCPOpen && !httpOK:
		note := "port accepts TCP but HTTP " + p.HTTPPath + " returned " + p.HTTPCode
		if p.HTTPCode == "" {
			note = "port accepts TCP but the HTTP probe failed: " + p.HTTPErr
		}
		return verdict{"PARTIAL", note + "; no docker healthcheck to corroborate"}
	case probed && p.Skipped != "":
		return verdict{"UNKNOWN", "no docker healthcheck and " + p.Skipped}
	case probed && !p.TCPOpen:
		return verdict{"SUSPECT", "no docker healthcheck and the published port is not reachable"}
	}
	return verdict{"UNKNOWN", "no docker healthcheck; probe disabled"}
}

func handleHealthOverview(paramPatterns string, probe bool) (string, error) {
	t0 := time.Now()
	sc, err := resolveScope(paramPatterns)
	if err != nil {
		return "", err
	}
	svcs := sc.Svcs
	if len(svcs) == 0 {
		return fmt.Sprintf("No container selected on %s.\n%s\n\nAll three layers failed: the call parameters, the configured default and the hardcoded %s fallback each matched nothing.\nMatching runs on the compose service name and falls back to the container name.\n",
			appCfg.SSHHost, sc.ScopeLine(), patternsLabel(hardcodedPatterns)), nil
	}

	probes := make([]ProbeResult, len(svcs))
	if probe {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i := range svcs {
			if svcs[i].State != "running" {
				continue
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				probes[i] = probeSvc(svcs[i], "", 0)
			}(i)
		}
		wg.Wait()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Health overview - %s (engine API %s)\n", appCfg.SSHHost, dockerAPIVersion())
	fmt.Fprintf(&b, "%s\n", sc.ScopeLine())
	fmt.Fprintf(&b, "matched %d container(s) | host probe: %s\n\n",
		len(svcs), map[bool]string{true: "on", false: "off"}[probe])

	fmt.Fprintf(&b, "%-28s %-11s %-14s %-22s %-11s %s\n",
		"SERVICE", "VERDICT", "DOCKER-HEALTH", "PROBE", "RUNTIME", "NOTE")

	var noHC, mismatch, bad []string
	for i, s := range svcs {
		v := classify(s, probes[i], probe)

		dh := "none"
		if s.HasHC {
			dh = s.HCStatus
			if s.HCStreak > 0 {
				dh = fmt.Sprintf("%s(%d)", s.HCStatus, s.HCStreak)
			}
		} else if s.State == "running" {
			noHC = append(noHC, s.Service)
		}

		pr := "-"
		if probe && s.State == "running" {
			p := probes[i]
			switch {
			case p.Skipped != "":
				pr = "no-port"
			case p.HTTPCode != "":
				pr = fmt.Sprintf(":%d http=%s %ss", p.Port, p.HTTPCode, trimFloat(p.HTTPTime))
			case p.TCPOpen:
				pr = fmt.Sprintf(":%d tcp-open", p.Port)
			default:
				pr = fmt.Sprintf(":%d unreachable", p.Port)
			}
		}

		rt := s.State
		if s.Restarts > 0 {
			rt = fmt.Sprintf("%s r=%d", s.State, s.Restarts)
		}

		if strings.Contains(v.Note, "signal mismatch") {
			mismatch = append(mismatch, s.Service)
		}
		if v.Label == "DOWN" || v.Label == "UNHEALTHY" || v.Label == "SUSPECT" {
			bad = append(bad, s.Service+" ("+v.Label+")")
		}

		fmt.Fprintf(&b, "%-28s %-11s %-14s %-22s %-11s %s\n",
			truncStr(s.Service, 28), v.Label, dh, pr, rt, v.Note)
	}

	b.WriteString("\n")
	if len(bad) > 0 {
		fmt.Fprintf(&b, "ATTENTION: %s\n", strings.Join(bad, ", "))
	}
	if len(mismatch) > 0 {
		fmt.Fprintf(&b, "SIGNAL MISMATCH (%d): %s\n", len(mismatch), strings.Join(mismatch, ", "))
		for _, i := range indexesOf(svcs, mismatch) {
			fmt.Fprintf(&b, "  %s healthcheck: %s\n", svcs[i].Service, svcs[i].HCTest)
			fmt.Fprintf(&b, "  %s host probe : :%d%s -> %s\n", svcs[i].Service, probes[i].Port, probes[i].HTTPPath, probes[i].HTTPCode)
		}
	}
	if len(noHC) > 0 {
		fmt.Fprintf(&b, "NO DOCKER HEALTHCHECK (%d): %s\n", len(noHC), strings.Join(noHC, ", "))
		b.WriteString("  For these the verdict rests on the host probe alone; docker only knows whether the process is running.\n")
	}
	if len(bad) == 0 && len(mismatch) == 0 {
		b.WriteString("All matched services are serving.\n")
	}
	fmt.Fprintf(&b, "\nCollected in %s.\n", time.Since(t0).Round(time.Millisecond))
	return b.String(), nil
}

func indexesOf(svcs []Svc, names []string) []int {
	var out []int
	for i, s := range svcs {
		for _, n := range names {
			if s.Service == n {
				out = append(out, i)
			}
		}
	}
	return out
}

func trimFloat(s string) string {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return strconv.FormatFloat(f, 'f', 3, 64)
	}
	return s
}

// ---------- health_probe ----------

func handleHealthProbe(service, pathOverride string, port int) (string, error) {
	s, err := findSvc(service)
	if err != nil {
		return "", err
	}
	p := probeSvc(s, pathOverride, port)

	var b strings.Builder
	fmt.Fprintf(&b, "Probe %s (container %s, project %s) on %s\n\n", s.Service, s.Container, s.Project, appCfg.SSHHost)
	fmt.Fprintf(&b, "runtime        : state=%s restarts=%d", s.State, s.Restarts)
	if !s.StartedAt.IsZero() {
		fmt.Fprintf(&b, " uptime=%s", compactDur(time.Since(s.StartedAt)))
	}
	b.WriteString("\n")

	if s.HasHC {
		fmt.Fprintf(&b, "docker health  : %s (failing streak %d, last probe exit %d)\n", s.HCStatus, s.HCStreak, s.HCLastExit)
		fmt.Fprintf(&b, "healthcheck cmd: %s\n", s.HCTest)
		if s.HCLastOut != "" {
			fmt.Fprintf(&b, "last probe out : %s\n", truncStr(firstLine(s.HCLastOut), 200))
		}
	} else {
		b.WriteString("docker health  : none defined for this container\n")
	}

	fmt.Fprintf(&b, "published ports: %v\n", s.Ports)
	if p.Skipped != "" {
		fmt.Fprintf(&b, "host probe     : skipped - %s\n", p.Skipped)
		b.WriteString("\nPass an explicit port to probe a container port that is not published.\n")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "tcp 127.0.0.1:%d : %s\n", p.Port, map[bool]string{true: "open", false: "closed/unreachable - " + p.TCPErr}[p.TCPOpen])
	if p.HTTPCode != "" {
		fmt.Fprintf(&b, "http %-10s: %s in %ss\n", p.HTTPPath, p.HTTPCode, trimFloat(p.HTTPTime))
	} else if p.HTTPErr != "" {
		fmt.Fprintf(&b, "http %-10s: %s\n", p.HTTPPath, p.HTTPErr)
	}

	v := classify(s, p, true)
	fmt.Fprintf(&b, "\nverdict        : %s", v.Label)
	if v.Note != "" {
		fmt.Fprintf(&b, " - %s", v.Note)
	}
	b.WriteString("\n")
	b.WriteString("\nProbes run on the dev host because published ports are firewalled from outside it.\n")
	return b.String(), nil
}

// ---------- stats_snapshot ----------

type statRow struct {
	Svc      string
	CPUPct   float64
	MemUsed  uint64
	MemLimit uint64
	MemPct   float64
	NetRx    uint64
	NetTx    uint64
	BlkRead  uint64
	BlkWrite uint64
	Pids     uint64
	Err      string
}

func handleStatsSnapshot(paramPatterns, sortBy string) (string, error) {
	sc, err := resolveScope(paramPatterns)
	if err != nil {
		return "", err
	}
	svcs := sc.Svcs
	ctx, cancel := ctxWithTimeout()
	defer cancel()

	rows := make([]statRow, 0, len(svcs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, s := range svcs {
		if s.State != "running" {
			mu.Lock()
			rows = append(rows, statRow{Svc: s.Service, Err: "not running (" + s.State + ")"})
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(s Svc) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := statRow{Svc: s.Service}
			res, err := dockerCli.ContainerStats(ctx, s.ID, client.ContainerStatsOptions{
				Stream:                false,
				IncludePreviousSample: true,
			})
			if err != nil {
				r.Err = err.Error()
			} else {
				defer res.Body.Close()
				var st container.StatsResponse
				if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
					r.Err = "decode stats: " + err.Error()
				} else {
					r.CPUPct = cpuPercent(st)
					r.MemUsed, r.MemLimit, r.MemPct = memUsage(st)
					for _, n := range st.Networks {
						r.NetRx += n.RxBytes
						r.NetTx += n.TxBytes
					}
					r.BlkRead, r.BlkWrite = blkIO(st)
					r.Pids = st.PidsStats.Current
				}
			}
			mu.Lock()
			rows = append(rows, r)
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	switch strings.ToLower(sortBy) {
	case "mem":
		sort.Slice(rows, func(i, j int) bool { return rows[i].MemUsed > rows[j].MemUsed })
	case "name":
		sort.Slice(rows, func(i, j int) bool { return rows[i].Svc < rows[j].Svc })
	default:
		sort.Slice(rows, func(i, j int) bool { return rows[i].CPUPct > rows[j].CPUPct })
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Stats snapshot - %s | %d service(s)\n%s\n\n", appCfg.SSHHost, len(rows), sc.ScopeLine())
	fmt.Fprintf(&b, "%-28s %7s %11s %11s %6s %10s %10s %6s  %s\n",
		"SERVICE", "CPU%", "MEM", "MEM-LIMIT", "MEM%", "NET-RX", "NET-TX", "PIDS", "NOTE")

	var totalCPU, totalMem float64
	for _, r := range rows {
		if r.Err != "" {
			fmt.Fprintf(&b, "%-28s %7s %11s %11s %6s %10s %10s %6s  %s\n",
				truncStr(r.Svc, 28), "-", "-", "-", "-", "-", "-", "-", r.Err)
			continue
		}
		totalCPU += r.CPUPct
		totalMem += float64(r.MemUsed)
		fmt.Fprintf(&b, "%-28s %7.2f %11s %11s %6.2f %10s %10s %6d  %s\n",
			truncStr(r.Svc, 28), r.CPUPct, humanBytes(int64(r.MemUsed)), humanBytes(int64(r.MemLimit)),
			r.MemPct, humanBytes(int64(r.NetRx)), humanBytes(int64(r.NetTx)), r.Pids, "")
	}
	fmt.Fprintf(&b, "\ntotal CPU %.2f%% across matched services | total memory %s\n", totalCPU, humanBytes(int64(totalMem)))

	if out, code, err := sshExec(15*time.Second, "free", "-b"); err == nil && code == 0 {
		if line := pickLine(out, "Mem:"); line != "" {
			f := strings.Fields(line)
			if len(f) >= 7 {
				tot, _ := strconv.ParseInt(f[1], 10, 64)
				used, _ := strconv.ParseInt(f[2], 10, 64)
				avail, _ := strconv.ParseInt(f[6], 10, 64)
				fmt.Fprintf(&b, "host memory: %s used of %s (%s available)\n",
					humanBytes(used), humanBytes(tot), humanBytes(avail))
			}
		}
	}
	return b.String(), nil
}

func cpuPercent(st container.StatsResponse) float64 {
	cpuDelta := float64(st.CPUStats.CPUUsage.TotalUsage) - float64(st.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(st.CPUStats.SystemUsage) - float64(st.PreCPUStats.SystemUsage)
	if sysDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	cpus := float64(st.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(st.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		cpus = 1
	}
	return cpuDelta / sysDelta * cpus * 100.0
}

// memUsage subtracts the page cache the way `docker stats` does, so the number
// matches what an operator sees on the host.
func memUsage(st container.StatsResponse) (used, limit uint64, pct float64) {
	used = st.MemoryStats.Usage
	if v, ok := st.MemoryStats.Stats["inactive_file"]; ok && v < used {
		used -= v
	} else if v, ok := st.MemoryStats.Stats["cache"]; ok && v < used {
		used -= v
	}
	limit = st.MemoryStats.Limit
	if limit > 0 {
		pct = float64(used) / float64(limit) * 100.0
	}
	return
}

func blkIO(st container.StatsResponse) (read, write uint64) {
	for _, e := range st.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			read += e.Value
		case "write":
			write += e.Value
		}
	}
	return
}

func pickLine(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return l
		}
	}
	return ""
}

// ---------- shared formatting / safety ----------

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	f := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for _, unit := range units {
		f /= u
		if f < u {
			return fmt.Sprintf("%.2f%s", f, unit)
		}
	}
	return fmt.Sprintf("%.2fPiB", f/u)
}

var secretRe = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|authorization|bearer|jwt|private[_-]?key)\b(\s*[:=]\s*|\s+)([^\s,;}"']+)`)

func redact(s string) string {
	return secretRe.ReplaceAllString(s, "$1$2***")
}

func truncate(s string) string {
	if len(s) <= appCfg.MaxOutputBytes {
		return s
	}
	half := appCfg.MaxOutputBytes / 2
	return s[:half] + fmt.Sprintf("\n\n... [%d bytes elided] ...\n\n", len(s)-appCfg.MaxOutputBytes) + s[len(s)-half:]
}
