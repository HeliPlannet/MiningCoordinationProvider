package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/connhelper"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

var (
	dockerCli        *client.Client
	serverAPIVersion string
	dockerMu         sync.Mutex
	controlPath      = fmt.Sprintf("/tmp/.mcp-apollogix-%%r@%%h:%%p")
)

// initDocker dials the remote engine through the ssh connection helper, which
// shells out to `ssh <host> docker system dial-stdio`. The private key stays
// with OpenSSH: ~/.ssh/config supplies host, port, user and IdentityFile.
func initDocker() error {
	dockerMu.Lock()
	defer dockerMu.Unlock()
	if dockerCli != nil {
		return nil
	}
	helper, err := connhelper.GetConnectionHelper("ssh://" + appCfg.SSHHost)
	if err != nil {
		return fmt.Errorf("ssh connection helper for %q: %w", appCfg.SSHHost, err)
	}
	cli, err := client.New(
		client.WithHTTPClient(&http.Client{Transport: &http.Transport{DialContext: helper.Dialer}}),
		client.WithHost(helper.Host),
		client.WithDialContext(helper.Dialer),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appCfg.TimeoutSec)*time.Second)
	defer cancel()
	ping, err := cli.Ping(ctx, client.PingOptions{})
	if err != nil {
		return fmt.Errorf("ping engine over ssh://%s: %w", appCfg.SSHHost, err)
	}
	// Ping does not itself settle version negotiation, so record the API
	// version the daemon reported rather than the client's own default.
	serverAPIVersion = ping.APIVersion
	dockerCli = cli
	return nil
}

func ensureDocker() error {
	if dockerCli != nil {
		return nil
	}
	return initDocker()
}

func dockerAPIVersion() string {
	if serverAPIVersion != "" {
		return serverAPIVersion
	}
	if dockerCli == nil {
		return "unknown"
	}
	return dockerCli.ClientVersion()
}

func ctxWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(appCfg.TimeoutSec)*time.Second)
}

// Svc is one container reduced to the fields the tools report on.
type Svc struct {
	Service    string // compose service label; falls back to container name
	Container  string
	Project    string
	Image      string
	State      string
	StatusText string
	Restarts   int
	StartedAt  time.Time
	OOMKilled  bool
	ExitCode   int
	HasHC      bool
	HCStatus   string
	HCStreak   int
	HCTest     string
	HCLastExit int
	HCLastOut  string
	Ports      []int // published host ports, ascending
	ID         string
}

