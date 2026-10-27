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
	"sync"
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

// Config holds the DEFAULT DB connection settings. Any tool call may override
// the target database / schema, DataGrip-style: one server reaches every
// database the credentials are granted on.
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
	appCfg Config

	// pools holds one *sql.DB per (database, search_path) pair, created lazily.
	pools   = make(map[string]*sql.DB)
	poolsMu sync.Mutex
)

// identRe guards database / schema names before they are interpolated into a
// libpq keyword=value connection string. Anything with a space, quote,
// backslash or '=' could inject extra connection parameters.
var identRe = regexp.MustCompile(`^[A-Za-z0-9_$.\-]{1,63}$`)

const dbParamDesc = "Target database on the same PostgreSQL server (e.g. merchant, common, freight_forwarder). Defaults to the configured database. Use list_databases to discover."

var tools = []Tool{
	{
		Name:        "list_databases",
		Description: "List every database on the PostgreSQL server that the credentials can connect to, with size and the configured default flagged.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
	{
		Name:        "list_schemas",
		Description: "List all schemas in a database (defaults to the configured database).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"database": map[string]interface{}{
					"type":        "string",
					"description": dbParamDesc,
				},
			},
		},
	},
	{
		Name:        "list_tables",
		Description: "List all tables and views in a schema of a database.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"database": map[string]interface{}{
					"type":        "string",
					"description": dbParamDesc,
				},
				"schema": map[string]interface{}{
					"type":        "string",
					"description": "Schema name to inspect. Defaults to the configured default schema for the default database, otherwise 'public'.",
				},
			},
		},
	},
	{
		Name:        "describe_table",
		Description: "Get column definitions and primary keys for a specific table.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"table": map[string]interface{}{
					"type":        "string",
					"description": "Table name to inspect.",
				},
				"database": map[string]interface{}{
					"type":        "string",
					"description": dbParamDesc,
				},
				"schema": map[string]interface{}{
					"type":        "string",
					"description": "Schema containing the table. Defaults to the configured default schema for the default database, otherwise 'public'.",
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
				"database": map[string]interface{}{
					"type":        "string",
					"description": dbParamDesc,
				},
				"schema": map[string]interface{}{
					"type":        "string",
					"description": "Schema to put first on search_path, so unqualified table names resolve there (e.g. merchant_18). Defaults to the configured default schema for the default database, otherwise 'public'.",
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
				"database": map[string]interface{}{
					"type":        "string",
					"description": dbParamDesc,
				},
				"schema": map[string]interface{}{
					"type":        "string",
					"description": "Schema to put first on search_path. Defaults to the configured default schema for the default database, otherwise 'public'.",
				},
			},
			"required": []string{"query"},
		},
	},
}

