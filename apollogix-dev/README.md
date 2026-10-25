# apollogix-dev MCP server

Read-only health and monitoring for the `apollogix_dev` VM (`apollogix-vm-dev`,
22 containers across the FMS, TMS, CDC and log stacks).

## Transport

Two channels, both over the `apollogix_dev` alias in `~/.ssh/config`:

1. **Docker Engine API over SSH** — `github.com/moby/moby/client` with
   `github.com/docker/cli/cli/connhelper`, which runs
   `ssh apollogix_dev docker system dial-stdio`. Structured JSON; no text
   parsing of `docker ps`. The private key stays with OpenSSH.
2. **SSH exec** — `df`, `free`, `curl` and bash `/dev/tcp` for the things the
   Engine API cannot answer: host filesystem usage and health probes.

Measured: first dial ~0.9s (SSH handshake), then 9–19ms per inspect on the
multiplexed connection. A full 22-container health sweep with probes: ~1.1s.

## Why a separate probe layer

- Only **6 of 22** containers define a docker healthcheck. For the other 16,
  docker knows only that the process is running.
- Published ports are **firewalled from outside the host** (8091 and 8081 time
  out from a laptop; 3001 answers), so probes must run on the host itself.
- A docker healthcheck can be wrong. `fms-backend-service` probes
  `localhost:8081` while the app listens on `8091`, so docker reports
  `unhealthy` for a service that answers `200`. `health_overview` reports the
  three signals separately and flags the disagreement rather than picking one.

## Tools

| Tool | What it does |
| --- | --- |
| `health_overview` | Scans containers matching the configured patterns. Per service: docker healthcheck, host-side port probe, runtime state/restarts, plus a combined verdict. Flags `SIGNAL MISMATCH` and `NO DOCKER HEALTHCHECK`. |
| `health_probe` | One service in detail: TCP reachability, HTTP code and latency, the healthcheck command and its last probe output. |
| `events_recent` | Engine events recorded since the server started. Lifecycle only by default; `all=true` includes `exec_*` healthcheck traffic. |
| `restart_history` | Restart counts, uptime and start/die counts from the buffer. Flags `HIGH-RESTART-COUNT`, `OOM-KILLED`, `RECENTLY-RESTARTED`. |
| `stats_snapshot` | One-shot CPU, memory, network and PIDs per service, collected in parallel, plus host memory. |
| `stats_disk` | Host filesystems, docker images/containers/volumes/build-cache breakdown, per-volume detail, OK/WARNING/CRITICAL verdict and reclaim suggestions. |

## Scanning scope — three layers

`health_overview`, `restart_history` and `stats_snapshot` resolve their scope in
three layers and stop at the **first layer that actually selects a container**:

| Layer | Source | Treated as invalid when |
| --- | --- | --- |
| 1 | `patterns` passed in the MCP call | absent, or matches no container |
| 2 | `APOLLOGIX_DEV_CONTAINER_PATTERNS` in `.env` beside the server | empty, or matches no container |
| 3 | hardcoded `tms-*`, `fms-*` | — (last resort) |

Patterns are comma-separated globs matched against the **compose service name**,
falling back to the container name. An empty layer 2 is *invalid*, not "scan
everything": the server falls through to the hardcoded fallback rather than
silently widening to all 22 containers.

Every response prints the resolution so the caller can see which layer won:

```
scope: tms-*,fms-* (from fallback) | skipped -> parameters: "also-nope-*" matched no container; config: "nope-*" matched no container
```

Scope resolution costs one round trip: all containers are listed and inspected
once, then filtered in memory per layer.

The container-name fallback matters: five services on this host carry generic
compose labels — `app` (twice: `apollogix-web` and `apl-blog-cms`), `db`,
`connect`, `elasticsearch`, `filebeat` — so service-name matching alone would
be ambiguous or miss them.

## Safety

- Read-only. There is no tool that restarts, prunes, deletes or deploys
  anything. `stats_disk` prints commands for the operator to run and review.
- Data-bearing volumes (names matching postgres/kafka/redis/elastic/`*_data`)
  are never suggested for deletion, even when docker reports `LINKS=0`.
- Every argument sent over SSH is single-quoted before the command string is
  assembled. `ssh` concatenates its argv into one string for the remote shell,
  so passing separate argv entries does **not** preserve word boundaries;
  quoting is what prevents expansion, redirection and chaining.
- HTTP probe paths are validated against a whitelist regex.
- Output passes a secret-redaction filter and is capped at
  `APOLLOGIX_DEV_MAX_OUTPUT_BYTES`.
- `sudo` on the host requires a password and is never attempted. The server
  operates with the `docker` group membership of the `apollogix` user.

## Build

```sh
cd /Users/minhhuy/Repos/A/MCP/apollogix-dev
go build -o apollogix-dev .
```

## Configuration

`.env` (chmod 600) holds no credentials — only the SSH alias and tuning values.
See the file for the full key list.

## Registration

```json
"apollogix-dev": {
  "command": "/Users/minhhuy/Repos/A/MCP/apollogix-dev/apollogix-dev",
  "args": ["--env-file", "/Users/minhhuy/Repos/A/MCP/apollogix-dev/.env"]
}
```

## Staging

The same binary works against `apollogix_stg` by pointing
`APOLLOGIX_DEV_SSH_HOST` at it. Use a separate `.env` and a separate MCP entry
rather than switching this one.
