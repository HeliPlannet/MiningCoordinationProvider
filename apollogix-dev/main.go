package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// MCP Protocol Types
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type ToolCallParam struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolCallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Config holds connection and scan settings
type Config struct {
	SSHHost        string
	Patterns       []string // matched against compose service name (fallback: container name); empty = scan all
	ProbePaths     map[string]string
	DefaultPath    string
	TimeoutSec     int
	ProbeTimeout   int
	EventBuffer    int
	DiskWarnPct    int
	DiskCritPct    int
	MaxOutputBytes int
}

var appCfg Config

var tools = []Tool{
	{
		Name: "health_overview",
		Description: "Health of the containers in scope. Scope is resolved in three layers, first one that matches wins: " +
			"(1) the patterns passed in this call, (2) APOLLOGIX_DEV_CONTAINER_PATTERNS from the .env next to the server, " +
			"(3) a hardcoded tms-*,fms-* fallback. A layer that is absent or matches no container is skipped and the next is tried. " +
			"Reports three independent signals per service - docker healthcheck, a host-side port probe, and runtime state/restarts - " +
			"plus a combined verdict that flags services with no healthcheck and services whose signals disagree.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"patterns": map[string]interface{}{
					"type":        "string",
					"description": "Comma-separated service-name globs, e.g. 'fms-*,postgres16'. Layer 1: used when at least one container matches, otherwise the configured default and then the tms-*,fms-* fallback are tried.",
				},
				"probe": map[string]interface{}{
					"type":        "boolean",
					"description": "Run the host-side port probe (default true). Set false for a docker-only, faster sweep.",
				},
			},
		},
	},
	{
		Name:        "health_probe",
		Description: "Probe one service from inside the dev host: TCP reachability on its published port plus an HTTP health request. Needed because published ports are firewalled from outside the host and 16 of 22 containers define no docker healthcheck.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"service": map[string]interface{}{"type": "string", "description": "Compose service name or container name."},
				"path":    map[string]interface{}{"type": "string", "description": "HTTP path to request (default: configured path for the service, else /actuator/health)."},
				"port":    map[string]interface{}{"type": "integer", "description": "Override the published port to probe."},
			},
			"required": []string{"service"},
		},
	},
	{
		Name:        "events_recent",
		Description: "Docker engine events recorded by this server since it started: container start/die/kill/oom/health_status and image/volume/network changes. Answers what happened N minutes ago, which a stateless docker call cannot.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"minutes": map[string]interface{}{"type": "integer", "description": "Look-back window in minutes (default 30)."},
				"service": map[string]interface{}{"type": "string", "description": "Limit to one service/container name."},
				"actions": map[string]interface{}{"type": "string", "description": "Comma-separated actions to keep, e.g. 'die,start,oom,health_status'. Default: lifecycle events only (exec_* noise excluded)."},
				"all":     map[string]interface{}{"type": "boolean", "description": "Include noisy exec_*/healthcheck-probe events (default false)."},
			},
		},
	},
	{
		Name:        "restart_history",
		Description: "Restart counts and uptime for matching services, combined with start/die events from the buffer, to expose crash loops and services that only recently stabilised.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"patterns": map[string]interface{}{"type": "string", "description": "Comma-separated service-name globs. Resolved through the same three layers as health_overview: call parameters, then configured default, then the tms-*,fms-* fallback."},
			},
		},
	},
	{
		Name:        "stats_snapshot",
		Description: "One-shot CPU, memory, network and block IO sample for matching services, collected in parallel.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"patterns": map[string]interface{}{"type": "string", "description": "Comma-separated service-name globs. Resolved through the same three layers as health_overview: call parameters, then configured default, then the tms-*,fms-* fallback."},
				"sort":     map[string]interface{}{"type": "string", "description": "Sort by 'cpu' (default), 'mem' or 'name'."},
			},
		},
	},
	{
		Name: "stats_disk",
		Description: "Disk status for the whole dev cluster: host filesystem usage, docker images/containers/volumes/build-cache breakdown, and per-volume detail. " +
			"Emits OK/WARNING/CRITICAL against the configured thresholds and suggests which volumes, images and build cache are safe to reclaim, marking data-bearing volumes as protected. Read-only: this server never deletes anything.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"min_volume_bytes": map[string]interface{}{"type": "integer", "description": "Hide volumes smaller than this in the detail table (default 0 = show all)."},
			},
		},
	},
}

func main() {
	envFileFlag := flag.String("env-file", "/Users/minhhuy/Repos/A/MCP/apollogix-dev/.env", "Path to .env file to load configuration from")
	flag.Parse()

	loadConfig(*envFileFlag)

	if err := initDocker(); err != nil {
		fmt.Fprintf(os.Stderr, "[apollogix-dev] Warning: initial docker connection failed: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[apollogix-dev] Connected to ssh://%s (API %s), patterns=%v\n",
			appCfg.SSHHost, dockerAPIVersion(), patternsLabel(appCfg.Patterns))
	}

	startEventRecorder()

	scanner := bufio.NewScanner(os.Stdin)
	const maxScanCapacity = 10 * 1024 * 1024
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxScanCapacity)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			sendError(nil, -32700, "Parse error: "+err.Error())
			continue
		}
		handleRequest(&req)
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "[apollogix-dev] Scanner error: %v\n", err)
	}
}