func main() {
	envFileFlag := flag.String("env-file", "/Users/minhhuy/Repos/A/MCP/dev-RO-postgres/.env", "Path to .env file to load configuration from")
	flag.Parse()

	loadConfig(*envFileFlag)

	// Warm the default pool so startup problems surface in stderr immediately,
	// but never fail hard: other databases may still be reachable.
	if _, err := getDB(appCfg.Database, appCfg.Schema); err != nil {
		fmt.Fprintf(os.Stderr, "[dev-RO-postgres] Warning: initial DB connection failed: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[dev-RO-postgres] Connected to %s:%d/%s (schema: %s); any database on this server is reachable via the 'database' tool argument\n",
			appCfg.Host, appCfg.Port, appCfg.Database, appCfg.Schema)
	}
	defer closeAllPools()

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
		fmt.Fprintf(os.Stderr, "[dev-RO-postgres] Scanner error: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "[dev-RO-postgres] Note: could not read env file %s: %v\n", envPath, err)
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

// resolveTarget picks the database and search_path schema for one tool call.
func resolveTarget(args map[string]interface{}) (string, string, error) {
	dbName := appCfg.Database
	if v, ok := args["database"].(string); ok && strings.TrimSpace(v) != "" {
		dbName = strings.TrimSpace(v)
	}

	schema := ""
	if v, ok := args["schema"].(string); ok && strings.TrimSpace(v) != "" {
		schema = strings.TrimSpace(v)
	}
	if schema == "" {
		if dbName == appCfg.Database {
			schema = appCfg.Schema
		} else {
			schema = "public"
		}
	}

	if !identRe.MatchString(dbName) {
		return "", "", fmt.Errorf("invalid database name %q", dbName)
	}
	if !identRe.MatchString(schema) {
		return "", "", fmt.Errorf("invalid schema name %q", schema)
	}
	return dbName, schema, nil
}

func poolKey(dbName, schema string) string {
	return dbName + "\x00" + schema
}

// getDB returns a pooled connection for (database, schema), opening one on
// first use. A pool that has gone stale is discarded and reopened.
func getDB(dbName, schema string) (*sql.DB, error) {
	key := poolKey(dbName, schema)

	poolsMu.Lock()
	defer poolsMu.Unlock()

	if d, ok := pools[key]; ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := d.PingContext(ctx)
		cancel()
		if err == nil {
			return d, nil
		}
		d.Close()
		delete(pools, key)
	}

	d, err := openWithTunnel(dbName, schema)
	if err != nil {
		return nil, err
	}
	pools[key] = d
	return d, nil
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

// openDB dials one (database, search_path) pair. Only the configured tunnel
// port is used: never fall back to a local DB for the dev read-only server.
func openDB(dbName, schema string) (*sql.DB, error) {
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=5 search_path=%s,public",
		appCfg.Host, appCfg.Port, appCfg.User, appCfg.Password, dbName, appCfg.SSLMode, schema)

	testDB, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}
	testDB.SetMaxOpenConns(5)
	testDB.SetMaxIdleConns(2)
	testDB.SetConnMaxLifetime(10 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = testDB.PingContext(ctx)
	cancel()
	if err != nil {
		testDB.Close()
		return nil, fmt.Errorf("could not connect to database %q: %w", dbName, err)
	}
	return testDB, nil
}

// openWithTunnel tries the DB; on failure it starts the ssh tunnel when the
// port is not listening and retries once before giving up.
func openWithTunnel(dbName, schema string) (*sql.DB, error) {
	conn, err := openDB(dbName, schema)
	if err == nil {
		return conn, nil
	}
	fmt.Fprintf(os.Stderr, "[dev-RO-postgres] Connect to %q failed (%v), checking tunnel\n", dbName, err)
	if tErr := ensureTunnel(); tErr != nil {
		return nil, fmt.Errorf("%v; tunnel: %v", err, tErr)
	}
	conn, err2 := openDB(dbName, schema)
	if err2 != nil {
		return nil, fmt.Errorf("reconnect to %q after tunnel check failed: %w", dbName, err2)
	}
	return conn, nil
}

func closeAllPools() {
	poolsMu.Lock()
	defer poolsMu.Unlock()
	for k, d := range pools {
		d.Close()
		delete(pools, k)
	}
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
				"name":    "dev-RO-postgres",
				"version": "2.0.0",
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
	if args == nil {
		args = map[string]interface{}{}
	}

	dbName, schema, err := resolveTarget(args)
	if err != nil {
		return "", err
	}

	conn, err := getDB(dbName, schema)
	if err != nil {
		return "", err
	}

	switch name {
	case "list_databases":
		return handleListDatabases(conn)
	case "list_schemas":
		return handleListSchemas(conn, dbName)
	case "list_tables":
		return handleListTables(conn, dbName, schema)
	case "describe_table":
		table, _ := args["table"].(string)
		if strings.TrimSpace(table) == "" {
			return "", fmt.Errorf("table argument is required")
		}
		return handleDescribeTable(conn, dbName, schema, strings.TrimSpace(table))
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
		return handleReadQuery(conn, dbName, schema, q, limit)
	case "execute_query":
		q, _ := args["query"].(string)
		if strings.TrimSpace(q) == "" {
			return "", fmt.Errorf("query argument is required")
		}
		return handleExecuteQuery(conn, dbName, schema, q)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func handleListDatabases(conn *sql.DB) (string, error) {
	rows, err := conn.Query(`
		SELECT d.datname,
		       pg_get_userbyid(d.datdba) AS owner,
		       d.datallowconn,
		       d.datistemplate,
		       pg_size_pretty(pg_database_size(d.datname)) AS size
		FROM pg_database d
		WHERE d.datallowconn AND NOT d.datistemplate
		ORDER BY d.datname`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type DatabaseItem struct {
		Name      string `json:"database"`
		Owner     string `json:"owner"`
		Size      string `json:"size"`
		IsDefault bool   `json:"is_default,omitempty"`
	}

	var dbs []DatabaseItem
	for rows.Next() {
		var item DatabaseItem
		var allowConn, isTemplate bool
		if err := rows.Scan(&item.Name, &item.Owner, &allowConn, &isTemplate, &item.Size); err == nil {
			item.IsDefault = item.Name == appCfg.Database
			dbs = append(dbs, item)
		}
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"server":           fmt.Sprintf("%s:%d", appCfg.Host, appCfg.Port),
		"default_database": appCfg.Database,
		"count":            len(dbs),
		"databases":        dbs,
		"hint":             "Pass any of these names as the 'database' argument to the other tools.",
	}, "", "  ")
	return string(data), nil
}

func handleListSchemas(conn *sql.DB, dbName string) (string, error) {
	rows, err := conn.Query("SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('information_schema', 'pg_catalog', 'pg_toast') AND schema_name NOT LIKE 'pg_temp_%' AND schema_name NOT LIKE 'pg_toast_temp_%' ORDER BY schema_name")
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
		"database":         dbName,
		"default_database": appCfg.Database,
		"default_schema":   appCfg.Schema,
		"schemas":          schemas,
	}, "", "  ")
	return string(data), nil
}

func handleListTables(conn *sql.DB, dbName, schema string) (string, error) {
	rows, err := conn.Query(`
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
		"database": dbName,
		"schema":   schema,
		"count":    len(tables),
		"tables":   tables,
	}, "", "  ")
	return string(data), nil
}

func handleDescribeTable(conn *sql.DB, dbName, schema, table string) (string, error) {
	colRows, err := conn.Query(`
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
		return "", fmt.Errorf("table '%s.%s' not found in database '%s' or contains no columns", schema, table, dbName)
	}

	pkRows, err := conn.Query(`
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
		"database": dbName,
		"schema":   schema,
		"table":    table,
		"columns":  cols,
	}, "", "  ")
	return string(data), nil
}

func handleReadQuery(conn *sql.DB, dbName, schema, q string, limit int) (string, error) {
	trimmed := strings.TrimSpace(strings.ToUpper(q))
	destructiveKeywords := []string{"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE", "ALTER", "CREATE", "GRANT", "REVOKE"}
	for _, kw := range destructiveKeywords {
		pattern := `\b` + kw + `\b`
		matched, _ := regexp.MatchString(pattern, trimmed)
		if matched {
			return "", fmt.Errorf("read_query does not permit mutation/DDL statement containing '%s'. Use execute_query instead", kw)
		}
	}

	finalQuery := q
	if !strings.Contains(trimmed, "LIMIT") && !strings.HasPrefix(trimmed, "EXPLAIN") && !strings.HasPrefix(trimmed, "SHOW") {
		finalQuery = fmt.Sprintf("SELECT * FROM (%s) AS _subq LIMIT %d", strings.TrimSuffix(q, ";"), limit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rows, err := conn.QueryContext(ctx, finalQuery)
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
		"database":  dbName,
		"schema":    schema,
		"query":     q,
		"row_count": len(results),
		"rows":      results,
	}
	data, _ := json.MarshalIndent(resMap, "", "  ")
	return string(data), nil
}

func handleExecuteQuery(conn *sql.DB, dbName, schema, q string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := conn.ExecContext(ctx, q)
	if err != nil {
		return "", err
	}

	rowsAffected, _ := res.RowsAffected()
	data, _ := json.MarshalIndent(map[string]interface{}{
		"status":        "success",
		"database":      dbName,
		"schema":        schema,
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
