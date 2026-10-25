package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
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

// Config holds DB connection settings
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	Schema   string
	SSLMode  string
}

var (
	db     *sql.DB
	appCfg Config
)

var tools = []Tool{
	{
		Name:        "list_schemas",
		Description: "List all schemas in the PostgreSQL database.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
	{
		Name:        "list_tables",
		Description: "List all tables and views in a specified schema (defaults to the configured application schema).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"schema": map[string]interface{}{
					"type":        "string",
					"description": "Schema name to inspect. Defaults to the configured default schema (e.g. freight_forwarder_18).",
				},
			},
		},
	},
	{
		Name:        "describe_table",
		Description: "Get detailed column definitions and primary keys for a specific table.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"table": map[string]interface{}{
					"type":        "string",
					"description": "Table name to inspect.",
				},
				"schema": map[string]interface{}{
					"type":        "string",
					"description": "Schema containing the table. Defaults to the configured default schema.",
				},
			},
			"required": []string{"table"},
		},
	},
	{
		Name:        "read_query",
		Description: "Execute a read-only SQL query (SELECT, EXPLAIN, SHOW, WITH ... SELECT) and return results as JSON.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "The read-only SQL query to execute.",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of rows to return (default: 100, max: 500).",
				},
			},
			"required": []string{"query"},
		},
	},
	{
		Name:        "execute_query",
		Description: "Execute a write / DML / DDL SQL statement (INSERT, UPDATE, DELETE, etc.) and return affected rows count.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "The SQL statement to execute.",
				},
			},
			"required": []string{"query"},
		},
	},
}

