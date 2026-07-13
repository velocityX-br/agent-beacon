package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/local/agent-beacon/internal/orchestrator"
)

// TestDeleteFinishedRun removes a persisted, finished run and asserts it is
// gone from both the in-memory store and disk — the "real-time" cleanup path.
func TestDeleteFinishedRun(t *testing.T) {
	dir := t.TempDir()
	store := newOrchStore(dir)

	run := &orchRun{
		ID:          "run-del",
		Task:        "cleanup me",
		status:      statusDone,
		subscribers: make(map[int]chan orchestrator.Event),
	}
	run.finish(orchestrator.Report{Passed: true}, nil)
	store.runs[run.ID] = run
	if err := store.persist(run); err != nil {
		t.Fatalf("persist: %v", err)
	}
	path := filepath.Join(dir, "run-del.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected persisted file before delete: %v", err)
	}

	found, err := store.delete("run-del")
	if !found || err != nil {
		t.Fatalf("delete finished run: found=%v err=%v", found, err)
	}
	if _, ok := store.get("run-del"); ok {
		t.Error("run still in store after delete")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected persisted file removed, stat err=%v", err)
	}
}

// TestDeleteMissingRun reports found=false for an unknown id so the handler can
// return 404.
func TestDeleteMissingRun(t *testing.T) {
	store := newOrchStore(t.TempDir())
	found, err := store.delete("run-nope")
	if found {
		t.Error("expected found=false for missing run")
	}
	if err != nil {
		t.Errorf("expected no error for missing run, got %v", err)
	}
}

// TestDeleteActiveRunRefused ensures running/waiting runs are protected: delete
// leaves them in the store and returns errRunActive (handler -> 409).
func TestDeleteActiveRunRefused(t *testing.T) {
	for _, st := range []runStatus{statusRunning, statusWaiting} {
		store := newOrchStore(t.TempDir())
		store.runs["run-active"] = &orchRun{
			ID:          "run-active",
			status:      st,
			subscribers: make(map[int]chan orchestrator.Event),
		}
		found, err := store.delete("run-active")
		if !found {
			t.Errorf("[%s] expected found=true", st)
		}
		if !errors.Is(err, errRunActive) {
			t.Errorf("[%s] expected errRunActive, got %v", st, err)
		}
		if _, ok := store.get("run-active"); !ok {
			t.Errorf("[%s] active run must remain in store", st)
		}
	}
}
