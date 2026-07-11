# agent-beacon

A self-hosted dashboard for coding-agent (Claude Code) sessions running across
many machines. It is **observe-first**: a lightweight monitor daemon on each host
automatically discovers the `claude` processes already running there and shows
them in one view — no need to launch anything through a wrapper. From the
dashboard you can also spawn brand-new interactive sessions on remote hosts and
attach a browser terminal to them. All traffic is a single outbound WebSocket per
host (NAT-friendly, no inbound ports on agent hosts).

This is an **original implementation** written in Go. It is functionally similar
to, but independent from, the closed-source `ai-beacon`; it follows the same
openly documented architecture patterns.

## Two kinds of session

| Kind | How it appears | Terminal? |
|------|----------------|-----------|
| **Observed** | The monitor daemon discovers a running `claude` process (process scan + optional Claude hook enrichment) and reports it read-only. | No — read-only monitor card (pid, cwd, branch, model, state). |
| **Managed** | Started under a PTY by `agent-beacon session -- …` or a dashboard **spawn**. | Yes — full bidirectional browser terminal. |

You cannot attach a terminal to an already-running foreign `claude` process (its
stdin/TTY belongs to whoever launched it). To get an interactive terminal, spawn
a **new** managed session from the dashboard, or run `session -- claude` yourself.

## Components

| Command | Role |
|---------|------|
| `agent-beacon server`  | Runs the dashboard + REST + agent/terminal/monitor WebSockets. |
| `agent-beacon monitor` | Persistent daemon: scans for local `claude` processes, reports them as observed sessions, and accepts spawn requests. |
| `agent-beacon report`  | One-shot: POSTs session metadata to the local monitor listener (called from Claude hooks to enrich observed cards). |
| `agent-beacon session -- <cmd>` | Wraps a coding-agent process in a PTY as a managed, terminal-attachable session. |
| `agent-beacon install` | Writes `config.toml` and injects **report hooks** into `~/.claude/settings.json` so observed cards get model/task/state labels. |
| `agent-beacon uninstall` | Removes the managed hooks (`--purge-data` also drops token/config). |
| `agent-beacon version` | Prints the build version. |

## Quick start (local)

```bash
make build

# 1. Start the server (open auth for local dev; PSK for agents).
bin/agent-beacon server --address :8090 --auth none --auth-token dev-token &

# 2. Run the monitor daemon — it discovers the claude processes already
#    running on this machine and reports them as read-only observed cards.
AGENT_BEACON_URL=http://localhost:8090 \
AGENT_BEACON_AUTH_TOKEN=dev-token \
  bin/agent-beacon monitor --device my-mac &

# 3. Open the dashboard — every running `claude` shows up automatically.
open http://localhost:8090
```

