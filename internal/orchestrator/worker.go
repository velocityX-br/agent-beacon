package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/local/agent-beacon/internal/agent"
	"github.com/local/agent-beacon/internal/anthropic"
	"github.com/local/agent-beacon/internal/verify"
)

// The following seams are overridable in tests so the worker loop can be
// exercised without a real git worktree, coding-agent binary, or shell. In
// production they delegate to the real implementations.
var (
	createWorktreeFn = agent.CreateWorktree
	runAgentFn       = runCodingAgent
	runGatesFn       = runGates
	worktreeDiffFn   = worktreeDiff
)

// runGates detects the worktree's languages, builds the gate set (honoring any
// command overrides), and runs them fail-fast.
func runGates(ctx context.Context, cfg Config, wt string) []verify.GateResult {
	langs := verify.Detect(wt)
	gates := verify.GatesFromLanguages(wt, langs, cfg.BuildCmd, cfg.TestCmd, cfg.E2ECmd)
	return verify.RunGates(ctx, gates, cfg.GateTimeout)
}

// RunWorker executes one subtask to completion (or budget exhaustion) inside an
// isolated git worktree, running the headless coding agent, then deterministic
// gates, then the cross-model verifier — feeding failures back as repair
// feedback on the next iteration.
func RunWorker(ctx context.Context, cfg Config, st Subtask, m anthropic.Messenger) WorkerResult {
	res := WorkerResult{Subtask: st, Branch: st.Branch}

	wt, err := createWorktreeFn(ctx, cfg.RepoDir, st.Branch, cfg.WorktreeLocation)
	if err != nil {
		res.Error = fmt.Sprintf("worktree: %v", err)
		return res
	}
	res.WorktreePath = wt

	maxIters := cfg.MaxIters
	if maxIters < 1 {
		maxIters = 1
	}

	var feedback string
	for i := 1; i <= maxIters; i++ {
		res.Iterations = i
		res.Log = append(res.Log, fmt.Sprintf("iteration %d: running coding agent", i))

		if err := runAgentFn(ctx, cfg, wt, st, feedback); err != nil {
			res.Log = append(res.Log, "agent error: "+err.Error())
			// An agent failure is recoverable — feed it back and retry.
			feedback = "The previous coding attempt failed to run: " + err.Error()
			continue
		}

		// Gate 1: deterministic build/test/e2e in the worktree.
		gateResults := runGatesFn(ctx, cfg, wt)
		if !verify.AllPassed(gateResults) {
			gateOut := verify.Summarize(gateResults)
			res.Log = append(res.Log, "gate 1 failed")
			feedback = "Deterministic gates failed. Fix these and continue:\n" + gateOut
			continue
		}
		res.Log = append(res.Log, "gate 1 passed")

		// Gate 2: cross-model verifier judges goal-completion.
		diff := worktreeDiffFn(ctx, cfg.RepoDir, wt)
		gateOut := gateOutputText(gateResults)
		verdict, err := Verify(ctx, cfg, st, diff, gateOut, m)
		if err != nil {
			res.Log = append(res.Log, "verifier error: "+err.Error())
			res.Error = err.Error()
			return res
		}
		if verdict.Pass {
			res.Passed = true
			res.VerdictNotes = verdict.Reasons
			res.Log = append(res.Log, "gate 2 passed (verifier)")
			return res
		}
		res.VerdictNotes = verdict.Reasons
		res.Log = append(res.Log, "gate 2 failed (verifier)")
		feedback = "An independent reviewer judged the work incomplete.\nReasons:\n" +
			bullets(verdict.Reasons) + "\nMissing:\n" + bullets(verdict.Missing)
	}

	res.Log = append(res.Log, "iteration budget exhausted without passing")
	return res
}

// runCodingAgent invokes the real coding agent non-interactively in the
// worktree: `<worker-cmd> -p "<prompt>" --output-format json
// --permission-mode acceptEdits --model <model>`. Output is captured; a
// non-zero exit or malformed JSON becomes an error.
func runCodingAgent(ctx context.Context, cfg Config, workdir string, st Subtask, feedback string) error {
	prompt := buildPrompt(st, feedback)

	bin := cfg.WorkerCmd
	if strings.TrimSpace(bin) == "" {
		bin = "claude"
	}
	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--permission-mode", "acceptEdits",
	}
	if cfg.WorkerModel != "" {
		args = append(args, "--model", cfg.WorkerModel)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workdir
	// The worker must be able to launch `claude -p` even when agent-beacon was
	// itself started from inside a Claude Code session. Claude Code refuses to
	// run nested (guarded by CLAUDECODE / CLAUDE_CODE_ENTRYPOINT), so strip
	// those markers from the child's environment.
	cmd.Env = envWithoutNestedGuard(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	// Best-effort: confirm the CLI emitted a JSON result envelope. We do not
	// hard-fail on non-JSON since some agents stream plain text, but a parse
	// success lets us surface an explicit is_error result.
	var env struct {
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal(out, &env) == nil && env.IsError {
		return fmt.Errorf("agent reported error: %s", firstNonEmpty(env.Subtype, env.Result))
	}
	return nil
}

// envWithoutNestedGuard returns env with the Claude Code nested-session markers
// removed so a child `claude -p` can launch even when the orchestrator was run
// from within a Claude Code session. Without this, the CLI aborts with
// "cannot be launched inside another Claude Code session".
func envWithoutNestedGuard(env []string) []string {
	// env[:0:0] gives a zero-cap slice so the first append allocates a fresh
	// backing array — never mutating the caller's env. Do NOT relax to env[:0]:
	// that would alias and clobber the input.
	out := env[:0:0]
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "CLAUDECODE="),
			strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT="):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// buildPrompt composes the worker prompt from the subtask goal, acceptance
// criteria, and any repair feedback from the previous iteration.
func buildPrompt(st Subtask, feedback string) string {
	var b strings.Builder
	b.WriteString("You are an autonomous coding agent working in an isolated git worktree. ")
	b.WriteString("Implement the following goal fully, then ensure the project builds and all tests pass.\n\n")
	writeGoal(&b, st)
	if strings.TrimSpace(feedback) != "" {
		b.WriteString("REPAIR FEEDBACK from the previous attempt — address ALL of this:\n")
		b.WriteString(feedback)
		b.WriteString("\n")
	}
	return b.String()
}

// worktreeDiff returns the diff of the worktree against the repo's HEAD so the
// verifier can review the actual changes. Best-effort: returns "" on error.
func worktreeDiff(ctx context.Context, repo, wt string) string {
	// Include both committed-vs-HEAD and unstaged changes in the worktree.
	cmd := exec.CommandContext(ctx, "git", "-C", wt, "diff", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gateOutputText concatenates gate outputs for the verifier context.
func gateOutputText(results []verify.GateResult) string {
	var b strings.Builder
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "[%s] %s\n", status, r.Name)
		if r.Output != "" {
			b.WriteString(r.Output + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func bullets(items []string) string {
	if len(items) == 0 {
		return "  (none)\n"
	}
	var b strings.Builder
	for _, it := range items {
		b.WriteString("  - " + it + "\n")
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
