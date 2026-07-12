# UI Testing Guide — agent-beacon Browser-Driven Orchestration

This guide covers testing the **UI-driven multi-agent loop**: a user submits a
task + repo in the dashboard, the server runs the orchestrator (plan → parallel
worktree workers → Gate 1 build/test → Gate 2 cross-model verifier → self-repair),
the browser **streams live progress**, and on completion shows the report and a
diff of the delivered branch.

For the CLI-only `orchestrate` path, see [`TESTING.md`](TESTING.md).

## 0. Is UI testing possible? (yes — verified)

UI testing is fully feasible in this environment. The confirmations:

- The server serves the SPA (`web/static/{index.html,app.js,style.css}`, embedded
  via `web/web.go`) and the orchestration REST + WebSocket API.
- All `--orchestrate-*` server flags exist (`go run ./cmd/agent-beacon server --help`).
- Credentials for the multi-agent loop are already available via the HAI proxy
  environment: `ANTHROPIC_AUTH_TOKEN` (Bearer) + `ANTHROPIC_BASE_URL`. When either
  a key or bearer is present the server builds a `Messenger` and real runs execute.
  Without credentials the start route returns **503** (feature disabled), the rest
  of the UI still loads.
- A real end-to-end run against a scratch repo reached `status=done` with a
  populated report streamed over the events WebSocket.

## 1. Prerequisites

- **Go 1.25.1** (build/run the server).
- **git** on `PATH` (workers create per-subtask worktrees; the diff endpoint runs
  `git -C <repo> diff`).
- **A repo under an allowed root.** Browser-launched runs may only target a path
  under a server-configured `--orchestrate-root` (defaults to config `projects_dir`).
  This is the *only* place the browser can point a run at the filesystem — use a
  throwaway scratch repo, never a real working tree.
- **Anthropic credentials**, one of:
  - `ANTHROPIC_AUTH_TOKEN` (+ `ANTHROPIC_BASE_URL` for the HAI proxy) — already set
    in this environment; or
  - `ANTHROPIC_API_KEY`; or
  - `--orchestrate-api-key sk-...` on the server command.
  - The worker also needs the `claude` binary logged in (or override with
    `--orchestrate-worker-cmd`).

## 2. Prepare a scratch repo

```bash
mkdir -p /tmp/orch-ui && cd /tmp/orch-ui
git init -q
cat > go.mod <<'EOF'
module example.com/orchui

go 1.25
EOF
cat > add.go <<'EOF'
package orchui

func Add(a, b int) int { return a - b } // intentionally wrong
EOF
cat > add_test.go <<'EOF'
package orchui

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add(2,3) = %d, want 5", Add(2, 3))
	}
}
EOF
git add -A && git commit -qm "seed with failing test"
```

## 3. Start the server for UI testing

### Option A — open access (simplest for local testing)

```bash
go run ./cmd/agent-beacon server \
  --auth none \
  --address :18099 \
  --orchestrate-root /tmp/orch-ui \
  --orchestrate-worker-model claude-sonnet-4-6 \
  --orchestrate-verifier-model claude-opus-4-6 \
  --orchestrate-timeout 15m
```

### Option B — password auth (exercises the login gate)

```bash
go run ./cmd/agent-beacon server \
  --auth password --password test123 \
  --address :18099 \
  --orchestrate-root /tmp/orch-ui \
  --orchestrate-worker-model claude-sonnet-4-6 \
  --orchestrate-verifier-model claude-opus-4-6 \
  --orchestrate-timeout 15m
```

Notes:
- The HAI-proxy env (`ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_BASE_URL`) is picked up
  automatically; no extra flag needed. To use a direct key instead, add
  `--orchestrate-api-key sk-...`.
- Pick a free port; if you see `address already in use`, a prior server is still
  bound — stop it or choose another `--address`.
- Cross-model check: keep worker ≠ verifier (Sonnet vs Opus). If they match the
  verifier step is weakened.

## 4. Browser walkthrough (happy path)

1. Open **http://localhost:18099**.
2. With `--auth password`, sign in with the password (`test123` above). With
   `--auth none` you land directly on the dashboard.
3. In the left sidebar find the **Orchestrations** section; click the **+** button.
   The stage switches to the **New orchestration** launcher.
4. Fill the form:
   - **Task** — e.g. `make all unit tests pass and add one more test for the fixed function`.
   - **Repository** — `/tmp/orch-ui` (the datalist suggests allowed roots).
   - **Workers** — `2` (optional; blank = auto).
   - **Max iterations** — `3` (optional; blank = default).
5. Click **Start run**. On success (`202 {id}`) the stage switches to the **live
   run** view and a card appears in the Orchestrations sidebar list.
6. Watch the **live stream** (over the events WebSocket):
   - A **plan** appears, then one **subtask card** per subtask (branch `orch/<slug>`).
   - Each card advances through **worker-start → iteration N → Gate 1 (build/test)
     → Gate 2 (verifier verdict) → worker-done**.
   - The **Live log** pane fills with `log` lines in real time.
7. On **run-done** the **status pill** turns green (`pass`) or red (`fail`) and a
   **report** block renders per-subtask pass/fail, iterations, branch, and verifier
   notes.
8. Click **View diff** to fetch `GET /api/v1/orchestrations/{id}/diff` — the
   delivered changes render as text. The `orch/<slug>` branch is the deliverable.

### Reconnect / replay check

