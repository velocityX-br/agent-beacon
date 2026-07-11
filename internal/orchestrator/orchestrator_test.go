package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/local/agent-beacon/internal/anthropic"
	"github.com/local/agent-beacon/internal/verify"
)

// stubMessenger returns queued responses in order, recording the requests it
// received so tests can assert on prompt content.
type stubMessenger struct {
	responses []anthropic.Response
	err       error
	requests  []anthropic.Request
}

func (s *stubMessenger) Messages(_ context.Context, req anthropic.Request) (anthropic.Response, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return anthropic.Response{}, s.err
	}
	if len(s.responses) == 0 {
		return anthropic.Response{}, nil
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}

// toolResp builds a Response containing a single tool_use block with the given
// name and JSON input.
func toolResp(name string, input any) anthropic.Response {
	b, _ := json.Marshal(input)
	return anthropic.Response{
		StopReason: "tool_use",
		Content: []anthropic.ContentBlock{
			{Type: "tool_use", Name: name, Input: b},
		},
	}
}

func TestPlanParsesSubtasks(t *testing.T) {
	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(planToolName, map[string]any{
			"subtasks": []map[string]any{
				{"goal": "Fix the failing test", "branch": "Fix Test!", "acceptance": []string{"go test passes"}},
				{"goal": "Add a new test", "branch": "fix-test", "acceptance": []string{"coverage up"}},
			},
		}),
	}}
	cfg := Config{RepoDir: "/repo", Task: "do things", PlannerModel: "planner"}
	subs, err := Plan(context.Background(), cfg, m)
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subtasks, got %d", len(subs))
	}
	if subs[0].Branch != "orch/fix-test" {
		t.Errorf("branch slugify wrong: %q", subs[0].Branch)
	}
	// Second subtask had a colliding slug -> must be uniquified.
	if subs[1].Branch == subs[0].Branch {
		t.Errorf("expected unique branches, both = %q", subs[0].Branch)
	}
	// Forced tool_choice must have been requested.
	if m.requests[0].ToolChoice == nil || m.requests[0].ToolChoice.Name != planToolName {
		t.Errorf("planner did not force emit_plan tool_choice: %+v", m.requests[0].ToolChoice)
	}
}

func TestPlanEmptyIsError(t *testing.T) {
	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(planToolName, map[string]any{"subtasks": []map[string]any{}}),
	}}
	if _, err := Plan(context.Background(), Config{}, m); err == nil {
		t.Fatal("expected error for empty plan")
	}
}

func TestVerifyParsesVerdict(t *testing.T) {
	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(verdictToolName, map[string]any{
			"verdict": "fail",
			"reasons": []string{"missing tests"},
			"missing": []string{"add unit test for foo"},
		}),
	}}
	cfg := Config{VerifierModel: "verifier"}
	st := Subtask{Goal: "g", Acceptance: []string{"a"}}
	v, err := Verify(context.Background(), cfg, st, "diff", "gate out", m)
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if v.Pass {
		t.Errorf("expected fail verdict")
	}
	if len(v.Reasons) != 1 || v.Reasons[0] != "missing tests" {
		t.Errorf("reasons wrong: %+v", v.Reasons)
	}
	if m.requests[0].ToolChoice == nil || m.requests[0].ToolChoice.Name != verdictToolName {
		t.Errorf("verifier did not force emit_verdict tool_choice")
	}
	// The verifier prompt must contain the goal and gate output.
	prompt := m.requests[0].Messages[0].Content
	if !strings.Contains(prompt, "gate out") || !strings.Contains(prompt, "g") {
		t.Errorf("verifier prompt missing context: %q", prompt)
	}
}