// collectAll lists every container and inspects them in parallel over the
// single multiplexed ssh connection. Scope filtering happens afterwards, in
// memory, so the three-layer pattern resolution costs one round trip.
func collectAll() ([]Svc, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()

	list, err := dockerCli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]Svc, 0, len(list.Items))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 12)

	for _, c := range list.Items {
		cname := trimName(c.Names)
		service := c.Labels["com.docker.compose.service"]
		if service == "" {
			service = cname
		}
		wg.Add(1)
		go func(id, cname, service, project, image, status string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			s := Svc{ID: id, Container: cname, Service: service, Project: project, Image: image, StatusText: status}
			ir, err := dockerCli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
			if err == nil {
				st := ir.Container.State
				s.State = string(st.Status)
				s.Restarts = ir.Container.RestartCount
				s.OOMKilled = st.OOMKilled
				s.ExitCode = st.ExitCode
				if t, err := time.Parse(time.RFC3339Nano, st.StartedAt); err == nil {
					s.StartedAt = t
				}
				if st.Health != nil {
					s.HasHC = true
					s.HCStatus = string(st.Health.Status)
					s.HCStreak = st.Health.FailingStreak
					if n := len(st.Health.Log); n > 0 {
						s.HCLastExit = st.Health.Log[n-1].ExitCode
						s.HCLastOut = strings.TrimSpace(st.Health.Log[n-1].Output)
					}
				}
				if hc := ir.Container.Config.Healthcheck; hc != nil {
					s.HCTest = strings.Join(hc.Test, " ")
				}
				s.Ports = publishedPorts(ir.Container)
			}
			mu.Lock()
			out = append(out, s)
			mu.Unlock()
		}(c.ID, cname, service, c.Labels["com.docker.compose.project"], c.Image, c.Status)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// collectSvcs keeps the old single-scope call shape for tools that take one
// explicit pattern set.
func collectSvcs(patterns []string) ([]Svc, error) {
	all, err := collectAll()
	if err != nil {
		return nil, err
	}
	return filterSvcs(all, patterns), nil
}

func filterSvcs(all []Svc, patterns []string) []Svc {
	out := make([]Svc, 0, len(all))
	for _, s := range all {
		if matchAny(patterns, s.Service, s.Container) {
			out = append(out, s)
		}
	}
	return out
}

// hardcodedPatterns is the last-resort scope: the two application stacks on
// this host. Used only when neither the call parameters nor the configured
// patterns select any container.
var hardcodedPatterns = []string{"tms-*", "fms-*"}

// Scope is the outcome of resolving which services to act on.
type Scope struct {
	Patterns []string
	Layer    string   // "parameters", "config" or "fallback"
	Rejected []string // why the earlier layers were skipped
	Svcs     []Svc
}

// resolveScope applies the three layers in order - call parameters, then the
// configured default, then the hardcoded tms-*/fms-* fallback - and stops at
// the first layer that actually selects a container. A layer that is empty or
// matches nothing is treated as invalid and the next one is tried.
func resolveScope(paramRaw string) (Scope, error) {
	all, err := collectAll()
	if err != nil {
		return Scope{}, err
	}

	layers := []struct {
		name     string
		patterns []string
	}{
		{"parameters", splitPatterns(paramRaw)},
		{"config", appCfg.Patterns},
		{"fallback", hardcodedPatterns},
	}

	sc := Scope{}
	for _, l := range layers {
		if len(l.patterns) == 0 {
			if l.name != "fallback" {
				sc.Rejected = append(sc.Rejected, l.name+": not provided")
			}
			continue
		}
		matched := filterSvcs(all, l.patterns)
		if len(matched) > 0 {
			sc.Patterns, sc.Layer, sc.Svcs = l.patterns, l.name, matched
			return sc, nil
		}
		sc.Rejected = append(sc.Rejected,
			fmt.Sprintf("%s: %q matched no container", l.name, strings.Join(l.patterns, ",")))
	}

	// Every layer failed, including the fallback: report the empty scope with
	// the reasons rather than silently widening to all containers.
	sc.Patterns, sc.Layer, sc.Svcs = hardcodedPatterns, "fallback", nil
	return sc, nil
}

// ScopeLine renders the resolution for the tool output so the caller can see
// which layer won and what was discarded.
func (sc Scope) ScopeLine() string {
	var b strings.Builder
	fmt.Fprintf(&b, "scope: %s (from %s)", patternsLabel(sc.Patterns), sc.Layer)
	if len(sc.Rejected) > 0 {
		fmt.Fprintf(&b, " | skipped -> %s", strings.Join(sc.Rejected, "; "))
	}
	return b.String()
}

func trimName(names []string) string {
	if len(names) == 0 {
		return "<unnamed>"
	}
	return strings.TrimPrefix(names[0], "/")
}

func publishedPorts(ins container.InspectResponse) []int {
	seen := map[int]bool{}
	if ins.NetworkSettings != nil {
		for _, binds := range ins.NetworkSettings.Ports {
			for _, b := range binds {
				var p int
				fmt.Sscanf(b.HostPort, "%d", &p)
				if p > 0 {
					seen[p] = true
				}
			}
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}

// matchAny matches the glob patterns against the compose service name first,
// then the container name. Five services on this host carry generic labels
// (app, db, connect, elasticsearch, filebeat), so the container-name fallback
// keeps patterns like "fms-*" from silently missing them.
func matchAny(patterns []string, service, cname string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, service); ok {
			return true
		}
		if ok, _ := path.Match(p, cname); ok {
			return true
		}
		if strings.EqualFold(p, service) || strings.EqualFold(p, cname) {
			return true
		}
	}
	return false
}

func findSvc(name string) (Svc, error) {
	svcs, err := collectAll()
	if err != nil {
		return Svc{}, err
	}
	for _, s := range svcs {
		if strings.EqualFold(s.Service, name) || strings.EqualFold(s.Container, name) {
			return s, nil
		}
	}
	var near []string
	for _, s := range svcs {
		if strings.Contains(strings.ToLower(s.Service), strings.ToLower(name)) ||
			strings.Contains(strings.ToLower(s.Container), strings.ToLower(name)) {
			near = append(near, s.Service)
		}
	}
	if len(near) > 0 {
		return Svc{}, fmt.Errorf("no service named %q; did you mean: %s", name, strings.Join(near, ", "))
	}
	return Svc{}, fmt.Errorf("no service or container named %q on %s", name, appCfg.SSHHost)
}

// ---------- host-side SSH exec ----------

// shellQuote wraps a string in single quotes so the remote shell treats it as
// one literal word. ssh concatenates whatever argv it is given into a single
// command string and hands it to the remote shell, so passing separate argv
// entries does NOT preserve word boundaries - the quoting has to happen here.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshExec runs a command on the dev host. Every argument is single-quoted
// before the pieces are joined, so no value from a tool call can expand,
// redirect or chain on the remote side.
func sshExec(timeout time.Duration, argv ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}

	base := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath,
		"-o", "ControlPersist=300",
		appCfg.SSHHost, "--",
	}
	cmd := exec.CommandContext(ctx, "ssh", append(base, strings.Join(quoted, " "))...)
	cmd.Env = os.Environ()
	outBytes, err := cmd.CombinedOutput()
	out := string(outBytes)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		err = nil
	}
	if ctx.Err() != nil {
		return out, code, fmt.Errorf("ssh exec timed out after %s", timeout)
	}
	return out, code, err
}

