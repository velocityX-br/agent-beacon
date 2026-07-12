package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

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

// maxExtraGuidedIters bounds how many additional iterations a single guidance
// nudge may grant when the budget is exhausted, so repeated guidance can never
// loop forever.
const maxExtraGuidedIters = 3

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
func RunWorker(ctx context.Context, cfg Config, st Subtask, m anthropic.Messenger) (res WorkerResult) {
	res = WorkerResult{Subtask: st, Branch: st.Branch}
	res.StartedAt = time.Now()
	// Deferred so timing is recorded on every exit path (worktree fail,
	// verifier error, budget exhausted, pass).
	defer func() {
		res.FinishedAt = time.Now()
		res.DurationMS = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
	}()

	wt, err := createWorktreeFn(ctx, cfg.RepoDir, st.Branch, cfg.WorktreeLocation)
	if err != nil {
		res.Error = fmt.Sprintf("worktree: %v", err)
		emit(cfg, Event{Kind: EventWorkerDone, SubtaskID: st.ID, Branch: st.Branch, Passed: boolPtr(false), Message: "worktree failed", Detail: res.Error})
		return res
	}
	res.WorktreePath = wt
	emit(cfg, Event{Kind: EventWorkerStart, SubtaskID: st.ID, Branch: st.Branch, Message: st.Goal, Detail: wt})

	maxIters := cfg.MaxIters
	if maxIters < 1 {
		maxIters = 1
	}

	// extraIters tracks additional iterations granted by user guidance when the
	// base budget is exhausted, capped by maxExtraGuidedIters.
	var (
		feedback   string
		extraIters int
	)
	for i := 1; i <= maxIters+extraIters; i++ {
		res.Iterations = i
		res.Log = append(res.Log, fmt.Sprintf("iteration %d: running coding agent", i))
		emit(cfg, Event{Kind: EventIteration, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Message: "running coding agent"})

		dangers, err := runAgentFn(ctx, cfg, wt, st, feedback, i)
		if err != nil {
			res.Log = append(res.Log, "agent error: "+err.Error())
			emit(cfg, Event{Kind: EventAgent, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(false), Message: "agent error", Detail: err.Error()})
			// An agent failure is recoverable — feed it back and retry. When
			// the error looks like a missing tool / infra problem, record it as
			// an explicit self-repair action rather than silent feedback.
			feedback = "The previous coding attempt failed to run: " + err.Error()
			if classifyFailure(err.Error()) == "environment" {
				recordSelfRepair(cfg, &res, i, "agent environment failure", err.Error())
			}
			continue
		}
		emit(cfg, Event{Kind: EventAgent, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(true), Message: "coding agent finished"})

		// If the agent invoked any dangerous operation this iteration, pause
		// for explicit user authorization before proceeding. A denial (or
		// timeout) ends the subtask safely.
		if len(dangers) > 0 {
			if !authorizeDangers(ctx, cfg, &res, st, i, dangers) {
				res.Error = "denied dangerous operation"
				res.Log = append(res.Log, "run stopped: dangerous operation denied")
				emit(cfg, Event{Kind: EventWorkerDone, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(false), Message: "dangerous operation denied"})
				return res
			}
		}

		// Gate 1: deterministic build/test/e2e in the worktree.
		gateResults := runGatesFn(ctx, cfg, wt)
		res.Gates = gateResults
		if !verify.AllPassed(gateResults) {
			gateOut := verify.Summarize(gateResults)
			res.Log = append(res.Log, "gate 1 failed")
			emit(cfg, Event{Kind: EventGate, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(false), Message: "gate 1 failed", Detail: gateOut})
			feedback = "Deterministic gates failed. Fix these and continue:\n" + gateOut
			if classifyFailure(gateOut) == "environment" {
				recordSelfRepair(cfg, &res, i, "gate environment failure", gateOut)
			}
			continue
		}
		res.Log = append(res.Log, "gate 1 passed")
		emit(cfg, Event{Kind: EventGate, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(true), Message: "gate 1 passed"})

		// Gate 2: cross-model verifier judges goal-completion.
		diff := worktreeDiffFn(ctx, cfg.RepoDir, wt)
		res.Artifacts = changedFilesFromDiff(diff)
		gateOut := gateOutputText(gateResults)
		verdict, err := Verify(ctx, cfg, st, diff, gateOut, m)
		if err != nil {
			res.Log = append(res.Log, "verifier error: "+err.Error())
			res.Error = err.Error()
			emit(cfg, Event{Kind: EventWorkerDone, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(false), Message: "verifier error", Detail: err.Error()})
			return res
		}
		if verdict.Pass {
			res.Passed = true
			res.VerdictNotes = verdict.Reasons
			res.Log = append(res.Log, "gate 2 passed (verifier)")
			emit(cfg, Event{Kind: EventVerdict, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(true), Message: "gate 2 passed (verifier)", Detail: strings.Join(verdict.Reasons, "; ")})
			emit(cfg, Event{Kind: EventWorkerDone, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(true), Message: "subtask passed"})
			return res
		}
		res.VerdictNotes = verdict.Reasons
		res.Log = append(res.Log, "gate 2 failed (verifier)")
		emit(cfg, Event{Kind: EventVerdict, SubtaskID: st.ID, Branch: st.Branch, Iteration: i, Passed: boolPtr(false), Message: "gate 2 failed (verifier)", Detail: strings.Join(verdict.Reasons, "; ")})
		feedback = "An independent reviewer judged the work incomplete.\nReasons:\n" +
			bullets(verdict.Reasons) + "\nMissing:\n" + bullets(verdict.Missing)

		// On the final scheduled iteration, offer the user a chance to steer
		// the run with guidance rather than giving up immediately. Accepted
		// guidance grants a bounded number of extra iterations.
		if i == maxIters+extraIters && extraIters < maxExtraGuidedIters {
			if g, ok := requestGuidance(ctx, cfg, &res, st, i); ok {
				feedback = "User guidance — follow this precisely:\n" + g + "\n\n" + feedback
				extraIters++
			}
		}
	}

	res.Log = append(res.Log, "iteration budget exhausted without passing")
	emit(cfg, Event{Kind: EventWorkerDone, SubtaskID: st.ID, Branch: st.Branch, Iteration: res.Iterations, Passed: boolPtr(false), Message: "iteration budget exhausted without passing"})
	return res
}

