package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// Recorded engine event, kept in a bounded in-memory ring so the tools can
// answer "what happened N minutes ago" - the engine's own event endpoint only
// replays history when asked with an explicit since, and MCP calls are
// stateless otherwise.
type EventRec struct {
	At      time.Time
	Type    string
	Action  string
	Name    string
	Service string
	Extra   string
}

var (
	evMu      sync.RWMutex
	evRing    []EventRec
	evStart   time.Time
	evDropped int
	evStatus  string
)

// Lifecycle actions kept by default; exec_* probes dominate the stream
// (healthchecks run every 30s on 6 containers) and are filtered out.
var lifecycleActions = map[string]bool{
	"start": true, "die": true, "stop": true, "kill": true, "restart": true,
	"oom": true, "health_status": true, "create": true, "destroy": true,
	"pause": true, "unpause": true, "rename": true, "update": true,
}

func startEventRecorder() {
	evStart = time.Now()
	evStatus = "starting"
	go func() {
		backoff := 2 * time.Second
		for {
			if err := ensureDocker(); err != nil {
				setEvStatus("disconnected: " + err.Error())
				time.Sleep(backoff)
				backoff = capDur(backoff*2, 60*time.Second)
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			res := dockerCli.Events(ctx, client.EventsListOptions{})
			setEvStatus("streaming")
			backoff = 2 * time.Second

			streaming := true
			for streaming {
				select {
				case m, ok := <-res.Messages:
					if !ok {
						streaming = false
						break
					}
					at := time.Unix(0, m.TimeNano)
					if m.TimeNano == 0 {
						at = time.Unix(m.Time, 0)
					}
					attrs := m.Actor.Attributes
					name := attrs["name"]
					if name == "" {
						name = shortID(m.Actor.ID)
					}
					var extras []string
					for _, k := range []string{"exitCode", "signal", "health_status", "image"} {
						if v := attrs[k]; v != "" {
							extras = append(extras, k+"="+v)
						}
					}
					push(EventRec{
						At:      at,
						Type:    string(m.Type),
						Action:  string(m.Action),
						Name:    name,
						Service: attrs["com.docker.compose.service"],
						Extra:   strings.Join(extras, " "),
					})
				case err := <-res.Err:
					if err != nil {
						setEvStatus("stream error: " + err.Error())
						fmt.Fprintf(os.Stderr, "[apollogix-dev] event stream ended: %v\n", err)
					}
					streaming = false
				}
			}
			cancel()
			setEvStatus("reconnecting")
			time.Sleep(backoff)
			backoff = capDur(backoff*2, 60*time.Second)
		}
	}()
}

func capDur(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	return d
}

func setEvStatus(s string) {
	evMu.Lock()
	evStatus = s
	evMu.Unlock()
}

func push(r EventRec) {
	evMu.Lock()
	defer evMu.Unlock()
	evRing = append(evRing, r)
	if len(evRing) > appCfg.EventBuffer {
		over := len(evRing) - appCfg.EventBuffer
		evRing = evRing[over:]
		evDropped += over
	}
}

func snapshotEvents() ([]EventRec, string, int, time.Time) {
	evMu.RLock()
	defer evMu.RUnlock()
	cp := make([]EventRec, len(evRing))
	copy(cp, evRing)
	return cp, evStatus, evDropped, evStart
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ---------- events_recent ----------

func handleEventsRecent(minutes int, service, actionsCSV string, all bool) (string, error) {
	if minutes <= 0 {
		minutes = 30
	}
	events, status, dropped, started := snapshotEvents()
	cutoff := time.Now().Add(-time.Duration(minutes) * time.Minute)

	keep := map[string]bool{}
	for _, a := range strings.Split(actionsCSV, ",") {
		if a = strings.TrimSpace(a); a != "" {
			keep[a] = true
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Engine events on %s - last %d minutes\n", appCfg.SSHHost, minutes)
	fmt.Fprintf(&b, "recorder: %s | recording since %s (%s ago) | buffered %d | dropped %d\n",
		status, started.Format("2006-01-02 15:04:05"), time.Since(started).Round(time.Second), len(events), dropped)
	if started.After(cutoff) {
		fmt.Fprintf(&b, "NOTE: the recorder started inside the requested window, so events before %s are not known to this server.\n",
			started.Format("15:04:05"))
	}
	b.WriteString("\n")

	var rows []EventRec
	for _, e := range events {
		if e.At.Before(cutoff) {
			continue
		}
		if service != "" && !strings.EqualFold(e.Name, service) && !strings.EqualFold(e.Service, service) {
			continue
		}
		base := strings.SplitN(e.Action, ":", 2)[0]
		if len(keep) > 0 {
			if !keep[base] && !keep[e.Action] {
				continue
			}
		} else if !all && !lifecycleActions[base] {
			continue
		}
		rows = append(rows, e)
	}

	if len(rows) == 0 {
		filter := "lifecycle events"
		if all {
			filter = "all events"
		} else if len(keep) > 0 {
			filter = "actions " + actionsCSV
		}
		fmt.Fprintf(&b, "No %s in the window.\n", filter)
		if !all && len(keep) == 0 {
			b.WriteString("Healthcheck probe traffic (exec_*) is excluded by default; pass all=true to include it.\n")
		}
		return b.String(), nil
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].At.Before(rows[j].At) })
	fmt.Fprintf(&b, "%-19s  %-10s  %-16s  %-30s  %s\n", "TIME", "TYPE", "ACTION", "NAME", "DETAIL")
	for _, e := range rows {
		action := e.Action
		if len(action) > 16 {
			action = action[:15] + "~"
		}
		fmt.Fprintf(&b, "%-19s  %-10s  %-16s  %-30s  %s\n",
			e.At.Format("2006-01-02 15:04:05"), e.Type, action, truncStr(e.Name, 30), e.Extra)
	}
	fmt.Fprintf(&b, "\n%d event(s).\n", len(rows))
	return b.String(), nil
}

// ---------- restart_history ----------

func handleRestartHistory(paramPatterns string) (string, error) {
	sc, err := resolveScope(paramPatterns)
	if err != nil {
		return "", err
	}
	svcs := sc.Svcs
	events, status, _, started := snapshotEvents()

	// Count start/die per container name within the recorded window.
	type counts struct {
		starts, dies, ooms int
		lastDie, lastStart time.Time
	}
	per := map[string]*counts{}
	for _, e := range events {
		base := strings.SplitN(e.Action, ":", 2)[0]
		c := per[e.Name]
		if c == nil {
			c = &counts{}
			per[e.Name] = c
		}
		switch base {
		case "start":
			c.starts++
			c.lastStart = e.At
		case "die":
			c.dies++
			c.lastDie = e.At
		case "oom":
			c.ooms++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Restart history on %s\n%s\n", appCfg.SSHHost, sc.ScopeLine())
	fmt.Fprintf(&b, "recorder: %s since %s (%s ago); start/die columns cover only that window\n\n",
		status, started.Format("2006-01-02 15:04:05"), time.Since(started).Round(time.Second))

	sort.Slice(svcs, func(i, j int) bool {
		if svcs[i].Restarts != svcs[j].Restarts {
			return svcs[i].Restarts > svcs[j].Restarts
		}
		return svcs[i].Service < svcs[j].Service
	})

	fmt.Fprintf(&b, "%-28s %-10s %9s %9s %5s %5s  %s\n",
		"SERVICE", "STATE", "RESTARTS", "UPTIME", "START", "DIE", "FLAGS")
	var flagged []string
	for _, s := range svcs {
		uptime := "-"
		if !s.StartedAt.IsZero() && s.State == "running" {
			uptime = compactDur(time.Since(s.StartedAt))
		}
		c := per[s.Container]
		starts, dies := 0, 0
		if c != nil {
			starts, dies = c.starts, c.dies
		}
		var flags []string
		if s.Restarts > 20 {
			flags = append(flags, "HIGH-RESTART-COUNT")
		}
		if s.OOMKilled {
			flags = append(flags, "OOM-KILLED")
		}
		if dies > 0 {
			flags = append(flags, fmt.Sprintf("DIED-%dx-IN-WINDOW", dies))
		}
		if s.State == "running" && !s.StartedAt.IsZero() && time.Since(s.StartedAt) < 10*time.Minute && s.Restarts > 0 {
			flags = append(flags, "RECENTLY-RESTARTED")
		}
		if len(flags) > 0 {
			flagged = append(flagged, s.Service)
		}
		fmt.Fprintf(&b, "%-28s %-10s %9d %9s %5d %5d  %s\n",
			truncStr(s.Service, 28), s.State, s.Restarts, uptime, starts, dies, strings.Join(flags, ","))
	}

	b.WriteString("\n")
	if len(flagged) == 0 {
		b.WriteString("No crash-loop or OOM indicators among the matched services.\n")
	} else {
		fmt.Fprintf(&b, "Flagged: %s\n", strings.Join(flagged, ", "))
		b.WriteString("A high RESTARTS value with a long UPTIME means the container crash-looped earlier and has since stabilised;\n")
		b.WriteString("RESTARTS is cumulative since the container was created and is never reset by docker.\n")
	}
	return b.String(), nil
}

func compactDur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "~"
}
