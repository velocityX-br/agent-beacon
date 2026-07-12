package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/local/agent-beacon/internal/orchestrator"
)

// TestPersistRoundTrip writes a finished run to disk and reloads it into a
// fresh store, asserting the reloaded run carries the report and is treated as
// already-finished (so a subscriber drains history then closes).
func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newOrchStore(dir)

	run := &orchRun{
		ID:          "run-persist",
		Task:        "do the thing",
		Repo:        "/repo",
		StartedAt:   time.Now().Add(-time.Minute),
		status:      statusDone,
		subscribers: make(map[int]chan orchestrator.Event),
	}
	run.finish(orchestrator.Report{
		Task:       "do the thing",
		Passed:     true,
		Subtasks:   2,
		DurationMS: 1234,
		Results:    []orchestrator.WorkerResult{{Branch: "orch/a", Passed: true}},
	}, nil)

	if err := store.persist(run); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run-persist.json")); err != nil {
		t.Fatalf("expected persisted file: %v", err)
	}

	// Fresh store must reload the run from disk.
	reloaded := newOrchStore(dir)
	got, ok := reloaded.get("run-persist")
	if !ok {
		t.Fatal("reloaded store missing run")
	}
	v, rep := got.detail()
	if v.Status != statusDone || !v.Passed || v.Subtasks != 2 {
		t.Errorf("reloaded view wrong: %+v", v)
	}
	if rep == nil || rep.DurationMS != 1234 || len(rep.Results) != 1 {
		t.Errorf("reloaded report wrong: %+v", rep)
	}
	// A reloaded run is already done: Subscribe returns a closed channel after
	// the (empty) backlog so subscribers exit cleanly.
	backlog, ch, unsub := got.Subscribe()
	defer unsub()
	if len(backlog) != 0 {
		t.Errorf("expected empty backlog, got %d", len(backlog))
	}
	if _, open := <-ch; open {
		t.Error("expected closed channel for finished reloaded run")
	}
}

// TestPersistDisabledWhenNoDir confirms an empty state dir keeps the store fully
// in-memory: nothing is written and no error occurs.
func TestPersistDisabledWhenNoDir(t *testing.T) {
	store := newOrchStore("")
	run := &orchRun{ID: "run-x", subscribers: make(map[int]chan orchestrator.Event)}
	run.finish(orchestrator.Report{}, nil)
	if err := store.persist(run); err != nil {
		t.Fatalf("persist with empty dir should be a no-op, got %v", err)
	}
}

// TestPersistPathRejectsTraversal ensures a run id containing path separators
// cannot escape the state directory.
func TestPersistPathRejectsTraversal(t *testing.T) {
	if p := persistPath("/state", "../evil"); p != "" {
		t.Errorf("expected traversal id rejected, got %q", p)
	}
	if p := persistPath("/state", "a/b"); p != "" {
		t.Errorf("expected separator id rejected, got %q", p)
	}
	if p := persistPath("/state", "run-abc"); p != "/state/run-abc.json" {
		t.Errorf("clean id path wrong: %q", p)
	}
}
