package verify

import (
	"context"
	"testing"
	"time"
)

func TestRunGatesPassAndFail(t *testing.T) {
	dir := t.TempDir()
	gates := []Gate{
		{Name: "ok", Cmd: []string{"true"}, Dir: dir},
		{Name: "bad", Cmd: []string{"false"}, Dir: dir},
		{Name: "never", Cmd: []string{"true"}, Dir: dir},
	}
	res := RunGates(context.Background(), gates, 0)
	// Fail-fast: should stop after the failing gate, so only 2 results.
	if len(res) != 2 {
		t.Fatalf("expected fail-fast to 2 results, got %d: %+v", len(res), res)
	}
	if !res[0].Passed {
		t.Errorf("gate 0 should pass")
	}
	if res[1].Passed {
		t.Errorf("gate 1 should fail")
	}
	if AllPassed(res) {
		t.Errorf("AllPassed should be false")
	}
}

func TestRunGatesCapturesOutput(t *testing.T) {
	dir := t.TempDir()
	gates := []Gate{{Name: "echo", Cmd: []string{"sh", "-c", "echo hello; exit 1"}, Dir: dir}}
	res := RunGates(context.Background(), gates, 5*time.Second)
	if len(res) != 1 || res[0].Passed {
		t.Fatalf("expected one failing gate, got %+v", res)
	}
	if res[0].Output != "hello" {
		t.Errorf("expected captured output 'hello', got %q", res[0].Output)
	}
	if res[0].Err == "" {
		t.Errorf("expected non-empty Err for failing gate")
	}
}

func TestGatesFromLanguagesOverrides(t *testing.T) {
	dir := "/repo"
	langs := []Language{{Name: "go", Build: []string{"go", "build", "./..."}, Test: []string{"go", "test", "./..."}}}

	// Overrides fully replace detected build/test.
	gates := GatesFromLanguages(dir, langs, []string{"make", "build"}, []string{"make", "test"}, []string{"make", "e2e"})
	if len(gates) != 3 {
		t.Fatalf("expected 3 gates (build, test, e2e), got %d: %+v", len(gates), gates)
	}
	if cmdString(gates[0].Cmd) != "make build" || cmdString(gates[1].Cmd) != "make test" || cmdString(gates[2].Cmd) != "make e2e" {
		t.Errorf("override gates wrong: %+v", gates)
	}

	// No overrides -> language defaults + no e2e.
	gates2 := GatesFromLanguages(dir, langs, nil, nil, nil)
	if len(gates2) != 2 {
		t.Fatalf("expected 2 language gates, got %d: %+v", len(gates2), gates2)
	}
	if cmdString(gates2[0].Cmd) != "go build ./..." {
		t.Errorf("expected go build first, got %q", cmdString(gates2[0].Cmd))
	}

	// e2e appended even when using language defaults.
	gates3 := GatesFromLanguages(dir, langs, nil, nil, []string{"pytest", "-m", "e2e"})
	if gates3[len(gates3)-1].Name != "e2e" {
		t.Errorf("expected trailing e2e gate, got %+v", gates3)
	}
}

func TestSummarizeOnlyFailures(t *testing.T) {
	res := []GateResult{
		{Name: "ok", Passed: true, Output: "fine"},
		{Name: "bad", Passed: false, Output: "boom", Err: "exit 1"},
	}
	s := Summarize(res)
	if !contains(s, "GATE FAILED: bad") || !contains(s, "boom") {
		t.Errorf("summary missing failure detail: %q", s)
	}
	if contains(s, "fine") {
		t.Errorf("summary should not include passing gate output: %q", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