`--auth none` is for local development only. Use `password`, `proxy-header`, or
`oidc` for any shared/networked deployment (see [Auth modes](#auth-modes)).
The examples use `:8090`; the code default is `:8080`.

For an interactive, terminal-attachable session, wrap a command yourself:

```bash
AGENT_BEACON_URL=http://localhost:8090 \
AGENT_BEACON_AUTH_TOKEN=dev-token \
  bin/agent-beacon session -- bash
```

## Connect an agent host (recommended flow)

On each machine that runs `claude`, run `install` once (writes `config.toml` and
injects report hooks) and leave the monitor daemon running:

```bash
# Point the agent at your server and give it the shared PSK.
export AGENT_BEACON_URL=https://beacon.example.com
export AGENT_BEACON_AUTH_TOKEN=<shared-psk>

# Optional: name this device and expose project roots for remote spawn.
export AGENT_BEACON_DEVICE=laptop-alice
export AGENT_BEACON_PROJECTS_DIR=$HOME/src

# Write config.toml + inject report hooks into ~/.claude/settings.json.
agent-beacon install

# Start the persistent monitor (e.g. as a login item / systemd user service).
agent-beacon monitor

# From now on, any `claude` you run anywhere shows up in the dashboard,
# enriched with model / task / state via the report hooks.
claude
```

The report hooks call `agent-beacon report` on Claude
`SessionStart`/`Stop`/`Notification` events, POSTing metadata to the monitor's
loopback listener (`127.0.0.1:47615` by default, `--report-addr` to change) so
each observed card is labelled with model, task, and state. Discovery still works
without the hooks — they only add richer labels (and the intervention alerts
below).

`agent-beacon uninstall` removes the hooks (add `--purge-data` to also drop the
local token/config).

Device name precedence: `--device` flag → `AGENT_BEACON_DEVICE` env →
`config.toml` → hostname → `unknown`.

## Spawn a session from the dashboard

If an agent host exports `AGENT_BEACON_PROJECTS_DIR`, its git projects appear in
the dashboard. Click **+ Open Session** under a device to open the spawn dialog:

- **Project** — a git repo discovered under the projects dir.
- **Command** — defaults to `claude`.
- **Git worktree branch** *(optional)* — creates a fresh worktree via
  `git worktree add -B <branch>`; leave empty to run in the repo root.
- **Worktree location** — sibling directory (default) or `.worktrees/` subdirectory.

The server routes the request to the live monitor daemon on that device, which
validates the project path (must resolve under an allowed root), prepares the
worktree, and launches a new detached `agent-beacon session` there. The new
managed session appears in the dashboard — with an attachable terminal — within a
couple of seconds.

## Getting alerted when Claude needs you

Claude periodically blocks on the user — asking permission to run a tool,
answering a question, or otherwise waiting for input. agent-beacon surfaces this
in real time so you can walk away and still know the moment a session needs
attention.

The mechanism rides Claude Code's **`Notification`** hook, which `install`
injects alongside the other report hooks. When Claude blocks, the hook runs
`agent-beacon report --event Notification --state waiting`, the monitor marks
that session **waiting**, and the next observed snapshot carries the state to the
dashboard. A subsequent event (a heartbeat, or `Stop`) clears it automatically.

On the dashboard, a waiting session shows up three ways:

- **Card highlight** — the session card gets a pulsing amber border (the pulse is
  suppressed under `prefers-reduced-motion`).
- **Sidebar banner** — a top-of-list `⚠ N sessions waiting for you` count.
- **Title badge** — the browser tab title becomes `(N) agent-beacon`, and a
  one-shot **browser notification** fires the moment a session enters the waiting
  state (permission is requested lazily on first use). It fires once per
  transition, not on every 2s poll.

This works for any discovered `claude` — but a process it merely **observed** has
no interactive terminal, so you can see it needs you but must respond in whatever
terminal actually launched it. To respond *from the browser*, run Claude as a
**managed** session (dashboard **+ Open Session**, or `session -- claude`): you
get the full TUI in the browser terminal *and* the alerts, so you can both see
the blocking prompt and answer it in one place.

## Auth modes

Selected with `--auth`; `login-info` reflects the active mode.

| Mode | Flags | Behaviour |
|------|-------|-----------|
| `none` | — | Open. **Local dev only.** |
| `password` | `--password` / `--password-hash` (or `AGENT_BEACON_PASSWORD[_HASH]`) | bcrypt-checked password → opaque session cookie (HttpOnly, SameSite=Lax, Secure over TLS). |
| `proxy-header` | `--proxy-header`, `--proxy-allow` | Trusts a reverse-proxy header (e.g. `X-Forwarded-User`) against an allowlist. |
| `oidc` | `--oidc-issuer`, `--oidc-client-id`, `--oidc-client-secret`, `--oidc-redirect-url` | Authorization-code flow with issuer discovery; subject = email or sub. |

Common flags: `--session-ttl` (default 12h), `--provider-name` (label on the login screen).
Agents always authenticate separately with the `--auth-token` PSK (bearer, constant-time compare) — independent of the browser auth mode.

## Build & release

```bash
make build              # local binary -> bin/agent-beacon  (version from git describe)
make test               # go test ./...
make vet                # go vet ./...
make dist               # cross-compile matrix -> dist/  (linux/darwin/windows, amd64/arm64)
make VERSION=1.2.3 dist # stamp an explicit version
```

`make dist` builds static (`CGO_ENABLED=0`) binaries for:
`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`.

## Docker

Multi-stage build produces a small static image that runs as a non-root user;
`git` is included in the runtime layer for the worktree/spawn feature.

```bash
make docker                       # builds agent-beacon:<version>
docker run --rm -p 8080:8080 agent-beacon:<version> \
  server --address :8080 --auth none --auth-token dev-token
```

Or with plain Docker:

```bash
docker build --build-arg VERSION=$(git describe --tags --always) -t agent-beacon .
```

## Architecture

```
agent host
  ├─ agent-beacon monitor            (persistent daemon, observe-first)
  │    ├─ scans the process table for running `claude` procs
  │    ├─ loopback report listener 127.0.0.1:47615  ← `agent-beacon report` (hooks)
  │    ├─ outbound WebSocket → server /api/v1/agent/connect?role=monitor (PSK)
  │    │     periodic FrameObserved snapshots (read-only sessions)
  │    └─ accepts spawn control frames → launches a managed `session`
  │
  └─ agent-beacon session -- claude  (managed, on demand / via spawn)
       ├─ PTY runs the real agent
       ├─ outbound WebSocket → server /api/v1/agent/connect (PSK)
       │     heartbeats + PTY output
       └─ accepts input / resize control frames

server (single process, in-memory registry)
  ├─ dashboard SPA + REST + /api/v1/health + /api/v1/login-info
  ├─ /api/v1/agent/connect            (agent WS, PSK; role=monitor | managed)
  ├─ /api/v1/terminal/{id}            (browser WS, session cookie; managed only)
  └─ /api/v1/spawn                    (browser → device monitor control frame)
```

## HTTP surface

| Method & path | Purpose |
|---------------|---------|
| `GET /api/v1/health` | Liveness. |
| `GET /api/v1/login-info` | `{provider, provider_name, start_url, logout_url}`. |
| `GET /api/v1/sessions` | Live sessions grouped by device — observed + managed (+ discovered projects). |
| `POST /api/v1/login` / `POST /api/v1/logout` | Password session lifecycle. |
| `GET /api/v1/auth/start` / `.../auth/callback` | OIDC authorization-code flow. |
| `POST /api/v1/spawn` | Request a new managed session on a device. |
| `WS /api/v1/agent/connect` | Agent uplink (PSK bearer). `?role=monitor` = observed-snapshot stream; otherwise one managed session. |
| `WS /api/v1/terminal/{id}` | Browser terminal attach (session cookie; managed sessions only). |
