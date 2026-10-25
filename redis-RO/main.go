package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
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

const maxOutputBytes = 200 * 1024

var (
	cfg     *Config
	mgr     *Manager
	clients = map[int]*redis.Client{}
	clMu    sync.Mutex
)

func main() {
	exe, _ := os.Executable()
	baseDir := filepath.Dir(exe)
	envFile := flag.String("env-file", filepath.Join(baseDir, ".env"), "Path to .env file to load configuration from")
	flag.Parse()

	var err error
	cfg, err = loadConfig(*envFile, baseDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[redis-RO] fatal: %v\n", err)
		os.Exit(1)
	}
	mgr = &Manager{cfg: cfg}
	fmt.Fprintf(os.Stderr, "[redis-RO] ready, hosts: %s\n", cfg.hostNames())

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
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
		fmt.Fprintf(os.Stderr, "[redis-RO] Scanner error: %v\n", err)
	}
	// Tunnels are deliberately left running so CLI tools can keep using them.
}

func handleRequest(req *JSONRPCRequest) {
	switch {
	case req.Method == "initialize":
		sendResult(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "redis-RO", "version": "1.0.0"},
		})
	case strings.HasPrefix(req.Method, "notifications/"):
	case req.Method == "ping":
		sendResult(req.ID, map[string]interface{}{})
	case req.Method == "tools/list":
		sendResult(req.ID, map[string]interface{}{"tools": tools()})
	case req.Method == "tools/call":
		var call ToolCallParam
		if err := json.Unmarshal(req.Params, &call); err != nil {
			sendError(req.ID, -32602, "Invalid params: "+err.Error())
			return
		}
		text, err := executeTool(call.Name, call.Arguments)
		if err != nil {
			sendResult(req.ID, ToolCallResult{Content: []ContentItem{{Type: "text", Text: "Error: " + err.Error()}}, IsError: true})
			return
		}
		if len(text) > maxOutputBytes {
			text = text[:maxOutputBytes] + "\n...[truncated]"
		}
		sendResult(req.ID, ToolCallResult{Content: []ContentItem{{Type: "text", Text: text}}})
	default:
		if req.ID != nil {
			sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
		}
	}
}

func prop(typ, desc string) map[string]interface{} {
	return map[string]interface{}{"type": typ, "description": desc}
}

