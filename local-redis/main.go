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
	"strconv"
	"strings"
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

// Config holds Redis connection settings
type Config struct {
	Host     string
	Port     int
	Password string
	DB       int
}

var (
	rdb    *redis.Client
	appCfg Config
)

var tools = []Tool{
	{
		Name:        "redis_keys",
		Description: "Find keys matching a pattern using safe SCAN cursor (default: '*', limit: 100).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"description": "Pattern to match (e.g. '*', 'Department:*', 'user:*'). Default: '*'",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of keys to return (default: 100, max: 500).",
				},
			},
		},
	},
	{
		Name:        "redis_get",
		Description: "Get the value of a key in Redis.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"key": map[string]interface{}{
					"type":        "string",
					"description": "The Redis key name.",
				},
			},
			"required": []string{"key"},
		},
	},
	{
		Name:        "redis_set",
		Description: "Set the string value of a key with optional TTL (seconds).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"key": map[string]interface{}{
					"type":        "string",
					"description": "The Redis key name.",
				},
				"value": map[string]interface{}{
					"type":        "string",
					"description": "The value to set.",
				},
				"ttl_seconds": map[string]interface{}{
					"type":        "integer",
					"description": "Optional Time-to-Live in seconds (0 = never expire).",
				},
			},
			"required": []string{"key", "value"},
		},
	},
	{
		Name:        "redis_del",
		Description: "Delete one or more keys from Redis.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"keys": map[string]interface{}{
					"type": "array",
					"items": map[string]interface{}{
						"type": "string",
					},
					"description": "List of key names to delete.",
				},
				"key": map[string]interface{}{
					"type":        "string",
					"description": "Single key name to delete (alternative to keys array).",
				},
			},
		},
	},
	{
		Name:        "redis_hgetall",
		Description: "Get all fields and values of a Redis Hash key.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"key": map[string]interface{}{
					"type":        "string",
					"description": "The Hash key name.",
				},
			},
			"required": []string{"key"},
		},
	},
	{
		Name:        "redis_type",
		Description: "Get the data type of a key (string, hash, list, set, zset, none).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"key": map[string]interface{}{
					"type":        "string",
					"description": "The Redis key name.",
				},
			},
			"required": []string{"key"},
		},
	},
	{
		Name:        "redis_ttl",
		Description: "Get the remaining Time-To-Live (TTL) of a key in seconds (-1: no expiry, -2: key does not exist).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"key": map[string]interface{}{
					"type":        "string",
					"description": "The Redis key name.",
				},
			},
			"required": []string{"key"},
		},
	},
	{
		Name:        "redis_info",
		Description: "Get server information and statistics from Redis.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"section": map[string]interface{}{
					"type":        "string",
					"description": "Optional info section (e.g. server, clients, memory, stats, keyspace). Default: all.",
				},
			},
		},
	},
}

func main() {
	envFileFlag := flag.String("env-file", "/Users/minhhuy/Repos/A/ff-api/localdev.env", "Path to .env file to load configuration from")
	flag.Parse()

	// Load configuration
	loadConfig(*envFileFlag)

	// Connect to Redis
	rdb = connectRedis(appCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	pong, err := rdb.Ping(ctx).Result()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[local-redis] Warning: Redis ping failed: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[local-redis] Connected successfully to %s:%d (ping: %s)\n", appCfg.Host, appCfg.Port, pong)
	}
	defer rdb.Close()

	// JSON-RPC stdio loop
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
		fmt.Fprintf(os.Stderr, "[local-redis] Scanner error: %v\n", err)
	}
}

func loadConfig(envPath string) {
	envVars := make(map[string]string)
	if data, err := os.ReadFile(envPath); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				k := strings.TrimSpace(parts[0])
				v := strings.TrimSpace(parts[1])
				v = strings.Trim(v, `"'`)
				envVars[k] = v
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "[local-redis] Note: could not read env file %s: %v\n", envPath, err)
	}

	getVal := func(key string, def string) string {
		if v, exists := os.LookupEnv(key); exists && v != "" {
			return v
		}
		if v, exists := envVars[key]; exists && v != "" {
			return v
		}
		return def
	}

	appCfg.Host = getVal("REDIS_HOST", getVal("SPRING_REDIS_HOST", "localhost"))
	portStr := getVal("REDIS_PORT", getVal("SPRING_REDIS_PORT", "6379"))
	appCfg.Port = 6379
	if p, err := strconv.Atoi(portStr); err == nil {
		appCfg.Port = p
	}
	appCfg.Password = getVal("REDIS_PASSWORD", getVal("SPRING_REDIS_PASSWORD", ""))
	dbStr := getVal("REDIS_DB", "0")
	if d, err := strconv.Atoi(dbStr); err == nil {
		appCfg.DB = d
	}
}

func connectRedis(cfg Config) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Password: cfg.Password,
		DB:       cfg.DB,
	})
}

