# Orchestration Architecture

An analysis of agent-beacon's orchestration engine: the autonomous
"Plan → Fan-out → Self-Repair Loop → Cross-Model Verify" coordinator that
drives a virtual team of coding-agent workers to completion.

Source of truth (as of this writing):

- `internal/orchestrator/orchestrator.go` — Plan (decomposition) + Run (concurrent dispatch)
- `internal/orchestrator/worker.go` — the per-subtask self-repair loop (the heart)
- `internal/orchestrator/verifier.go` — Gate 2 cross-model verifier
- `internal/orchestrator/danger.go` — heuristic dangerous-command detection
- `internal/orchestrator/events.go` — progress event model
- `internal/verify/gates.go` — Gate 1 deterministic build/test/e2e
- `internal/agent/spawn.go` — git worktree isolation & path-boundary safety
- `internal/server/orchestrations.go` — HTTP/WS exposure + human intervention (Intervener)

---

## 1. Core Concept

The orchestrator autonomously coordinates a "virtual engineering team". It
decomposes one development task into independent subtasks, runs a headless
coding agent for each inside an **isolated git worktree**, and loops each one
through a self-repair cycle of **deterministic gates + cross-model review**
until it passes or exhausts a per-worker iteration budget.

---

## 2. The Three Agent Roles (multi-model division of labor)

Unlike a traditional recursive subagent tree, this design uses three distinct,
non-recursive roles with clear separation:

| Role | Implementation | Model | Transport |
|------|----------------|-------|-----------|
| **Planner** | `Plan()` (orchestrator.go) | `PlannerModel` | Anthropic API + forced tool_call `emit_plan` |
| **Worker** | `runCodingAgent()` (worker.go) | `WorkerModel` (default `claude` CLI) | **subprocess** `claude -p ... --output-format stream-json` |
| **Verifier** | `Verify()` (verifier.go) | `VerifierModel` (**must differ from Worker**) | Anthropic API + forced tool_call `emit_verdict` |

Key points:

- **Planner and Verifier are API calls** (`anthropic.Messenger`) using
  **forced `tool_choice`** so their output is always structured JSON, never
  free-form prose.
- **Worker is a real subprocess**: it forks the `claude` CLI, runs headless
  with `--permission-mode acceptEdits`, and streams every step (tool_use,
  text, result) back as **NDJSON stream-json**, parsed line-by-line into
  `EventLog` entries for the live UI.
- **Verifier uses a different model** than the Worker (e.g. Worker=Sonnet,
  Verifier=Opus) for cross-model verification — preventing a model from
  rubber-stamping its own work.

---

## 3. Architecture Diagram