Refresh the browser mid-run, or click the run card again after completion. Because
the events WS **replays the buffered history then streams live**, you should see the
full run reconstructed (and for a finished run the socket closes cleanly after
draining history).

## 5. Endpoints and expected status codes

All routes are gated by `browserAuthorized` (cookie/proxy/none per `--auth`).

| Method | Path | Purpose | Expected |
|---|---|---|---|
| `POST` | `/api/v1/orchestrations` | start a run `{task, repo, workers?, max_iters?, *_model?}` | **202** `{id}` |
| `GET` | `/api/v1/orchestrations` | list runs, newest-first | **200** `[runView]` |
| `GET` | `/api/v1/orchestrations/{id}` | run + full report (`report` null until done) | **200** / **404** |
| `GET` | `/api/v1/orchestrations/{id}/events` | **WS**: replay buffer then live JSON events, close on run-done | **101** upgrade |
| `GET` | `/api/v1/orchestrations/{id}/diff` | `git diff` (optional `?branch=orch/<slug>` → `HEAD..<branch>`) | **200** text/plain |

Negative / boundary results:

- **401** — any route without a valid browser session (`--auth password`, no cookie).
- **503** — `POST` start when the server has **no Anthropic credentials** (Messenger nil).
- **400** — empty/whitespace `task`; or `repo` not under an allowed `--orchestrate-root`.
- **404** — `GET .../{id}` or `.../{id}/events` for an unknown run id.
- **400** — `/diff?branch=` with an unsafe ref (leading `-`, `..`, or non-slug chars).

## 6. Live event kinds the UI renders

The events WebSocket carries `orchestrator.Event` JSON frames. `kind` values:

| kind | UI effect |
|---|---|
| `plan` | plan summary logged; subtask cards prepared |
| `worker-start` | subtask card created for `branch` / `subtask_id` |
| `iteration` | card shows current iteration N |
| `agent` | coding-agent activity logged |
| `gate` | Gate 1 (build/test) result on the card (ok/err) |
| `verdict` | Gate 2 cross-model verifier verdict + notes |
| `worker-done` | subtask card marked pass/fail |
| `log` | appended to the Live log pane |
| `run-done` | status pill final; report block rendered; WS closes |

Event fields: `kind, time, subtask_id, branch, iteration, passed, message, detail`.

## 7. Reproducible API test cases (curl / websocat)

With `--auth none` on `:18099` and repo `/tmp/orch-ui`:

```bash
BASE=http://localhost:18099

# health / SPA served with orchestration UI
curl -s $BASE/ | grep -o 'orch-launch-panel'          # => orch-launch-panel
curl -s $BASE/app.js | grep -o 'startOrchestration'   # => startOrchestration

# list (initially empty)
curl -s $BASE/api/v1/orchestrations                   # => []

# start a run -> 202 {"id":"run-..."}
RID=$(curl -s -XPOST $BASE/api/v1/orchestrations \
  -H 'content-type: application/json' \
  -d '{"task":"make all unit tests pass","repo":"/tmp/orch-ui","workers":1,"max_iters":2}' \
  | sed -E 's/.*"id":"([^"]+)".*/\1/')
echo "$RID"

# poll detail until status=done
curl -s $BASE/api/v1/orchestrations/$RID | jq '.run.status,.run.passed'

# stream events live (if websocat is installed)
websocat "ws://localhost:18099/api/v1/orchestrations/$RID/events"

# diff of delivered work
curl -s "$BASE/api/v1/orchestrations/$RID/diff"

# negatives
curl -s -o /dev/null -w '%{http_code}\n' $BASE/api/v1/orchestrations/nope        # 404
curl -s -o /dev/null -w '%{http_code}\n' -XPOST $BASE/api/v1/orchestrations \
  -H 'content-type: application/json' -d '{"task":"  ","repo":"/tmp/orch-ui"}'   # 400
curl -s -o /dev/null -w '%{http_code}\n' -XPOST $BASE/api/v1/orchestrations \
  -H 'content-type: application/json' -d '{"task":"x","repo":"/etc"}'            # 400 (outside root)
```

## 8. Automated regression (offline)

The server routes, store fan-out/replay, and validation are unit-tested with a
stubbed `Messenger` — no network:

```bash
go build ./... && go vet ./... && go test ./...

# just the UI-backing package:
go test ./internal/server/... -run Orch -v
```

`internal/server/orchestrations_test.go` covers: replay-then-stream, done-close for
late subscribers, finish closing live subscribers, bounded replay buffer, start
requires auth (401), missing credentials (503), empty task (400), repo outside
roots (400), list requires auth, empty list, unknown run (404), and `isSafeBranchRef`.

## 9. Cleanup

```bash
cd /tmp/orch-ui
git worktree list
git worktree remove <path> --force   # remove each worker worktree
git branch -D orch/<slug>            # delete delivered branches
cd / && rm -rf /tmp/orch-ui
# stop the server (Ctrl-C); it shuts down gracefully via signal.NotifyContext
```

## Notes

- **Runs survive the request.** The run context is `context.Background()` +
  `--orchestrate-timeout`, not the HTTP request context, so a run keeps going after
  the `POST` returns and the browser can re-attach.
- **Repo safety is enforced server-side** via `agent.ResolveUnderRoot`; the browser
  can never write outside an allowed root.
- **No secrets to the client.** The Anthropic key/bearer live only server-side and
  are never logged or returned.
- **Determinism before LLM.** Gate 1 (build/test) must pass before a verifier call
  is spent (unchanged from the CLI path).