func handleRequest(req *JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		result := map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "local-redis",
				"version": "1.0.0",
			},
		}
		sendResult(req.ID, result)

	case "notifications/initialized":
		// No response required

	case "ping":
		sendResult(req.ID, map[string]interface{}{})

	case "tools/list":
		sendResult(req.ID, map[string]interface{}{
			"tools": tools,
		})

	case "tools/call":
		var callParam ToolCallParam
		if err := json.Unmarshal(req.Params, &callParam); err != nil {
			sendError(req.ID, -32602, "Invalid params: "+err.Error())
			return
		}

		textResult, err := executeTool(callParam.Name, callParam.Arguments)
		if err != nil {
			sendResult(req.ID, ToolCallResult{
				Content: []ContentItem{
					{Type: "text", Text: "Error: " + err.Error()},
				},
				IsError: true,
			})
			return
		}

		sendResult(req.ID, ToolCallResult{
			Content: []ContentItem{
				{Type: "text", Text: textResult},
			},
			IsError: false,
		})

	default:
		sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func executeTool(name string, args map[string]interface{}) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch name {
	case "redis_keys":
		pattern := "*"
		if p, ok := args["pattern"].(string); ok && strings.TrimSpace(p) != "" {
			pattern = strings.TrimSpace(p)
		}
		limit := int64(100)
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int64(l)
		}
		if limit > 500 {
			limit = 500
		}

		var keys []string
		iter := rdb.Scan(ctx, 0, pattern, limit).Iterator()
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
			if int64(len(keys)) >= limit {
				break
			}
		}
		if err := iter.Err(); err != nil {
			return "", err
		}

		data, _ := json.MarshalIndent(map[string]interface{}{
			"pattern": pattern,
			"count":   len(keys),
			"keys":    keys,
		}, "", "  ")
		return string(data), nil

	case "redis_get":
		k, _ := args["key"].(string)
		if strings.TrimSpace(k) == "" {
			return "", fmt.Errorf("key argument is required")
		}
		val, err := rdb.Get(ctx, k).Result()
		if err == redis.Nil {
			return fmt.Sprintf("Key '%s' does not exist", k), nil
		} else if err != nil {
			return "", err
		}

		// Try to format as JSON if valid JSON
		var jsonObj interface{}
		if err := json.Unmarshal([]byte(val), &jsonObj); err == nil {
			formatted, _ := json.MarshalIndent(map[string]interface{}{
				"key":   k,
				"value": jsonObj,
			}, "", "  ")
			return string(formatted), nil
		}

		data, _ := json.MarshalIndent(map[string]interface{}{
			"key":   k,
			"value": val,
		}, "", "  ")
		return string(data), nil

	case "redis_set":
		k, _ := args["key"].(string)
		if strings.TrimSpace(k) == "" {
			return "", fmt.Errorf("key argument is required")
		}
		v, _ := args["value"].(string)
		ttl := time.Duration(0)
		if ttlSec, ok := args["ttl_seconds"].(float64); ok && ttlSec > 0 {
			ttl = time.Duration(ttlSec) * time.Second
		}
		err := rdb.Set(ctx, k, v, ttl).Err()
		if err != nil {
			return "", err
		}
		data, _ := json.MarshalIndent(map[string]interface{}{
			"status": "success",
			"key":    k,
			"ttl":    ttl.String(),
		}, "", "  ")
		return string(data), nil

	case "redis_del":
		var keysToDelete []string
		if singleKey, ok := args["key"].(string); ok && strings.TrimSpace(singleKey) != "" {
			keysToDelete = append(keysToDelete, strings.TrimSpace(singleKey))
		}
		if listKeys, ok := args["keys"].([]interface{}); ok {
			for _, item := range listKeys {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					keysToDelete = append(keysToDelete, strings.TrimSpace(s))
				}
			}
		}
		if len(keysToDelete) == 0 {
			return "", fmt.Errorf("at least one key must be specified via 'key' or 'keys'")
		}
		deletedCount, err := rdb.Del(ctx, keysToDelete...).Result()
		if err != nil {
			return "", err
		}
		data, _ := json.MarshalIndent(map[string]interface{}{
			"status":        "success",
			"deleted_count": deletedCount,
			"keys":          keysToDelete,
		}, "", "  ")
		return string(data), nil

	case "redis_hgetall":
		k, _ := args["key"].(string)
		if strings.TrimSpace(k) == "" {
			return "", fmt.Errorf("key argument is required")
		}
		fields, err := rdb.HGetAll(ctx, k).Result()
		if err != nil {
			return "", err
		}
		if len(fields) == 0 {
			return fmt.Sprintf("Hash key '%s' does not exist or is empty", k), nil
		}
		data, _ := json.MarshalIndent(map[string]interface{}{
			"key":    k,
			"count":  len(fields),
			"fields": fields,
		}, "", "  ")
		return string(data), nil

	case "redis_type":
		k, _ := args["key"].(string)
		if strings.TrimSpace(k) == "" {
			return "", fmt.Errorf("key argument is required")
		}
		t, err := rdb.Type(ctx, k).Result()
		if err != nil {
			return "", err
		}
		data, _ := json.MarshalIndent(map[string]interface{}{
			"key":  k,
			"type": t,
		}, "", "  ")
		return string(data), nil

	case "redis_ttl":
		k, _ := args["key"].(string)
		if strings.TrimSpace(k) == "" {
			return "", fmt.Errorf("key argument is required")
		}
		ttl, err := rdb.TTL(ctx, k).Result()
		if err != nil {
			return "", err
		}
		var desc string
		if ttl == -1*time.Second {
			desc = "key has no expiration set"
		} else if ttl == -2*time.Second {
			desc = "key does not exist"
		} else {
			desc = fmt.Sprintf("expires in %v", ttl)
		}
		data, _ := json.MarshalIndent(map[string]interface{}{
			"key":         k,
			"ttl_seconds": int(ttl.Seconds()),
			"description": desc,
		}, "", "  ")
		return string(data), nil

	case "redis_info":
		section := ""
		if s, ok := args["section"].(string); ok {
			section = strings.TrimSpace(s)
		}
		var info string
		var err error
		if section != "" {
			info, err = rdb.Info(ctx, section).Result()
		} else {
			info, err = rdb.Info(ctx).Result()
		}
		if err != nil {
			return "", err
		}
		return info, nil

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func sendResult(id interface{}, result interface{}) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintf(os.Stdout, "%s\n", data)
}

func sendError(id interface{}, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintf(os.Stdout, "%s\n", data)
}
