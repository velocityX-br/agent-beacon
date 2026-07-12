package orchestrator

import (
	"context"
	"sync"
	"testing"

	"github.com/local/agent-beacon/internal/anthropic"
	"github.com/local/agent-beacon/internal/verify"
)

// captureEmitter records the ordered kinds of events it receives. It is safe
// for concurrent use since workers may emit from goroutines.
type captureEmitter struct {
	mu     sync.Mutex
	events []Event
}

func (c *captureEmitter) Emit(ev Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *captureEmitter) kinds() []EventKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]EventKind, len(c.events))
	for i, e := range c.events {
		out[i] = e.Kind
	}
	return out
}

// containsKinds reports whether want appears in order (not necessarily
// contiguously) within got.
func containsInOrder(got, want []EventKind) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestWorkerEmitsOrderedEventsOnPass(t *testing.T) {
	restore := swapSeams(
		func(_ context.Context, _, _, _ string) (string, error) { return "/wt", nil },
		func(_ context.Context, _ Config, _ string, _ Subtask, _ string, _ int) ([]DangerHit, error) { return nil, nil },
		func(_ context.Context, _ Config, _ string) []verify.GateResult {
			return []verify.GateResult{{Name: "test", Passed: true}}
		},
		func(_ context.Context, _, _ string) string { return "diff" },
	)
	defer restore()

	cap := &captureEmitter{}
	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(verdictToolName, map[string]any{"verdict": "pass", "reasons": []string{"ok"}}),
	}}
	cfg := Config{MaxIters: 4, Emitter: cap}
	res := RunWorker(context.Background(), cfg, Subtask{ID: "st-1", Goal: "g", Branch: "orch/x"}, m)
	if !res.Passed {
		t.Fatalf("expected pass, got %+v", res)
	}
	want := []EventKind{
		EventWorkerStart, EventIteration, EventAgent, EventGate, EventVerdict, EventWorkerDone,
	}
	if !containsInOrder(cap.kinds(), want) {
		t.Fatalf("event order mismatch:\n got=%v\nwant=%v", cap.kinds(), want)
	}
	// The terminal worker-done must carry Passed=true.
	last := cap.events[len(cap.events)-1]
	if last.Kind != EventWorkerDone || last.Passed == nil || !*last.Passed {
		t.Errorf("final event should be worker-done pass, got %+v", last)
	}
}

func TestWorkerEmitsFailRepairPassLoop(t *testing.T) {
	gateCalls := 0
	restore := swapSeams(
		func(_ context.Context, _, _, _ string) (string, error) { return "/wt", nil },
		func(_ context.Context, _ Config, _ string, _ Subtask, _ string, _ int) ([]DangerHit, error) { return nil, nil },
		func(_ context.Context, _ Config, _ string) []verify.GateResult {
			gateCalls++
			if gateCalls == 1 {
				return []verify.GateResult{{Name: "test", Passed: false, Output: "FAIL", Err: "exit 1"}}
			}
			return []verify.GateResult{{Name: "test", Passed: true}}
		},
		func(_ context.Context, _, _ string) string { return "diff" },
	)
	defer restore()

	cap := &captureEmitter{}
	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(verdictToolName, map[string]any{"verdict": "pass", "reasons": []string{"ok"}}),
	}}
	cfg := Config{MaxIters: 4, Emitter: cap}
	res := RunWorker(context.Background(), cfg, Subtask{ID: "st-1", Goal: "g", Branch: "orch/x"}, m)
	if !res.Passed || res.Iterations != 2 {
		t.Fatalf("expected pass on 2nd iteration, got %+v", res)
	}
	// Gate 1 fails first (no verdict), then a second iteration passes both gates.
	want := []EventKind{
		EventWorkerStart,
		EventIteration, EventAgent, EventGate, // iter 1: gate fail (no verdict)
		EventIteration, EventAgent, EventGate, EventVerdict, EventWorkerDone, // iter 2: pass
	}
	got := cap.kinds()
	if !containsInOrder(got, want) {
		t.Fatalf("event order mismatch:\n got=%v\nwant=%v", got, want)
	}
	// Exactly one verdict should be emitted (only iteration 2 reaches Gate 2).
	verdicts := 0
	firstGateFailed := false
	for _, e := range cap.events {
		if e.Kind == EventVerdict {
			verdicts++
		}
		if e.Kind == EventGate && e.Passed != nil && !*e.Passed && !firstGateFailed {
			firstGateFailed = true
		}
	}
	if verdicts != 1 {
		t.Errorf("expected exactly 1 verdict event, got %d", verdicts)
	}
	if !firstGateFailed {
		t.Errorf("expected a failing gate event in the first iteration")
	}
}

func TestRunEmitsPlanAndRunDone(t *testing.T) {
	restore := swapSeams(
		func(_ context.Context, _, _, _ string) (string, error) { return "/wt", nil },
		func(_ context.Context, _ Config, _ string, _ Subtask, _ string, _ int) ([]DangerHit, error) { return nil, nil },
		func(_ context.Context, _ Config, _ string) []verify.GateResult {
			return []verify.GateResult{{Name: "test", Passed: true}}
		},
		func(_ context.Context, _, _ string) string { return "diff" },
	)
	defer restore()

	cap := &captureEmitter{}
	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(planToolName, map[string]any{
			"subtasks": []map[string]any{
				{"goal": "do it", "branch": "do-it", "acceptance": []string{"works"}},
			},
		}),
		toolResp(verdictToolName, map[string]any{"verdict": "pass", "reasons": []string{"ok"}}),
	}}
	cfg := Config{RepoDir: t.TempDir(), Task: "task", Workers: 1, MaxIters: 2, Emitter: cap}
	rep, err := Run(context.Background(), cfg, m)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !rep.Passed {
		t.Fatalf("expected passing report, got %+v", rep)
	}
	got := cap.kinds()
	// plan comes before the first worker-start; run-done is terminal.
	if !containsInOrder(got, []EventKind{EventPlan, EventWorkerStart, EventRunDone}) {
		t.Fatalf("run events out of order: %v", got)
	}
	if got[len(got)-1] != EventRunDone {
		t.Errorf("last event should be run-done, got %v", got[len(got)-1])
	}
}

func TestNilEmitterIsNoOp(t *testing.T) {
	// A nil Emitter must not panic and must leave behavior unchanged: this is
	// what guarantees the CLI path and all other tests are unaffected.
	restore := swapSeams(
		func(_ context.Context, _, _, _ string) (string, error) { return "/wt", nil },
		func(_ context.Context, _ Config, _ string, _ Subtask, _ string, _ int) ([]DangerHit, error) { return nil, nil },
		func(_ context.Context, _ Config, _ string) []verify.GateResult {
			return []verify.GateResult{{Name: "test", Passed: true}}
		},
		func(_ context.Context, _, _ string) string { return "diff" },
	)
	defer restore()

	m := &stubMessenger{responses: []anthropic.Response{
		toolResp(verdictToolName, map[string]any{"verdict": "pass", "reasons": []string{"ok"}}),
	}}
	res := RunWorker(context.Background(), Config{MaxIters: 2}, Subtask{ID: "st-1", Goal: "g", Branch: "orch/x"}, m)
	if !res.Passed {
		t.Fatalf("expected pass with nil emitter, got %+v", res)
	}
}