// recordSelfRepair logs a recoverable environment/infra failure the loop is
// auto-fixing by feeding it back for another iteration, emitting an
// EventSelfRepair and appending a RepairRecord to the result for the report.
func recordSelfRepair(cfg Config, res *WorkerResult, iter int, reason, detail string) {
	action := "fed failure back as repair feedback"
	res.Repairs = append(res.Repairs, RepairRecord{
		Iteration: iter,
		Kind:      "environment",
		Reason:    reason,
		Action:    action,
		Time:      time.Now(),
	})
	res.Log = append(res.Log, "self-repair: "+reason)
	emit(cfg, Event{Kind: EventSelfRepair, SubtaskID: res.Subtask.ID, Branch: res.Branch, Iteration: iter, Message: reason, Detail: truncate(detail, 300)})
}

// authorizeDangers pauses the run for user authorization of the dangerous
// operations observed this iteration. It emits EventAuthNeeded, blocks on the
// Intervener, records the decision, and returns whether to proceed. A nil
// Intervener (headless CLI) auto-denies.
func authorizeDangers(ctx context.Context, cfg Config, res *WorkerResult, st Subtask, iter int, dangers []DangerHit) bool {
	reqID := newReqID()
	rules := make([]string, 0, len(dangers))
	var detail strings.Builder
	for _, d := range dangers {
		rules = append(rules, d.Rule)
		fmt.Fprintf(&detail, "[%s] %s\n", d.Rule, truncate(d.Command, 200))
	}
	summary := "Dangerous operation detected: " + strings.Join(rules, ", ")
	emit(cfg, Event{Kind: EventAuthNeeded, SubtaskID: st.ID, Branch: st.Branch, Iteration: iter, ReqID: reqID, Message: summary, Detail: strings.TrimSpace(detail.String())})

	dec := awaitDecision(ctx, cfg, InterventionRequest{
		ReqID:     reqID,
		SubtaskID: st.ID,
		Branch:    st.Branch,
		Kind:      "auth",
		Summary:   summary,
		Detail:    strings.TrimSpace(detail.String()),
	})
	res.Interventions = append(res.Interventions, InterventionRecord{
		ReqID:    reqID,
		Kind:     "auth",
		Summary:  summary,
		Approved: dec.Approve,
		Guidance: dec.Guidance,
		Time:     time.Now(),
	})
	if dec.Approve && strings.TrimSpace(dec.Guidance) != "" {
		res.Log = append(res.Log, "authorized with guidance")
	}
	return dec.Approve
}

// requestGuidance pauses the run to offer optional user guidance when the
// iteration budget is exhausted. It emits EventInputNeeded, blocks on the
// Intervener, records the decision, and returns the guidance text (and whether
// any was supplied). A nil Intervener (headless CLI) supplies none.
func requestGuidance(ctx context.Context, cfg Config, res *WorkerResult, st Subtask, iter int) (string, bool) {
	reqID := newReqID()
	summary := "Iteration budget exhausted — provide guidance to continue, or skip to finish."
	emit(cfg, Event{Kind: EventInputNeeded, SubtaskID: st.ID, Branch: st.Branch, Iteration: iter, ReqID: reqID, Message: summary})

	dec := awaitDecision(ctx, cfg, InterventionRequest{
		ReqID:     reqID,
		SubtaskID: st.ID,
		Branch:    st.Branch,
		Kind:      "input",
		Summary:   summary,
	})
	guidance := strings.TrimSpace(dec.Guidance)
	if guidance == "" {
		return "", false
	}
	res.Interventions = append(res.Interventions, InterventionRecord{
		ReqID:    reqID,
		Kind:     "input",
		Summary:  summary,
		Approved: true,
		Guidance: guidance,
		Time:     time.Now(),
	})
	res.Log = append(res.Log, "user guidance granted extra iteration")
	return guidance, true
}