func schema(required []string, props map[string]interface{}) map[string]interface{} {
	s := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func tools() []Tool {
	hostDesc := "Host name from .env REDIS_TUNNEL_<HOST> (" + cfg.hostNames() + "). Case and -/_ are interchangeable."
	redisProps := func(extra map[string]interface{}) map[string]interface{} {
		p := map[string]interface{}{
			"host": prop("string", hostDesc+" The tunnel is opened automatically if it is down."),
			"port": prop("integer", "Remote Redis port on that host (default: first configured port)."),
		}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	key := map[string]interface{}{"key": prop("string", "The Redis key name.")}
	rng := map[string]interface{}{
		"key":   prop("string", "The Redis key name."),
		"start": prop("integer", "Start index (default 0)."),
		"stop":  prop("integer", "Stop index, inclusive (default start+99; at most 500 items per call)."),
	}
	return []Tool{
		{
			Name:        "tunnel_connect",
			Description: "Open (or reuse) the SSH tunnel to a host. The tunnel runs as a detached ssh process, so it stays up after this call and CLI tools can use the returned local ports (e.g. redis-cli -h 127.0.0.1 -p 16379).",
			InputSchema: schema([]string{"host"}, map[string]interface{}{"host": prop("string", hostDesc)}),
		},
		{
			Name:        "tunnel_disconnect",
			Description: "Stop the SSH tunnel this MCP started for a host. Use host=\"all\" to stop every tunnel. Tunnels started outside this MCP are never killed.",
			InputSchema: schema([]string{"host"}, map[string]interface{}{"host": prop("string", hostDesc+" Or \"all\".")}),
		},
		{
			Name:        "tunnel_status",
			Description: "Show configured hosts, tunnel state, port mappings (local -> remote) and ready-to-use redis-cli commands. With host, also returns the tail of that tunnel's ssh log (add -v to the command in .env for verbose output). Omit host to list all.",
			InputSchema: schema(nil, map[string]interface{}{"host": prop("string", hostDesc)}),
		},
		{
			Name:        "redis_keys",
			Description: "Find keys matching a pattern using SCAN (default '*', limit 100, max 500).",
			InputSchema: schema([]string{"host"}, redisProps(map[string]interface{}{
				"pattern": prop("string", "Pattern to match, e.g. 'user:*'. Default '*'."),
				"limit":   prop("integer", "Maximum keys to return (default 100, max 500)."),
			})),
		},
		{Name: "redis_get", Description: "Get the value of a string key.", InputSchema: schema([]string{"host", "key"}, redisProps(key))},
		{Name: "redis_hgetall", Description: "Get all fields and values of a hash key.", InputSchema: schema([]string{"host", "key"}, redisProps(key))},
		{Name: "redis_type", Description: "Get the data type of a key.", InputSchema: schema([]string{"host", "key"}, redisProps(key))},
		{Name: "redis_ttl", Description: "Get the remaining TTL of a key in seconds (-1 no expiry, -2 missing).", InputSchema: schema([]string{"host", "key"}, redisProps(key))},
		{Name: "redis_lrange", Description: "Read a range of a list key.", InputSchema: schema([]string{"host", "key"}, redisProps(rng))},
		{Name: "redis_zrange", Description: "Read a range of a sorted-set key with scores.", InputSchema: schema([]string{"host", "key"}, redisProps(rng))},
		{
			Name:        "redis_smembers",
			Description: "Read members of a set key using SSCAN (at most 500).",
			InputSchema: schema([]string{"host", "key"}, redisProps(map[string]interface{}{
				"key":   prop("string", "The Redis key name."),
				"limit": prop("integer", "Maximum members to return (default 100, max 500)."),
			})),
		},
		{Name: "redis_dbsize", Description: "Number of keys in the current database.", InputSchema: schema([]string{"host"}, redisProps(nil))},
		{
			Name:        "redis_info",
			Description: "Server information and statistics.",
			InputSchema: schema([]string{"host"}, redisProps(map[string]interface{}{
				"section": prop("string", "Optional INFO section (server, clients, memory, stats, keyspace). Default: all."),
			})),
		},
	}
}

func str(args map[string]interface{}, k string) string {
	s, _ := args[k].(string)
	return strings.TrimSpace(s)
}

func num(args map[string]interface{}, k string, def int64) int64 {
	if f, ok := args[k].(float64); ok {
		return int64(f)
	}
	return def
}

func toJSON(v interface{}) string {
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}

func executeTool(name string, args map[string]interface{}) (string, error) {
	switch name {
	case "tunnel_connect":
		h, err := cfg.find(str(args, "host"))
		if err != nil {
			return "", err
		}
		st, err := mgr.Connect(h)
		if err != nil {
			return "", err
		}
		return toJSON(withLog(withRedisPing(st), h)), nil

	case "tunnel_disconnect":
		if strings.EqualFold(str(args, "host"), "all") {
			var out []Status
			for _, k := range cfg.Order {
				out = append(out, mgr.Disconnect(cfg.Hosts[k]))
			}
			return toJSON(out), nil
		}
		h, err := cfg.find(str(args, "host"))
		if err != nil {
			return "", err
		}
		return toJSON(mgr.Disconnect(h)), nil

	case "tunnel_status":
		if str(args, "host") != "" {
			h, err := cfg.find(str(args, "host"))
			if err != nil {
				return "", err
			}
			return toJSON(withLog(withRedisPing(mgr.Status(h)), h)), nil
		}
		var out []Status
		for _, k := range cfg.Order {
			out = append(out, mgr.Status(cfg.Hosts[k]))
		}
		return toJSON(out), nil
	}

	return executeRedis(name, args)
}

// withLog attaches the ssh output (verbose when the tunnel command has -v) for troubleshooting.
func withLog(st Status, h *Host) Status {
	st.Log = h.logFile()
	st.LogTail = tailFile(h.logFile(), 4000)
	return st
}

// withRedisPing adds a per-port PING so a tunnel that is up but forwards to a dead remote port is visible.
func withRedisPing(st Status) Status {
	for i, p := range st.Ports {
		if !p.Listening {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		pong, err := clientFor(p.Local).Ping(ctx).Result()
		cancel()
		if err != nil {
			st.Ports[i].Redis = "unreachable: " + err.Error()
		} else {
			st.Ports[i].Redis = pong
		}
	}
	return st
}

func clientFor(local int) *redis.Client {
	clMu.Lock()
	defer clMu.Unlock()
	if c, ok := clients[local]; ok {
		return c
	}
	c := redis.NewClient(&redis.Options{
		Addr:         fmt.Sprintf("127.0.0.1:%d", local),
		DialTimeout:  3 * time.Second,
		ReadTimeout:  5 * time.Second,
		MaxRetries:   1,
		PoolSize:     2,
		MinIdleConns: 0,
	})
	clients[local] = c
	return c
}

func resolveTarget(args map[string]interface{}) (*redis.Client, error) {
	h, err := cfg.find(str(args, "host"))
	if err != nil {
		return nil, err
	}
	pm := h.Ports[0]
	if want := int(num(args, "port", 0)); want != 0 {
		found := false
		for _, p := range h.Ports {
			if p.Remote == want {
				pm, found = p, true
			}
		}
		if !found {
			return nil, fmt.Errorf("remote port %d is not configured for %s (configured: %v)", want, h.Name, h.Ports)
		}
	}
	if !listening(pm.Local) {
		if _, err := mgr.Connect(h); err != nil {
			return nil, fmt.Errorf("auto-connect to %s failed: %w", h.Name, err)
		}
	}
	return clientFor(pm.Local), nil
}

func capLimit(v, def int64) int64 {
	if v <= 0 {
		v = def
	}
	if v > 500 {
		v = 500
	}
	return v
}

// capStop bounds a range to 500 items; negative stops (-1 = to the end) are clamped too.
func capStop(start, stop int64) int64 {
	if stop < 0 || stop-start+1 > 500 {
		return start + 499
	}
	return stop
}

func executeRedis(name string, args map[string]interface{}) (string, error) {
	rdb, err := resolveTarget(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	needKey := func() (string, error) {
		k := str(args, "key")
		if k == "" {
			return "", fmt.Errorf("key argument is required")
		}
		return k, nil
	}

	switch name {
	case "redis_keys":
		pattern := str(args, "pattern")
		if pattern == "" {
			pattern = "*"
		}
		limit := capLimit(num(args, "limit", 100), 100)
		keys := []string{}
		iter := rdb.Scan(ctx, 0, pattern, 500).Iterator()
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
			if int64(len(keys)) >= limit {
				break
			}
		}
		if err := iter.Err(); err != nil {
			return "", err
		}
		return toJSON(map[string]interface{}{"pattern": pattern, "count": len(keys), "keys": keys}), nil

	case "redis_get":
		k, err := needKey()
		if err != nil {
			return "", err
		}
		val, err := rdb.Get(ctx, k).Result()
		if err == redis.Nil {
			return fmt.Sprintf("Key '%s' does not exist", k), nil
		} else if err != nil {
			return "", err
		}
		var parsed interface{}
		if json.Unmarshal([]byte(val), &parsed) == nil {
			return toJSON(map[string]interface{}{"key": k, "value": parsed}), nil
		}
		return toJSON(map[string]interface{}{"key": k, "value": val}), nil

	case "redis_hgetall":
		k, err := needKey()
		if err != nil {
			return "", err
		}
		fields, err := rdb.HGetAll(ctx, k).Result()
		if err != nil {
			return "", err
		}
		if len(fields) == 0 {
			return fmt.Sprintf("Hash key '%s' does not exist or is empty", k), nil
		}
		return toJSON(map[string]interface{}{"key": k, "count": len(fields), "fields": fields}), nil

	case "redis_type":
		k, err := needKey()
		if err != nil {
			return "", err
		}
		t, err := rdb.Type(ctx, k).Result()
		if err != nil {
			return "", err
		}
		return toJSON(map[string]interface{}{"key": k, "type": t}), nil

	case "redis_ttl":
		k, err := needKey()
		if err != nil {
			return "", err
		}
		ttl, err := rdb.TTL(ctx, k).Result()
		if err != nil {
			return "", err
		}
		desc := fmt.Sprintf("expires in %v", ttl)
		switch ttl {
		case -1 * time.Second:
			desc = "key has no expiration set"
		case -2 * time.Second:
			desc = "key does not exist"
		}
		return toJSON(map[string]interface{}{"key": k, "ttl_seconds": int(ttl.Seconds()), "description": desc}), nil

	case "redis_lrange":
		k, err := needKey()
		if err != nil {
			return "", err
		}
		start := num(args, "start", 0)
		stop := capStop(start, num(args, "stop", start+99))
		items, err := rdb.LRange(ctx, k, start, stop).Result()
		if err != nil {
			return "", err
		}
		return toJSON(map[string]interface{}{"key": k, "start": start, "count": len(items), "items": items}), nil

	case "redis_zrange":
		k, err := needKey()
		if err != nil {
			return "", err
		}
		start := num(args, "start", 0)
		stop := capStop(start, num(args, "stop", start+99))
		items, err := rdb.ZRangeWithScores(ctx, k, start, stop).Result()
		if err != nil {
			return "", err
		}
		out := make([]map[string]interface{}, 0, len(items))
		for _, z := range items {
			out = append(out, map[string]interface{}{"member": z.Member, "score": z.Score})
		}
		return toJSON(map[string]interface{}{"key": k, "start": start, "count": len(out), "items": out}), nil

	case "redis_smembers":
		k, err := needKey()
		if err != nil {
			return "", err
		}
		limit := capLimit(num(args, "limit", 100), 100)
		members := []string{}
		iter := rdb.SScan(ctx, k, 0, "*", 500).Iterator()
		for iter.Next(ctx) {
			members = append(members, iter.Val())
			if int64(len(members)) >= limit {
				break
			}
		}
		if err := iter.Err(); err != nil {
			return "", err
		}
		return toJSON(map[string]interface{}{"key": k, "count": len(members), "members": members}), nil

	case "redis_dbsize":
		n, err := rdb.DBSize(ctx).Result()
		if err != nil {
			return "", err
		}
		return toJSON(map[string]interface{}{"dbsize": n}), nil

	case "redis_info":
		if section := str(args, "section"); section != "" {
			return rdb.Info(ctx, section).Result()
		}
		return rdb.Info(ctx).Result()
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
