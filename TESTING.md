# Testing Guide — agent-beacon Orchestrator

This guide covers the orchestrator loop (`agent-beacon orchestrate`): its offline
unit tests, an end-to-end manual run against a real repo, and negative/boundary
checks.

## 1. Automated tests (offline — no network / API key)

The `verify` and `orchestrator` packages are fully unit-tested with a stubbed
`Messenger` and overridable worker seams, so nothing touches the network, git,
or the `claude` binary.

```bash
# Full gate (must be green before considering work done):
make build && make vet && make test

# Just the orchestrator-related packages:
go test ./internal/verify/... ./internal/orchestrator/... -v

# Race detector (validates the parallel worker fan-out is data-race free):
go test -race ./internal/orchestrator/...
```

### Coverage

| Package | Test | Verifies |
|---|---|---|
| `verify` | `TestDetectGo/Node/Python/Rust` | marker files map to the right build/test/vet command sets |
| `verify` | `TestDetectNodeBuildScriptGating` | no build gate when `scripts.build` is absent |
| `verify` | `TestRunGatesPassAndFail` | fail-fast: stops after the failing gate (only 2 results) |
| `verify` | `TestRunGatesCapturesOutput` | captures stdout/stderr and a non-empty `Err` |
| `verify` | `TestGatesFromLanguagesOverrides` | `--build/test/e2e-cmd` fully replace detected commands |
| `orchestrator` | `TestPlanParsesSubtasks` | forced `tool_choice`; branch slugify → `orch/x`; collision uniquified |
| `orchestrator` | `TestPlanEmptyIsError` | empty plan is an error |
| `orchestrator` | `TestVerifyParsesVerdict` | parses `emit_verdict`; prompt contains goal + gate output |
| `orchestrator` | `TestWorkerStopsOnFirstPass` | stops on first pass (1 iteration) |
| `orchestrator` | `TestWorkerFeedsGateOutputIntoRepairPrompt` | gate-failure output fed back into the next repair prompt |
| `orchestrator` | `TestWorkerExhaustsBudget` | fails after the iteration budget is exhausted |

## 2. End-to-end manual test (requires `ANTHROPIC_API_KEY` + a logged-in `claude` CLI)

### 2.1 Build

```bash
make build   # produces bin/agent-beacon
```

### 2.2 Prepare a small scratch repo (Go example with one failing test)

```bash
mkdir -p /tmp/scratch-repo && cd /tmp/scratch-repo
git init -q
cat > go.mod <<'EOF'
module example.com/scratch

go 1.25
EOF
cat > add.go <<'EOF'
package scratch

func Add(a, b int) int { return a - b } // intentionally wrong
EOF
cat > add_test.go <<'EOF'
package scratch

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add(2,3) = %d, want 5", Add(2, 3))
	}
}
EOF
git add -A && git commit -qm "seed with failing test"
```

### 2.3 Run the orchestrator

```bash
export ANTHROPIC_API_KEY=sk-...

bin/agent-beacon orchestrate \
  --task "make all unit tests pass and add one more test for the fixed function" \
  --repo /tmp/scratch-repo \
  --workers 2 --max-iters 3 \
  --worker-model claude-sonnet-4-6 \
  --verifier-model claude-opus-4-6 \
  --report /tmp/orch-report.json \
  --timeout 15m
```

### 2.4 What to observe (acceptance)

- **Worktree isolation** — `git -C /tmp/scratch-repo worktree list` shows sibling
  directories on `orch/<slug>` branches.
- **Self-repair loop** — each worker logs `iteration N` → `gate 1` →
  `gate 2 (verifier)`; the first `go test` failure is fed back and repaired on the
  next iteration.
- **Cross-model check** — the verifier uses a *different* model than the worker
  (Opus vs Sonnet); if they match, a `warning: ... cross-model check is weakened`
  line is printed.
- **Determinism before LLM** — Gate 1 (build/test/e2e) must pass before a verifier
  call is spent.
- **Stop condition** — stops on pass, or when `--max-iters` is exhausted.

### 2.5 Inspect the report

```bash
cat /tmp/orch-report.json | jq
```

Should include, per subtask: `passed`, `iterations`, `branch`, `worktreePath`,
`verdictNotes`, `log`; plus top-level `passed` / `total` / timestamps.

## 3. Boundary / negative tests

```bash
# Missing --task -> error
bin/agent-beacon orchestrate --repo /tmp/scratch-repo
# => "--task is required"

# No API key (clear env + no flag + no config)
env -u ANTHROPIC_API_KEY bin/agent-beacon orchestrate --task "x" --repo /tmp/scratch-repo
# => "no Anthropic API key ..."

# Non-directory repo
bin/agent-beacon orchestrate --task "x" --repo /tmp/does-not-exist
# => "--repo is not a directory: ..."

# worker == verifier triggers a warning
bin/agent-beacon orchestrate --task "x" --repo /tmp/scratch-repo \
  --worker-model claude-sonnet-4-6 --verifier-model claude-sonnet-4-6
# => stderr: "warning: worker and verifier use the same model ..."

# Command overrides (any language / product)
bin/agent-beacon orchestrate --task "x" --repo /path/to/repo \
  --build-cmd "make build" --test-cmd "make test" --e2e-cmd "make e2e"

# Wall-clock budget
bin/agent-beacon orchestrate --task "x" --repo /tmp/scratch-repo --timeout 30s
```

## 4. Cleanup

```bash
cd /tmp/scratch-repo
git worktree list
git worktree remove <path> --force   # remove each worker worktree
git branch -D orch/<slug>            # delete the branch
rm -rf /tmp/scratch-repo /tmp/orch-report.json
```

## Notes

- The unit tests are the authoritative regression gate — CI only needs
  `make build && make vet && make test`, fully offline.
- The end-to-end test **really calls the Anthropic API and lets `claude` modify a
  temporary repo** — always use a throwaway scratch repo, never point it at a real
  project working tree.
- SIGINT/SIGTERM cancels gracefully via `signal.NotifyContext`; on interrupt the
  partial report is still printed.