// newReqID returns a short random hex id correlating a pause event with the
// browser's response on /respond.
func newReqID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-based id; collisions are harmless (the pending
		// map is per-run and short-lived).
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(b[:])
}

// runCodingAgent invokes the real coding agent non-interactively in the
// worktree: `<worker-cmd> -p "<prompt>" --output-format stream-json --verbose
// --permission-mode acceptEdits --model <model>`. Output is streamed line by
// line; each meaningful step is summarized and emitted as an EventLog so the
// browser's Live Log shows Claude's real-time progress. A non-zero exit or an
// is_error result envelope becomes an error (recoverable → repair feedback).
func runCodingAgent(ctx context.Context, cfg Config, workdir string, st Subtask, feedback string, iter int) ([]DangerHit, error) {
	prompt := buildPrompt(st, feedback)

	bin := cfg.WorkerCmd
	if strings.TrimSpace(bin) == "" {
		bin = "claude"
	}
	// stream-json emits NDJSON (one JSON object per line); Claude requires
	// --verbose alongside stream-json under -p.
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
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

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %v", err)
	}
	// Capture stderr into a bounded tail buffer for the error message.
	var stderr tailBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %v", err)
	}

	// Scan NDJSON stdout, summarizing each line into a Live Log entry. Raise
	// the scanner buffer so large tool payloads/diffs aren't truncated past the
	// default 64KB line cap.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var (
		resultErr     bool
		resultSubtype string
		resultText    string
		dangers       []DangerHit
		seenDanger    = map[string]bool{}
	)
	for scanner.Scan() {
		line := scanner.Bytes()
		text, isErr, isResult, ok := summarizeStreamLine(line)
		if isResult {
			resultErr = isErr
			resultSubtype, resultText = parseResultEnvelope(line)
		}
		// Heuristically scan Bash tool_use commands for destructive patterns.
		// Because the agent runs under acceptEdits it may already have executed
		// the command; danger hits gate continuation between iterations (see
		// RunWorker) rather than blocking mid-invocation.
		for _, hit := range scanLineForDanger(line) {
			key := hit.Rule + "\x00" + hit.Command
			if seenDanger[key] {
				continue
			}
			seenDanger[key] = true
			dangers = append(dangers, hit)
		}
		if ok && text != "" {
			emit(cfg, Event{Kind: EventLog, SubtaskID: st.ID, Branch: st.Branch, Iteration: iter, Message: text})
		}
	}

	if err := cmd.Wait(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		return dangers, fmt.Errorf("%v: %s", err, tail)
	}
	if resultErr {
		return dangers, fmt.Errorf("agent reported error: %s", firstNonEmpty(resultSubtype, resultText))
	}
	return dangers, nil
}

// scanLineForDanger parses one stream-json NDJSON line and returns any
// dangerous Bash commands found in tool_use blocks, matched against the
// conservative denylist. Non-tool, non-Bash, and unparseable lines yield none.
func scanLineForDanger(line []byte) []DangerHit {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}
	if ev.Type != "assistant" {
		return nil
	}
	var out []DangerHit
	for _, c := range ev.Message.Content {
		if c.Type != "tool_use" || !strings.EqualFold(c.Name, "Bash") {
			continue
		}
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(c.Input, &in) != nil {
			continue
		}
		if matched, rule := scanDangerous(in.Command); matched {
			out = append(out, DangerHit{Rule: rule, Command: strings.TrimSpace(in.Command)})
		}
	}
	return out
}

// parseResultEnvelope extracts the subtype/result strings from a
// type:"result" stream-json line for the error message.
func parseResultEnvelope(line []byte) (subtype, result string) {
	var env struct {
		Subtype string `json:"subtype"`
		Result  string `json:"result"`
	}
	_ = json.Unmarshal(line, &env)
	return env.Subtype, env.Result
}

