# redis-RO

Redis MCP (Go, stdio) that opens and manages SSH tunnels to remote hosts.

## Config: `.env` only

One line per host, holding the literal tunnel command:

```
REDIS_TUNNEL_APOLLOGIX_DEV=ssh -N -v -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o BatchMode=yes -L 127.0.0.1:16380:127.0.0.1:6380 apollogix_dev
```

- `<HOST>` (lower-cased) is the `host` you pass to tools; `-`/`_`/case are interchangeable.
- The `-L local:host:remote` flags are the port mappings; the first one is the default for `redis_*` tools.
- The last argument is the ssh alias from `~/.ssh/config`.
- The command runs as-is (split on spaces, no shell, no quoting).
- No MCP listening port: this MCP speaks stdio.
- Add a host = add a line, then reconnect the MCP (`/mcp`); config is read at startup.
- `.run/` holds the pid and ssh log per host. `-v` in the command makes the log verbose; `tunnel_status` with a host and `tunnel_connect` return its tail.

## Tools

- `tunnel_connect`, `tunnel_disconnect` (`host` or `all`), `tunnel_status`
- `redis_keys`, `redis_get`, `redis_hgetall`, `redis_type`, `redis_ttl`, `redis_lrange`, `redis_zrange`, `redis_smembers`, `redis_dbsize`, `redis_info`

Every `redis_*` tool takes `host` and optional `port` (remote port, default = first configured) and opens the tunnel if it is down.

Tunnels are detached `ssh -N` processes that survive the MCP, so CLI tools can reuse them, e.g. `redis-cli -h 127.0.0.1 -p 16380`. `disconnect` only kills tunnels started by this MCP (pid file + command check).

## Build and register

```
cd MCP/redis-RO && go build -o redis-RO .
claude mcp add --scope user redis-RO -- MCP/redis-RO/redis-RO --env-file MCP/redis-RO/.env
```

## Limits

Read-only is enforced by exposing only read tools. The forwarded local port itself is an open Redis, so a CLI tool can still write; use a Redis ACL user for hard enforcement.
