package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const tunnelKeyPrefix = "REDIS_TUNNEL_"

// PortMap is one forwarded port: 127.0.0.1:Local on this machine -> Remote on the SSH host.
type PortMap struct {
	Local  int `json:"local_port"`
	Remote int `json:"remote_port"`
}

type Host struct {
	Name    string // lower-cased suffix of REDIS_TUNNEL_<NAME>
	SSHHost string // last argument of the tunnel command
	Command []string
	Ports   []PortMap
	runDir  string
}

type Config struct {
	BaseDir string
	RunDir  string
	Hosts   map[string]*Host // by normalized key
	Order   []string         // normalized keys, sorted
}

// normalize makes "apollogix-stg", "apollogix_stg" and "APOLLOGIX STG" the same key.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func readEnvFile(path string) map[string]string {
	vars := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[redis-RO] Note: could not read env file %s: %v\n", path, err)
		return vars
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vars[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return vars
}

func loadConfig(envPath, baseDir string) (*Config, error) {
	env := readEnvFile(envPath)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, tunnelKeyPrefix) && v != "" {
			env[k] = v
		}
	}

	cfg := &Config{
		BaseDir: baseDir,
		RunDir:  filepath.Join(baseDir, ".run"),
		Hosts:   map[string]*Host{},
	}

	for k, v := range env {
		if !strings.HasPrefix(k, tunnelKeyPrefix) || v == "" {
			continue
		}
		key := normalize(strings.TrimPrefix(k, tunnelKeyPrefix))
		h, err := parseTunnel(strings.ToLower(key), v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		h.runDir = cfg.RunDir
		cfg.Hosts[key] = h
		cfg.Order = append(cfg.Order, key)
	}
	if len(cfg.Order) == 0 {
		return nil, fmt.Errorf("no %s<HOST> entries in %s", tunnelKeyPrefix, envPath)
	}
	sort.Strings(cfg.Order)

	seen := map[int]string{}
	for _, key := range cfg.Order {
		for _, p := range cfg.Hosts[key].Ports {
			if other, dup := seen[p.Local]; dup {
				fmt.Fprintf(os.Stderr, "[redis-RO] Warning: local port %d is used by both %s and %s; they cannot be connected at the same time\n", p.Local, other, cfg.Hosts[key].Name)
			}
			seen[p.Local] = cfg.Hosts[key].Name
		}
	}

	return cfg, nil
}

// parseTunnel takes the literal ssh command from .env; its -L flags are the port mappings.
func parseTunnel(name, command string) (*Host, error) {
	args := strings.Fields(command)
	if len(args) < 2 || filepath.Base(args[0]) != "ssh" {
		return nil, fmt.Errorf("tunnel command must start with ssh: %q", command)
	}
	h := &Host{Name: name, SSHHost: args[len(args)-1], Command: args}
	for i := 1; i < len(args); i++ {
		var spec string
		switch {
		case args[i] == "-L" && i+1 < len(args):
			spec = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-L") && len(args[i]) > 2:
			spec = args[i][2:]
		default:
			continue
		}
		p, err := parseForward(spec)
		if err != nil {
			return nil, err
		}
		h.Ports = append(h.Ports, p)
	}
	if len(h.Ports) == 0 {
		return nil, fmt.Errorf("tunnel command has no -L forward: %q", command)
	}
	return h, nil
}

// parseForward reads "[bind:]local:host:remote".
func parseForward(spec string) (PortMap, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 && len(parts) != 4 {
		return PortMap{}, fmt.Errorf("unsupported -L %q (expected [bind:]local:host:remote)", spec)
	}
	local, lerr := strconv.Atoi(parts[len(parts)-3])
	remote, rerr := strconv.Atoi(parts[len(parts)-1])
	if lerr != nil || rerr != nil || local < 1 || local > 65535 || remote < 1 || remote > 65535 {
		return PortMap{}, fmt.Errorf("invalid ports in -L %q", spec)
	}
	return PortMap{Local: local, Remote: remote}, nil
}

// find resolves the user-supplied host name; only hosts defined in .env are accepted.
func (c *Config) find(name string) (*Host, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("host is required. Known hosts: %s", c.hostNames())
	}
	if h, ok := c.Hosts[normalize(name)]; ok {
		return h, nil
	}
	return nil, fmt.Errorf("unknown host %q. Known hosts: %s (add %s<HOST> to .env)", name, c.hostNames(), tunnelKeyPrefix)
}

func (c *Config) hostNames() string {
	names := make([]string, 0, len(c.Order))
	for _, k := range c.Order {
		names = append(names, c.Hosts[k].Name)
	}
	return strings.Join(names, ", ")
}