// ---------- probe ----------

// Health paths only ever need unreserved characters, a query string and
// percent-escapes. Shell metacharacters are rejected here even though
// shellQuote would already neutralise them - two independent barriers.
var safePathRe = regexp.MustCompile(`^/[A-Za-z0-9._~:@/%&=?-]*$`)

type ProbeResult struct {
	Port     int
	TCPOpen  bool
	TCPErr   string
	HTTPCode string
	HTTPTime string
	HTTPPath string
	HTTPErr  string
	Skipped  string
}

func probeSvc(s Svc, pathOverride string, portOverride int) ProbeResult {
	var r ProbeResult

	port := portOverride
	if port == 0 {
		if len(s.Ports) == 0 {
			r.Skipped = "no published host port"
			return r
		}
		port = s.Ports[0]
	}
	r.Port = port

	timeout := time.Duration(appCfg.ProbeTimeout) * time.Second

	// TCP reachability, using bash's /dev/tcp so nothing extra is installed.
	out, code, err := sshExec(timeout+5*time.Second,
		"bash", "-c", fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", port))
	switch {
	case err != nil:
		r.TCPErr = err.Error()
	case code == 0:
		r.TCPOpen = true
	default:
		r.TCPErr = strings.TrimSpace(firstLine(out))
		if r.TCPErr == "" {
			r.TCPErr = fmt.Sprintf("exit %d", code)
		}
	}

	hpath := pathOverride
	if hpath == "" {
		hpath = appCfg.ProbePaths[s.Service]
	}
	if hpath == "" {
		hpath = appCfg.ProbePaths[s.Container]
	}
	if hpath == "" {
		hpath = appCfg.DefaultPath
	}
	if !strings.HasPrefix(hpath, "/") {
		hpath = "/" + hpath
	}
	if !safePathRe.MatchString(hpath) {
		r.HTTPErr = fmt.Sprintf("refusing unsafe path %q", hpath)
		return r
	}
	r.HTTPPath = hpath

	if !r.TCPOpen {
		r.HTTPErr = "skipped (port not reachable)"
		return r
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, hpath)
	out, code, err = sshExec(timeout+5*time.Second,
		"curl", "-sS", "-o", "/dev/null", "-w", "%{http_code} %{time_total}",
		"--max-time", fmt.Sprint(appCfg.ProbeTimeout), url)
	if err != nil {
		r.HTTPErr = err.Error()
		return r
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if code != 0 || len(fields) < 2 {
		r.HTTPErr = strings.TrimSpace(firstLine(out))
		if r.HTTPErr == "" {
			r.HTTPErr = fmt.Sprintf("curl exit %d", code)
		}
		return r
	}
	r.HTTPCode, r.HTTPTime = fields[0], fields[1]
	return r
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