func TestWorkerStopsOnFirstPass(t *testing.T) {
	restore := swapSeams(
		func(_ context.Context, _, _, _ string) (string, error) { return "/wt", nil },
		func(_ context.Context, _ Config, _ string, _ Subtask, _ string) error { return nil },
		func(_ context.Context, _ Config, _ string) []verify.GateResult {
			return []verify.GateResult{{Name: "test", Passed: true}}
		},
		func(_ context.Context, _, _ string) string { return "diff" },
	)
	defer restore()

	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(verdictToolName, map[string]any{"verdict": "pass", "reasons": []string{"looks good"}}),
	}}
	res := RunWorker(context.Background(), Config{MaxIters: 4}, Subtask{Goal: "g", Branch: "orch/x"}, m)
	if !res.Passed {
		t.Fatalf("expected pass, got %+v", res)
	}
	if res.Iterations != 1 {
		t.Errorf("expected to stop after 1 iteration, got %d", res.Iterations)
	}
}

func TestWorkerFeedsGateOutputIntoRepairPrompt(t *testing.T) {
	gateCalls := 0
	agentPrompts := []string{}
	restore := swapSeams(
		func(_ context.Context, _, _, _ string) (string, error) { return "/wt", nil },
		func(_ context.Context, _ Config, _ string, _ Subtask, feedback string) error {
			agentPrompts = append(agentPrompts, feedback)
			return nil
		},
		func(_ context.Context, _ Config, _ string) []verify.GateResult {
			gateCalls++
			if gateCalls == 1 {
				return []verify.GateResult{{Name: "test", Passed: false, Output: "FAIL: TestFoo", Err: "exit 1"}}
			}
			return []verify.GateResult{{Name: "test", Passed: true}}
		},
		func(_ context.Context, _, _ string) string { return "diff" },
	)
	defer restore()

	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(verdictToolName, map[string]any{"verdict": "pass", "reasons": []string{"ok"}}),
	}}
	res := RunWorker(context.Background(), Config{MaxIters: 4}, Subtask{Goal: "g", Branch: "orch/x"}, m)
	if !res.Passed {
		t.Fatalf("expected eventual pass, got %+v", res)
	}
	if res.Iterations != 2 {
		t.Errorf("expected 2 iterations (fail then pass), got %d", res.Iterations)
	}
	// The second agent invocation must have received the gate failure output.
	if len(agentPrompts) < 2 || !strings.Contains(agentPrompts[1], "FAIL: TestFoo") {
		t.Errorf("gate output not fed into repair prompt: %+v", agentPrompts)
	}
}

func TestWorkerExhaustsBudget(t *testing.T) {
	restore := swapSeams(
		func(_ context.Context, _, _, _ string) (string, error) { return "/wt", nil },
		func(_ context.Context, _ Config, _ string, _ Subtask, _ string) error { return nil },
		func(_ context.Context, _ Config, _ string) []verify.GateResult {
			return []verify.GateResult{{Name: "test", Passed: true}}
		},
		func(_ context.Context, _, _ string) string { return "" },
	)
	defer restore()

	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(verdictToolName, map[string]any{"verdict": "fail", "reasons": []string{"nope"}}),
		toolResp(verdictToolName, map[string]any{"verdict": "fail", "reasons": []string{"nope"}}),
	}}
	res := RunWorker(context.Background(), Config{MaxIters: 2}, Subtask{Goal: "g", Branch: "orch/x"}, m)
	if res.Passed {
		t.Fatalf("expected fail after budget exhausted")
	}
	if res.Iterations != 2 {
		t.Errorf("expected 2 iterations, got %d", res.Iterations)
	}
}

// swapSeams overrides the worker's external seams and returns a restore func.
func swapSeams(
	cw func(context.Context, string, string, string) (string, error),
	ra func(context.Context, Config, string, Subtask, string) error,
	rg func(context.Context, Config, string) []verify.GateResult,
	wd func(context.Context, string, string) string,
) func() {
	oc, or, og, ow := createWorktreeFn, runAgentFn, runGatesFn, worktreeDiffFn
	createWorktreeFn = cw
	runAgentFn = ra
	runGatesFn = rg
	worktreeDiffFn = wd
	return func() {
		createWorktreeFn, runAgentFn, runGatesFn, worktreeDiffFn = oc, or, og, ow
	}
}
