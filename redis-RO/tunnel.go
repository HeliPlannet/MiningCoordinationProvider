package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type PortStatus struct {
	Local     int    `json:"local_port"`
	Remote    int    `json:"remote_port"`
	Listening bool   `json:"listening"`
	Redis     string `json:"redis,omitempty"`
	CLI       string `json:"cli,omitempty"`
}

type Status struct {
	Host    string       `json:"host"`
	SSHHost string       `json:"ssh_host"`
	State   string       `json:"state"`
	PID     int          `json:"pid,omitempty"`
	Note    string       `json:"note,omitempty"`
	Ports   []PortStatus `json:"ports"`
	Log     string       `json:"log,omitempty"`
	LogTail string       `json:"log_tail,omitempty"`
}

// Manager keeps tunnels as detached ssh processes (own session, pid file) so they outlive this MCP
// process and other CLI tools can reuse the forwarded ports.
type Manager struct {
	cfg *Config
	mu  sync.Mutex
}

func (h *Host) pidFile() string { return filepath.Join(h.runDir, h.Name+".pid") }
func (h *Host) logFile() string { return filepath.Join(h.runDir, h.Name+".log") }

func listening(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func allListening(h *Host) bool {
	for _, p := range h.Ports {
		if !listening(p.Local) {
			return false
		}
	}
	return true
}

func readPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// isTunnelProcess guards against a recycled pid: it must still be an ssh to this host.
func isTunnelProcess(pid int, h *Host) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	cmdline := string(out)
	return err == nil && strings.Contains(cmdline, "ssh") && strings.Contains(cmdline, h.SSHHost)
}

func tailFile(path string, max int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

func (m *Manager) status(h *Host) Status {
	pid := readPID(h.pidFile())
	managed := isTunnelProcess(pid, h)
	st := Status{Host: h.Name, SSHHost: h.SSHHost}
	up := 0
	for _, p := range h.Ports {
		l := listening(p.Local)
		if l {
			up++
		}
		st.Ports = append(st.Ports, PortStatus{
			Local: p.Local, Remote: p.Remote, Listening: l,
			CLI: fmt.Sprintf("redis-cli -h 127.0.0.1 -p %d", p.Local),
		})
	}
	switch {
	case managed && up == len(h.Ports):
		st.State, st.PID = "connected", pid
	case managed:
		st.State, st.PID = "degraded", pid
		st.Note = "ssh process is alive but not every local port is listening"
	case up == len(h.Ports):
		st.State = "external"
		st.Note = "ports are served by a tunnel this MCP did not start (or whose pid file is gone); disconnect will not kill it"
	case up > 0:
		st.State = "conflict"
		st.Note = "some local ports are in use by another process"
	default:
		st.State = "disconnected"
	}
	return st
}

func (m *Manager) Status(h *Host) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status(h)
}

func (m *Manager) Connect(h *Host) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pid := readPID(h.pidFile())
	managed := isTunnelProcess(pid, h)
	if managed && allListening(h) {
		return m.status(h), nil
	}
	if managed {
		m.stop(h, pid)
	} else {
		os.Remove(h.pidFile())
	}

	for _, key := range m.cfg.Order {
		other := m.cfg.Hosts[key]
		if other == h || !isTunnelProcess(readPID(other.pidFile()), other) {
			continue
		}
		for _, p := range h.Ports {
			for _, op := range other.Ports {
				if p.Local == op.Local {
					return Status{}, fmt.Errorf("local port %d is already used by the live tunnel to %s; disconnect it first or change the port mapping", p.Local, other.Name)
				}
			}
		}
	}

	busy := 0
	for _, p := range h.Ports {
		if listening(p.Local) {
			busy++
		}
	}
	if busy == len(h.Ports) {
		return m.status(h), nil
	}
	if busy > 0 {
		return Status{}, fmt.Errorf("some local ports of %s are in use by another process; free them or change the mapping", h.Name)
	}

	if err := os.MkdirAll(h.runDir, 0o755); err != nil {
		return Status{}, err
	}
	logf, err := os.OpenFile(h.logFile(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Status{}, err
	}
	args := h.Command
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return Status{}, fmt.Errorf("start %s: %w", args[0], err)
	}
	logf.Close()
	os.WriteFile(h.pidFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(15 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
			os.Remove(h.pidFile())
			return Status{}, fmt.Errorf("ssh exited before the tunnel came up: %s", tailFile(h.logFile(), 600))
		case <-deadline:
			m.stop(h, cmd.Process.Pid)
			return Status{}, fmt.Errorf("tunnel to %s not ready after 15s: %s", h.Name, tailFile(h.logFile(), 600))
		case <-tick.C:
			if allListening(h) {
				st := m.status(h)
				st.Log = h.logFile()
				return st, nil
			}
		}
	}
}

func (m *Manager) stop(h *Host, pid int) {
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 15 && isTunnelProcess(pid, h); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if isTunnelProcess(pid, h) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	os.Remove(h.pidFile())
}

func (m *Manager) Disconnect(h *Host) Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	pid := readPID(h.pidFile())
	if isTunnelProcess(pid, h) {
		m.stop(h, pid)
		st := m.status(h)
		st.Note = fmt.Sprintf("stopped ssh pid %d", pid)
		return st
	}
	os.Remove(h.pidFile())
	st := m.status(h)
	if st.State == "disconnected" {
		st.Note = "no tunnel was running"
	}
	return st
}