func loadConfig(envPath string) {
	envVars := make(map[string]string)
	if data, err := os.ReadFile(envPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				envVars[strings.TrimSpace(parts[0])] = strings.Trim(strings.TrimSpace(parts[1]), `"'`)
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "[apollogix-dev] Note: could not read env file %s: %v\n", envPath, err)
	}

	getVal := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		if v, ok := envVars[key]; ok && v != "" {
			return v
		}
		return def
	}
	getInt := func(key string, def int) int {
		if n, err := strconv.Atoi(getVal(key, "")); err == nil && n > 0 {
			return n
		}
		return def
	}

	appCfg.SSHHost = getVal("APOLLOGIX_DEV_SSH_HOST", "apollogix_dev")
	appCfg.Patterns = splitPatterns(getVal("APOLLOGIX_DEV_CONTAINER_PATTERNS", ""))
	appCfg.DefaultPath = getVal("APOLLOGIX_DEV_DEFAULT_PROBE_PATH", "/actuator/health")
	appCfg.ProbePaths = parseProbePaths(getVal("APOLLOGIX_DEV_PROBE_PATHS", ""))
	appCfg.TimeoutSec = getInt("APOLLOGIX_DEV_TIMEOUT_SEC", 30)
	appCfg.ProbeTimeout = getInt("APOLLOGIX_DEV_PROBE_TIMEOUT_SEC", 5)
	appCfg.EventBuffer = getInt("APOLLOGIX_DEV_EVENT_BUFFER", 2000)
	appCfg.DiskWarnPct = getInt("APOLLOGIX_DEV_DISK_WARN_PCT", 85)
	appCfg.DiskCritPct = getInt("APOLLOGIX_DEV_DISK_CRIT_PCT", 90)
	appCfg.MaxOutputBytes = getInt("APOLLOGIX_DEV_MAX_OUTPUT_BYTES", 262144)
}

func splitPatterns(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseProbePaths reads "svc=/path,svc2=/other" into a lookup used by the probe.
func parseProbePaths(raw string) map[string]string {
	m := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		kv := strings.SplitN(entry, "=", 2)
		if len(kv) == 2 {
			m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return m
}

func patternsLabel(p []string) string {
	if len(p) == 0 {
		return "<all>"
	}
	return strings.Join(p, ",")
}

func handleRequest(req *JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		sendResult(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "apollogix-dev", "version": "1.0.0"},
		})

	case "notifications/initialized":
		// No response required

	case "ping":
		sendResult(req.ID, map[string]interface{}{})

	case "tools/list":
		sendResult(req.ID, map[string]interface{}{"tools": tools})

	case "tools/call":
		var callParam ToolCallParam
		if err := json.Unmarshal(req.Params, &callParam); err != nil {
			sendError(req.ID, -32602, "Invalid params: "+err.Error())
			return
		}
		textResult, err := executeTool(callParam.Name, callParam.Arguments)
		if err != nil {
			sendResult(req.ID, ToolCallResult{
				Content: []ContentItem{{Type: "text", Text: "Error: " + err.Error()}},
				IsError: true,
			})
			return
		}
		sendResult(req.ID, ToolCallResult{
			Content: []ContentItem{{Type: "text", Text: truncate(redact(textResult))}},
		})

	default:
		sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func executeTool(name string, args map[string]interface{}) (string, error) {
	if err := ensureDocker(); err != nil {
		return "", err
	}

	argStr := func(k string) string {
		s, _ := args[k].(string)
		return strings.TrimSpace(s)
	}
	argInt := func(k string, def int) int {
		if f, ok := args[k].(float64); ok {
			return int(f)
		}
		if s := argStr(k); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				return n
			}
		}
		return def
	}
	argBool := func(k string, def bool) bool {
		if b, ok := args[k].(bool); ok {
			return b
		}
		return def
	}
	switch name {
	case "health_overview":
		return handleHealthOverview(argStr("patterns"), argBool("probe", true))
	case "health_probe":
		svc := argStr("service")
		if svc == "" {
			return "", fmt.Errorf("service is required")
		}
		return handleHealthProbe(svc, argStr("path"), argInt("port", 0))
	case "events_recent":
		return handleEventsRecent(argInt("minutes", 30), argStr("service"), argStr("actions"), argBool("all", false))
	case "restart_history":
		return handleRestartHistory(argStr("patterns"))
	case "stats_snapshot":
		return handleStatsSnapshot(argStr("patterns"), argStr("sort"))
	case "stats_disk":
		return handleStatsDisk(argInt("min_volume_bytes", 0))
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

func sendResult(id interface{}, result interface{}) {
	data, _ := json.Marshal(JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
	fmt.Fprintf(os.Stdout, "%s\n", data)
}

func sendError(id interface{}, code int, message string) {
	data, _ := json.Marshal(JSONRPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}})
	fmt.Fprintf(os.Stdout, "%s\n", data)
}