func main() {
	envFileFlag := flag.String("env-file", "/Users/minhhuy/Repos/A/ff-api/localdev.env", "Path to .env file to load configuration from")
	flag.Parse()

	// Load config from env file and OS env
	loadConfig(*envFileFlag)

	// Connect to Database
	var err error
	db, err = connectWithTunnel(appCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[local-postgres] Warning: Initial DB connection failed: %v\n", err)
	} else {
		defer db.Close()
		fmt.Fprintf(os.Stderr, "[local-postgres] Connected successfully to %s:%d/%s (schema: %s)\n",
			appCfg.Host, appCfg.Port, appCfg.Database, appCfg.Schema)
	}

	// JSON-RPC stdio loop
	scanner := bufio.NewScanner(os.Stdin)
	// Buffer size for large payloads
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
		fmt.Fprintf(os.Stderr, "[local-postgres] Scanner error: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "[local-postgres] Note: could not read env file %s: %v\n", envPath, err)
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

	appCfg.Host = getVal("DB_HOST", "localhost")
	appCfg.Port = 5432
	appCfg.User = getVal("DB_USER", getVal("DATASOURCE_USERNAME", "postgres"))
	appCfg.Password = getVal("DB_PASSWORD", getVal("DATASOURCE_PASSWORD", ""))
	appCfg.Database = getVal("DB_NAME", "freight_forwarder")
	appCfg.Schema = getVal("DB_SCHEMA", getVal("DATASOURCE_SCHEMA", "freight_forwarder_18"))
	appCfg.SSLMode = getVal("DB_SSLMODE", "disable")

	// Parse DATASOURCE_URL if present, e.g., jdbc:postgresql://localhost:6432/freight_forwarder
	dsURL := getVal("DATASOURCE_URL", "")
	if dsURL != "" {
		parseDataSourceURL(dsURL, &appCfg)
	}

	loadTunnelConfig(getVal)

	// dev-RO-postgres always goes through the IPv4 tunnel, regardless of URL or DB_PORT
	// ("localhost" may resolve to ::1, which the tunnel check does not cover)
	appCfg.Host = "127.0.0.1"
	appCfg.Port = tunnelPort
}

func parseDataSourceURL(rawURL string, cfg *Config) {
	uStr := strings.TrimPrefix(rawURL, "jdbc:")
	u, err := url.Parse(uStr)
	if err != nil {
		return
	}
	if u.Hostname() != "" {
		cfg.Host = u.Hostname()
	}
	if u.Port() != "" {
		if p, err := strconv.Atoi(u.Port()); err == nil {
			cfg.Port = p
		}
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName != "" {
		cfg.Database = dbName
	}
	if u.User != nil {
		if u.User.Username() != "" {
			cfg.User = u.User.Username()
		}
		if pass, ok := u.User.Password(); ok {
			cfg.Password = pass
		}
	}
}

var (
	tunnelPort = 6432
	tunnelCmd  = []string{"ssh", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30", "-L", "6432:127.0.0.1:5432", "apollogix_dev", "-N"}
	tunnelAddr string
)

// loadTunnelConfig reads the tunnel's local port and launch command from the
// env file / OS env, falling back to the previous hardcoded defaults.
//
// TUNNEL_PORT: local port the tunnel listens on (must match the port baked
// into TUNNEL_CMD's "-L" forwarding spec).
// TUNNEL_CMD: full command line used to start the tunnel, space-separated
// (e.g. `ssh -o BatchMode=yes -L 6432:127.0.0.1:5432 apollogix_dev -N`).
func loadTunnelConfig(getVal func(key, def string) string) {
	if p := getVal("TUNNEL_PORT", ""); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			tunnelPort = v
		}
	}
	if raw := getVal("TUNNEL_CMD", ""); raw != "" {
		if parts := strings.Fields(raw); len(parts) > 0 {
			tunnelCmd = parts
		}
	}
	tunnelAddr = fmt.Sprintf("127.0.0.1:%d", tunnelPort)
}

// tunnelUp reports whether something accepts connections on the tunnel port.
func tunnelUp() bool {
	c, err := net.DialTimeout("tcp", tunnelAddr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ensureTunnel starts the configured tunnel command when nothing is
// listening on the tunnel port, then waits for it to come up.
func ensureTunnel() error {
	if tunnelUp() {
		fmt.Fprintf(os.Stderr, "[dev-RO-postgres] Port %d already listening, not starting tunnel\n", tunnelPort)
		return nil
	}

	cmd := exec.Command(tunnelCmd[0], tunnelCmd[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive MCP server restarts
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ssh tunnel: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	fmt.Fprintf(os.Stderr, "[dev-RO-postgres] Started tunnel on port %d (pid %d)\n", tunnelPort, cmd.Process.Pid)

	// Wait up to 15s for the forwarded port to accept connections
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("ssh tunnel exited early: %v", err)
		default:
		}
		if tunnelUp() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("ssh tunnel did not open port %d within 15s", tunnelPort)
}

// connectWithTunnel tries the DB; on failure, starts the tunnel if the port
// is not listening and retries once before giving up.
func connectWithTunnel(cfg Config) (*sql.DB, error) {
	conn, err := connectDB(cfg)
	if err == nil {
		return conn, nil
	}
	fmt.Fprintf(os.Stderr, "[dev-RO-postgres] Connect failed (%v), checking tunnel\n", err)
	if tErr := ensureTunnel(); tErr != nil {
		return nil, fmt.Errorf("%v; tunnel: %v", err, tErr)
	}
	conn, err2 := connectDB(cfg)
	if err2 != nil {
		return nil, fmt.Errorf("reconnect after tunnel check failed: %w", err2)
	}
	return conn, nil
}

func connectDB(cfg Config) (*sql.DB, error) {
	// Only the configured port: never fall back to a local DB for the dev read-only server
	portsToTry := []int{cfg.Port}

	var lastErr error
	for _, p := range portsToTry {
		connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=5 search_path=%s,public",
			cfg.Host, p, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode, cfg.Schema)

		testDB, err := sql.Open("postgres", connStr)
		if err != nil {
			lastErr = err
			continue
		}
		testDB.SetMaxOpenConns(5)
		testDB.SetMaxIdleConns(2)
		testDB.SetConnMaxLifetime(10 * time.Minute)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = testDB.PingContext(ctx)
		cancel()

		if err == nil {
			appCfg.Port = p
			return testDB, nil
		}
		testDB.Close()
		lastErr = err
	}

	return nil, lastErr
}

func ensureDB() error {
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := db.PingContext(ctx)
		cancel()
		if err == nil {
			return nil
		}
		db.Close()
		db = nil
	}
	conn, err := connectWithTunnel(appCfg)
	if err != nil {
		return fmt.Errorf("failed to connect to postgres: %w", err)
	}
	db = conn
	return nil
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
				"name":    "local-postgres",
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
	if err := ensureDB(); err != nil {
		return "", err
	}

	switch name {
	case "list_schemas":
		return handleListSchemas()
	case "list_tables":
		schema := appCfg.Schema
		if s, ok := args["schema"].(string); ok && strings.TrimSpace(s) != "" {
			schema = strings.TrimSpace(s)
		}
		return handleListTables(schema)
	case "describe_table":
		table, _ := args["table"].(string)
		if strings.TrimSpace(table) == "" {
			return "", fmt.Errorf("table argument is required")
		}
		schema := appCfg.Schema
		if s, ok := args["schema"].(string); ok && strings.TrimSpace(s) != "" {
			schema = strings.TrimSpace(s)
		}
		return handleDescribeTable(schema, table)
	case "read_query":
		q, _ := args["query"].(string)
		if strings.TrimSpace(q) == "" {
			return "", fmt.Errorf("query argument is required")
		}
		limit := 100
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		if limit > 500 {
			limit = 500
		}
		return handleReadQuery(q, limit)
	case "execute_query":
		q, _ := args["query"].(string)
		if strings.TrimSpace(q) == "" {
			return "", fmt.Errorf("query argument is required")
		}
		return handleExecuteQuery(q)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func handleListSchemas() (string, error) {
	rows, err := db.Query("SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('information_schema', 'pg_catalog', 'pg_toast') ORDER BY schema_name")
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			schemas = append(schemas, s)
		}
	}
	data, _ := json.MarshalIndent(map[string]interface{}{
		"default_schema": appCfg.Schema,
		"schemas":        schemas,
	}, "", "  ")
	return string(data), nil
}

func handleListTables(schema string) (string, error) {
	rows, err := db.Query(`
		SELECT table_name, table_type 
		FROM information_schema.tables 
		WHERE table_schema = $1 
		ORDER BY table_name`, schema)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type TableItem struct {
		Name string `json:"table_name"`
		Type string `json:"table_type"`
	}

	var tables []TableItem
	for rows.Next() {
		var item TableItem
		if err := rows.Scan(&item.Name, &item.Type); err == nil {
			tables = append(tables, item)
		}
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"schema": schema,
		"count":  len(tables),
		"tables": tables,
	}, "", "  ")
	return string(data), nil
}

func handleDescribeTable(schema, table string) (string, error) {
	// Query columns
	colRows, err := db.Query(`
		SELECT column_name, data_type, is_nullable, column_default, character_maximum_length
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return "", err
	}
	defer colRows.Close()

	type ColumnInfo struct {
		Name         string  `json:"column_name"`
		Type         string  `json:"data_type"`
		IsNullable   string  `json:"is_nullable"`
		Default      *string `json:"column_default"`
		MaxLength    *int    `json:"max_length,omitempty"`
		IsPrimaryKey bool    `json:"is_primary_key"`
	}

	var cols []ColumnInfo
	for colRows.Next() {
		var c ColumnInfo
		var def sql.NullString
		var maxLen sql.NullInt64
		if err := colRows.Scan(&c.Name, &c.Type, &c.IsNullable, &def, &maxLen); err == nil {
			if def.Valid {
				c.Default = &def.String
			}
			if maxLen.Valid {
				l := int(maxLen.Int64)
				c.MaxLength = &l
			}
			cols = append(cols, c)
		}
	}

	if len(cols) == 0 {
		return "", fmt.Errorf("table '%s.%s' not found or contains no columns", schema, table)
	}

	// Query primary keys
	pkRows, err := db.Query(`
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		  AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND tc.table_schema = $1
		  AND tc.table_name = $2`, schema, table)
	if err == nil {
		defer pkRows.Close()
		pkMap := make(map[string]bool)
		for pkRows.Next() {
			var pkCol string
			if err := pkRows.Scan(&pkCol); err == nil {
				pkMap[pkCol] = true
			}
		}
		for i := range cols {
			if pkMap[cols[i].Name] {
				cols[i].IsPrimaryKey = true
			}
		}
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"schema":  schema,
		"table":   table,
		"columns": cols,
	}, "", "  ")
	return string(data), nil
}

func handleReadQuery(q string, limit int) (string, error) {
	trimmed := strings.TrimSpace(strings.ToUpper(q))
	// Prevent destructive queries in read_query
	destructiveKeywords := []string{"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE", "ALTER", "CREATE", "GRANT", "REVOKE"}
	for _, kw := range destructiveKeywords {
		pattern := `\b` + kw + `\b`
		matched, _ := regexp.MatchString(pattern, trimmed)
		if matched {
			return "", fmt.Errorf("read_query does not permit mutation/DDL statement containing '%s'. Use execute_query instead", kw)
		}
	}

	// Check if query has LIMIT, if not, append or wrap with limit
	finalQuery := q
	if !strings.Contains(trimmed, "LIMIT") && !strings.HasPrefix(trimmed, "EXPLAIN") && !strings.HasPrefix(trimmed, "SHOW") {
		finalQuery = fmt.Sprintf("SELECT * FROM (%s) AS _subq LIMIT %d", strings.TrimSuffix(q, ";"), limit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, finalQuery)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}

	var results []map[string]interface{}
	for rows.Next() {
		values := make([]interface{}, len(cols))
		valuePtrs := make([]interface{}, len(cols))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return "", err
		}

		rowMap := make(map[string]interface{})
		for i, col := range cols {
			val := values[i]
			if b, ok := val.([]byte); ok {
				// Try parsing as JSON or convert to string
				var jsonVal interface{}
				if err := json.Unmarshal(b, &jsonVal); err == nil {
					rowMap[col] = jsonVal
				} else {
					rowMap[col] = string(b)
				}
			} else {
				rowMap[col] = val
			}
		}
		results = append(results, rowMap)
	}

	resMap := map[string]interface{}{
		"query":     q,
		"row_count": len(results),
		"rows":      results,
	}
	data, _ := json.MarshalIndent(resMap, "", "  ")
	return string(data), nil
}

func handleExecuteQuery(q string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := db.ExecContext(ctx, q)
	if err != nil {
		return "", err
	}

	rowsAffected, _ := res.RowsAffected()
	data, _ := json.MarshalIndent(map[string]interface{}{
		"status":        "success",
		"rows_affected": rowsAffected,
	}, "", "  ")
	return string(data), nil
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