```
┌───────────────────────────────────────────────────────────────────────────┐
│  Browser (SPA)                                                              │
│   POST /api/v1/orchestrations        WS /events (replay+live)   POST /respond│
└───────┬───────────────────────────────────┬────────────────────┬───────────┘
        │                                    │ Event JSON          │ Decision
        ▼                                    │ (fan-out)           ▼
┌───────────────── server/orchestrations.go ─────────────────────────────────┐
│  orchStore.start() → orchRun{ Emitter + Intervener }                        │
│    • Emit()  : append replay buffer + fan out to subscribers                │
│                (slow consumers drop frames instead of blocking)             │
│    • Await() : block on pending[ReqID] channel; timeout → safe default(deny)│
└───────┬─────────────────────────────────────────────────────────────────────┘
        │ inject cfg.Emitter / cfg.Intervener
        ▼
┌────────────────────── orchestrator.Run() ───────────────────────────────────┐
│  1. Plan(task) ──API──▶ Planner  ──emit_plan──▶ []Subtask{goal,branch,accept}│
│  2. sem := chan struct{}, workers   (bounded concurrency)                    │
│  3. for st in subtasks:  go RunWorker(st)   ──── WaitGroup ────              │
└───────┬──────────────────────────────────────────────────────────────────────┘
        │ (one goroutine per subtask, semaphore-limited)
        ▼
┌──────────────────── RunWorker (worker.go) — self-repair loop ────────────────┐
│  createWorktree() → repo-<branch>/   (isolated, -B idempotent)               │
│                                                                              │
│  for i := 1..maxIters(+guided extras):                                       │
│    ┌──────────────────────────────────────────────────────────────────┐    │
│    │ (a) runCodingAgent: exec `claude -p <prompt+feedback>`             │    │
│    │        stream-json NDJSON ─▶ summarize each line ─▶ EventLog       │    │
│    │        also scan Bash tool_use → scanDangerous() collect DangerHit │    │
│    │           │ error? → feedback=err; classify env/logic; continue    │    │
│    │           ▼                                                        │    │
│    │ (b) if DangerHit → authorizeDangers() → Await(human auth)          │    │
│    │           deny/timeout → subtask ends safely                       │    │
│    │           ▼                                                        │    │
│    │ (c) GATE 1 (deterministic): build→check→test, fail-fast            │    │
│    │           fail → feedback=gate output; continue ↺                  │    │
│    │           ▼                                                        │    │
│    │ (d) worktreeDiff (git add -N to capture new files)                 │    │
│    │           ▼                                                        │    │
│    │ (e) GATE 2 (cross-model): Verify() ──API──▶ Verifier ──emit_verdict▶│    │
│    │           pass → res.Passed=true, RETURN ✓                         │    │
│    │           fail → feedback=reasons+missing; continue ↺              │    │
│    └──────────────────────────────────────────────────────────────────┘    │
│  budget exhausted on last iter → requestGuidance() → Await(optional human    │
│                                   guidance, extra ≤ maxExtraGuidedIters=3)    │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## 4. Agent-to-Agent Communication: no direct A2A — everything is mediated

A defining characteristic of this design: **workers never talk to each other**,
and Planner/Worker/Verifier never converse directly. All coordination flows
through orchestrator state and text feedback:

1. **Planner → Worker**: a `Subtask{Goal, Acceptance, Branch}` struct, rendered
   into the worker prompt via `buildPrompt`.
2. **Gate → Worker (the self-repair feedback loop)**: the only "iterative"
   channel. Failure signals are serialized into **natural-language feedback**
   injected into the next prompt:
   - Gate 1 fail → `"Deterministic gates failed. Fix these:\n" + gateOutput`
   - Gate 2 fail → `"An independent reviewer judged the work incomplete.\nReasons:\n... Missing:\n..."`
3. **Worker → Verifier**: no direct conversation. The Verifier only sees the
   **code diff + gate output** and judges like a real code review — "You did
   NOT write this code. Do not give the benefit of the doubt."
4. **Loop ↔ Human**: via the `Intervener` interface, crossing the process
   boundary — the worker goroutine blocks on a channel; the browser wakes it
   through `POST /respond` keyed by `ReqID`.

> **Essence**: worker isolation is guaranteed by **git worktree (physical
> filesystem isolation) + separate branch**, so workers run truly in parallel
> without stepping on each other. Coordination is entirely **share-nothing +
> a centralized event stream**.

---

## 5. How the Loop Is Implemented

The `RunWorker` loop:

```go
for i := 1; i <= maxIters+extraIters; i++
```

- **Unidirectional state progression**: Agent → Gate1 → (Danger auth) → Gate2.
  Any failed step does `continue` back to the top and writes the reason into
  the `feedback` variable, injected next round by `buildPrompt`.
- **Success exits**: only a Gate 2 (Verifier) `pass` does `return res`
  (Passed=true).
- **Failure classification** (`classifyFailure`): errors are split into
  `environment` (missing tool/permission → logged as self-repair) vs `logic`
  (code problem), purely for reporting; both continue retrying.
- **Budget guardrail**: base budget `MaxIters`; when exhausted, the last
  iteration may request human guidance, each grant awarding extra iterations,
  hard-capped by `maxExtraGuidedIters = 3` — so guidance can never loop forever.
- **Global clock guardrail**: `Run`'s parent ctx carries `OrchestrationTimeout`
  so a whole run can never run forever.

**Concurrency model**: `sem := make(chan struct{}, workers)` as a semaphore +
`sync.WaitGroup` for join; `results[idx]` written by index (lock-free — each
goroutine writes a distinct slot).

---

## 6. Event Stream & Human Intervention (Server Layer)

`orchRun` implements **both** `Emitter` and `Intervener`, bridging the stateless
orchestration engine to the stateful web service:

- **Emit (fan-out)**: events go into a `maxEventReplay=4096` ring replay buffer,
  then fan out to all WS subscribers. **Slow consumers drop frames (`default:`)
  rather than block** — protecting the orchestration loop from a slow browser.
- **Subscribe (replay-then-stream)**: a newly connected browser first receives
  the full backlog, then the live stream — so late subscribers see the whole run.
- **Await (blocking intervention)**: when a worker pauses, it registers a
  `pending[ReqID]` channel, flips status to `waiting`, and blocks until
  `/respond` delivers or the timeout fires. **Timeout returns the safe default
  (deny).**
- **Run lifecycle outlives the HTTP request**: the run uses
  `context.Background()`, so POST returns `{id}` immediately and the run lives
  on in a background goroutine.

**Security boundaries**:

- `ResolveUnderRoot`: symlink-resolved path-boundary check — the browser can
  only point a run at a directory under a server-configured allowed root,
  preventing path traversal.
- `sanitizeBranch` / `isSafeBranchRef`: allow-listed branch character set,
  preventing git flag / shell injection.
- Danger detection is **heuristic** (a regexp denylist); the code explicitly
  notes it is "best-effort heuristic detection, not a security boundary".

---

## 7. Design Trade-offs

| Design choice | Upside | Cost / Risk |
|---------------|--------|-------------|
| **Text feedback as the only A2A channel** | Simple, readable, debuggable, model-agnostic | Feedback is lossy compression; Verifier's `Reasons/Missing` flattened into a prompt can lose structure; no way to express fine-grained "partially correct" state |
| **Fully isolated workers (worktree)** | True parallelism, no conflicts, independently diffable | **Cannot handle dependent subtasks** (Planner is told to only split "independent" tasks); nobody owns cross-subtask integration; no final merge/integration stage — N branches each pass but may not compile together |
| **Cross-model verification (Worker ≠ Verifier)** | Prevents self-endorsement, independent judgment | Doubles model cost; Verifier only sees diff+gate, not runtime behavior — may misjudge (diff looks right but e2e scenarios uncovered) |
| **Gate 1 fail-fast** | Saves compute (no tests when build is broken) | Surfaces one failure at a time, may need multiple rounds to see everything; incomplete feedback lengthens iteration |
| **acceptEdits headless + post-hoc danger detection** | Smooth, non-blocking | Danger detection fires **after** the tool_use already executed (code admits "may already have executed"); `rm -rf` has already run by the time authorization is prompted — largely toothless |
| **Frame-dropping event fan-out** | Orchestration loop never blocked by a slow client | Browser may miss intermediate events (but replay buffer + terminal event backstop it — acceptable) |
| **Share-nothing + centralized coordination** | No deadlocks, clear state | No genuinely collaborative tasks (e.g. two agents pairing/debating); Planner decides decomposition once — **no re-planning** — if the split is wrong, a single worker must brute-force it |
| **All-in-memory state + JSON persistence** | Simple, fast single-replica | Single point, not horizontally scalable; restart loses in-flight runs (only finished runs persisted) |

**The two most critical gaps**:

1. **No integration/merge stage**: parallel workers each pass on their branch
   but the branches are never merged and never integration-tested together. In
   multi-subtask runs, "all green" can be an illusion.
2. **Danger detection is post-hoc**: under `acceptEdits` the command has already
   executed; the authorization gate can only block *subsequent* iterations, not
   the damage already done.

---

## 8. Possible Improvements

**A. Add an integration stage (highest priority)**
Introduce an **Integrator** role after all workers pass: merge each `orch/*`
branch into an integration branch in turn, run a full Gate 1 (build+test+e2e),
and on failure feed conflicts/integration errors back to the relevant workers.
This directly closes the "false all-green" gap.

**B. Make danger detection a true pre-execution gate**
Instead of `acceptEdits`, use the Claude CLI's `--permission-prompt-tool` / MCP
permission callback to intercept Bash commands **before** execution and route
through `Await` for authorization. Demote the current post-hoc scan to an audit
log for forensics.

**C. Support a dependency DAG rather than flat parallelism**
Add `depends_on: []id` to the Planner's `emit_plan` schema; the scheduler runs
in topological batches, and downstream workers branch worktrees off already-
merged upstream work. The current "must be independent" constraint limits the
kinds of tasks the system can handle.

**D. Structured feedback instead of plain text**
Preserve structure in Gate/Verdict feedback (which specific acceptance items
failed, file+line, failing test names) and present it as a structured checklist
in the worker prompt — reducing information loss and speeding convergence.

**E. Allow re-planning**
When a worker fails Gate 2 for several rounds with a stable, unchanging
`Missing` list, the subtask boundary itself is likely wrong. Call back into the
Planner to re-decompose that subtask rather than letting one worker burn its
whole budget.

**F. Give the Verifier runtime evidence**
Today the Verifier only sees a static diff+gate text. Let it request specific
verification commands (via a restricted tool), or feed it actual e2e
output/screenshots, to reduce "diff looks right but behavior is wrong"
misjudgments.

**G. Externalize state for restart recovery**
Persist in-flight run state (current iteration, feedback, worktree paths) too,
so the server can resume after a restart rather than losing the run. Currently
only finished runs are persisted.

---

## 9. Sub-Agents: flat three-role team, not a recursive subagent tree

This design *does* use sub-agents, but not in the Claude Code sense of a
recursive in-process Task/subagent tree. Instead it uses a **flat, three-role
"virtual team"** where the only true sub-agent is the Worker, spawned as an OS
subprocess.

| Role | Is it a sub-agent? | Mechanism |
|------|--------------------|-----------|
| **Planner** | No — a single API call | Anthropic API + forced `emit_plan` tool; splits the task into `[]Subtask` |
| **Worker** | **Yes — a real OS subprocess** | `exec.CommandContext(ctx, "claude", "-p", <prompt>, "--output-format", "stream-json", "--permission-mode", "acceptEdits", "--model", <model>)` (worker.go), one per subtask |
| **Verifier** | No — a single API call | Anthropic API + forced `emit_verdict` tool, on a **different model** |

Key properties:

- **No recursion**: roles are non-recursive and responsibility-separated. A
  Worker never spawns its own sub-workers; the Planner decomposes exactly once.
- **Process-level isolation**: each Worker sub-agent forks the `claude` CLI
  headless and runs inside its own git worktree + branch, so N workers run in
  true parallel without stepping on each other.
- **Bounded parallelism**: workers are dispatched via a
  `semaphore (chan struct{}) + sync.WaitGroup`; `results[idx]` is written by
  index (lock-free).
- **Nested-guard stripping**: `envWithoutNestedGuard()` removes
  `CLAUDECODE` / `CLAUDE_CODE_ENTRYPOINT` so a child `claude -p` can launch even
  when agent-beacon itself was started from within a Claude session.

> In short: **one layer of sub-agents (Workers as subprocesses)**, no recursive
> subagent tree, and workers never talk to each other.

---

## 10. Agent Review: structure and order (the two-gate self-repair loop)

Review runs as **two sequential gates**, strictly unidirectional and
**fail-fast**. Every failure serializes its reason into the `feedback` string
and `continue`s back to the top of the loop
(`for i := 1; i <= maxIters+extraIters; i++` in worker.go).

### Per-iteration review order

```
iteration i:
  (a) run Worker sub-agent (claude -p)              worker.go:83
        ├─ error → feedback=err; classify(env/logic); continue ↺
        └─ scan Bash tool_use → collect DangerHit
  (b) danger authorization (if any hits)            worker.go:101
        └─ Await(human auth); deny/timeout → subtask ends safely
  ┌──────────────────── review begins ────────────────────┐
  (c) GATE 1 — deterministic (build → check → test) worker.go:111
        ├─ machine-run, fail-fast, no model involved
        └─ fail → feedback="Deterministic gates failed…"; continue ↺
  (d) capture diff (git add -N to include new files) worker.go:127
  (e) GATE 2 — cross-model review (Verifier)         worker.go:130
        ├─ model DIFFERENT from Worker (e.g. Worker=Sonnet, Verifier=Opus)
        ├─ forced emit_verdict tool → {pass, reasons[], missing[]}
        ├─ pass → res.Passed=true, RETURN ✓ (the ONLY success exit)
        └─ fail → feedback="An independent reviewer judged…
                            Reasons:… Missing:…"; continue ↺
  └─────────────────────────────────────────────────────────┘
  budget exhausted on last iter → requestGuidance() → optional human
                                  guidance (extra ≤ maxExtraGuidedIters=3)
```

### The two gates contrasted

**Gate 1 — deterministic review** (`internal/verify/gates.go`)

- **No model involved** — pure machine execution: detect languages → assemble
  build/test/e2e commands (honoring any `TestCmd`/`BuildCmd`/`E2ECmd` override)
  → `RunGates` **fail-fast** (a broken build skips tests, saving compute).
- Fail → the gate output is fed back as natural-language feedback for the next
  iteration.

**Gate 2 — cross-model review** (`verifier.go`)

- **Must use a different model** than the Worker, preventing a model from
  rubber-stamping its own work.
- The system prompt is deliberately strict (verifier.go:62):
  > *"You are an independent code reviewer. You did NOT write this code. Judge
  > strictly whether the stated goal and every acceptance criterion are
  > demonstrably satisfied… **Do not give the benefit of the doubt.** Call the
  > emit_verdict tool exactly once."*
- The Verifier sees **only three things**: the subtask Goal + Acceptance
  criteria, the code **diff**, and the Gate 1 output. It **cannot see the
  Worker's conversation and cannot run code** — it judges the deliverable like a
  real code review.
- Output is forced into structured JSON via `tool_choice`:
  `{ verdict: pass|fail, reasons: [], missing: [] }`. The `verdict` enum permits
  only `pass`/`fail`, and `pass` requires the goal **and all** acceptance
  criteria to be demonstrably met.
- `flexStrings` tolerates models (notably Opus) that sometimes emit
  `reasons`/`missing` as a bare string instead of an array.

### Ordering invariants

1. **Unidirectional progression**: Worker → (danger auth) → Gate 1 → Gate 2.
   Any failed step `continue`s to the top with its reason written into
   `feedback` — the only iterative A2A channel.
2. **Single success exit**: only a Gate 2 (Verifier) `pass` does `return res`
   (Passed=true). Passing Gate 1 merely *admits* the work to Gate 2.
3. **Two lines of defense**: Gate 1 proves "it compiles and tests are green"
   (objective); Gate 2 proves "the goal is actually met" (subjective but
   independent and cross-model). A compilable diff ≠ a satisfied requirement.
4. **Budget guardrail**: base budget `MaxIters`; when exhausted, the last
   iteration may request human guidance, each grant awarding extra iterations,
   hard-capped by `maxExtraGuidedIters = 3` — so the review loop can never run
   forever.

### Known review limitation

Gate 2 sees only a **static diff + gate text**, never **runtime behavior** — so
it can misjudge ("diff looks right but the e2e scenario is uncovered").
Additionally, in multi-subtask runs each branch passes Gate 2 independently but
there is **no merge / integration review stage**, so "all green" can be an
illusion (see §7 and improvement A).