// summarizeStreamLine parses one stream-json NDJSON line and returns a concise,
// human-readable summary for the Live Log. isError/isResult report whether the
// line was the final result envelope and whether it flagged an error; ok is
// false for blank/unknown lines that should be skipped.
func summarizeStreamLine(line []byte) (text string, isError, isResult, ok bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return "", false, false, false
	}
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Model   string `json:"model"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false, false, false
	}

	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			if ev.Model != "" {
				return "agent: session started (model " + ev.Model + ")", false, false, true
			}
			return "agent: session started", false, false, true
		}
		return "", false, false, false
	case "assistant":
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					return "agent: " + truncate(t, 200), false, false, true
				}
			case "tool_use":
				return "agent: tool " + c.Name + summarizeToolInput(c.Input), false, false, true
			}
		}
		return "", false, false, false
	case "user":
		// tool_result blocks come back on user-type events.
		for _, c := range ev.Message.Content {
			if c.Type == "tool_result" {
				return "agent: ← tool result", false, false, true
			}
		}
		return "", false, false, false
	case "result":
		return "agent: result " + firstNonEmpty(ev.Subtype, "done"), ev.IsError, true, true
	default:
		return "", false, false, false
	}
}

// summarizeToolInput produces a compact hint from a tool_use input object,
// e.g. " path=main.go" for Edit/Write or " cmd=go test ./..." for Bash.
func summarizeToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "command", "pattern", "url"} {
		if v, ok := in[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				label := key
				switch key {
				case "file_path", "path":
					label = "path"
				case "command":
					label = "cmd"
				}
				return " " + label + "=" + truncate(s, 120)
			}
		}
	}
	return ""
}

// truncate shortens s to at most n characters, appending an ellipsis when cut,
// and collapses newlines so a single log line stays on one line.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tailBuffer is an io.Writer that retains only the last maxTail bytes written,
// so a large stderr stream is bounded when surfaced in an error message.
type tailBuffer struct {
	buf bytes.Buffer
}

const maxTail = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	n, err := t.buf.Write(p)
	if t.buf.Len() > maxTail {
		b := t.buf.Bytes()
		trimmed := b[len(b)-maxTail:]
		var next bytes.Buffer
		next.Write(trimmed)
		t.buf = next
	}
	return n, err
}

func (t *tailBuffer) String() string { return t.buf.String() }

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

// WorktreeDiff exposes the worktree-vs-HEAD diff for callers outside the
// package (e.g. the server's diff endpoint), so the browser sees the same
// changes the verifier reviewed — including brand-new files that a plain
// `git diff HEAD` omits. Best-effort: returns "" on error.
func WorktreeDiff(ctx context.Context, repo, wt string) string {
	return worktreeDiff(ctx, repo, wt)
}

// worktreeDiff returns the diff of the worktree against the repo's HEAD so the
// verifier can review the actual changes. Best-effort: returns "" on error.
func worktreeDiff(ctx context.Context, repo, wt string) string {
	// Workers frequently create brand-new files (e.g. a fresh script + its
	// test). A plain `git diff HEAD` omits untracked files entirely, so the
	// verifier would see an empty diff and (correctly) refuse to confirm the
	// work exists. Intent-to-add (`git add -N`) records new paths without
	// staging content, which makes them appear in the subsequent `git diff
	// HEAD` as additions. We exclude common build artifacts (which repos in a
	// scratch worktree often lack a .gitignore for) so the diff stays focused
	// on source. Failures here are non-fatal: we still fall back to whatever
	// `git diff HEAD` yields.
	excludes := diffArtifactExcludes()
	addArgs := append([]string{"-C", wt, "add", "-N", "--", "."}, excludes...)
	_ = exec.CommandContext(ctx, "git", addArgs...).Run()

	// Include committed-vs-HEAD, unstaged, and now intent-to-add changes,
	// applying the same artifact excludes to the diff itself.
	diffArgs := append([]string{"-C", wt, "diff", "HEAD", "--", "."}, excludes...)
	cmd := exec.CommandContext(ctx, "git", diffArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// diffArtifactExcludes returns git pathspec exclusions for common language
// build artifacts so a worktree diff (and the intent-to-add that precedes it)
// shows source changes rather than generated caches/binaries. These are best
// effort; unmatched patterns simply match nothing.
func diffArtifactExcludes() []string {
	patterns := []string{
		"__pycache__", "*.pyc", ".pytest_cache",
		"node_modules", "dist", "build",
		".venv", "venv", "*.egg-info",
		"target", "vendor",
	}
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, ":(exclude)"+p, ":(exclude)**/"+p)
	}
	return out
}

// changedFilesFromDiff scans a unified diff for `+++ b/<path>` header lines and
// returns the changed file paths in first-seen order, de-duplicated. New files
// whose `+++` target is /dev/null (deletions) are skipped. This reuses the diff
// already computed for the verifier rather than issuing a separate git call.
func changedFilesFromDiff(diff string) []string {
	if strings.TrimSpace(diff) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "+++ ") {
			continue
		}
		target := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
		if target == "/dev/null" {
			continue
		}
		target = strings.TrimPrefix(target, "b/")
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
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
