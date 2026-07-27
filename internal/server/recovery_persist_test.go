package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/local/agent-beacon/pkg/protocol"
)

// TestRecoverSaveTakeRoundTrip persists a heartbeat, reloads it into a fresh
// store, and asserts takePending returns the record for its device and drains
// it (a second take is empty).
func TestRecoverSaveTakeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newRecoverStore(dir)

	hb := protocol.Heartbeat{Device: "box1", CWD: "/repo/a", Command: "claude", Branch: "main"}
	store.saveHeartbeat(hb)

	id := recoveryID("box1", "/repo/a")
	if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
		t.Fatalf("expected persisted recovery file: %v", err)
	}

	// Fresh store must preload the record from disk.
	reloaded := newRecoverStore(dir)
	got := reloaded.takePending("box1")
	if len(got) != 1 {
		t.Fatalf("takePending got %d records, want 1", len(got))
	}
	if got[0].CWD != "/repo/a" || got[0].Command != "claude" || got[0].Device != "box1" {
		t.Errorf("reloaded record wrong: %+v", got[0])
	}
	// Draining is one-shot: a second take yields nothing.
	if again := reloaded.takePending("box1"); len(again) != 0 {
		t.Errorf("expected drained pending, got %d", len(again))
	}
}

// TestRecoverTakePendingFiltersByDevice ensures takePending returns only the
// requested device's records and leaves others pending.
func TestRecoverTakePendingFiltersByDevice(t *testing.T) {
	dir := t.TempDir()
	store := newRecoverStore(dir)
	store.saveHeartbeat(protocol.Heartbeat{Device: "box1", CWD: "/r/a", Command: "claude"})
	store.saveHeartbeat(protocol.Heartbeat{Device: "box2", CWD: "/r/b", Command: "claude"})

	got := store.takePending("box1")
	if len(got) != 1 || got[0].Device != "box1" {
		t.Fatalf("takePending(box1) = %+v, want one box1 record", got)
	}
	// box2 must still be pending.
	if other := store.takePending("box2"); len(other) != 1 || other[0].Device != "box2" {
		t.Fatalf("takePending(box2) = %+v, want one box2 record", other)
	}
}

// TestRecoverDisabledWhenNoDir confirms an empty dir keeps the store a no-op:
// nothing is written and takePending is empty.
func TestRecoverDisabledWhenNoDir(t *testing.T) {
	store := newRecoverStore("")
	store.saveHeartbeat(protocol.Heartbeat{Device: "box1", CWD: "/r/a", Command: "claude"})
	if got := store.takePending("box1"); len(got) != 0 {
		t.Fatalf("disabled store should have no pending, got %d", len(got))
	}
	if err := store.removeByDeviceCwd("box1", "/r/a"); err != nil {
		t.Fatalf("removeByDeviceCwd on disabled store should be a no-op, got %v", err)
	}
}

// TestRecoverSaveSkipsEmptyCwd ensures a heartbeat with no cwd persists nothing
// (there is no workspace to relaunch into).
func TestRecoverSaveSkipsEmptyCwd(t *testing.T) {
	dir := t.TempDir()
	store := newRecoverStore(dir)
	store.saveHeartbeat(protocol.Heartbeat{Device: "box1", CWD: "", Command: "claude"})
	if got := store.takePending("box1"); len(got) != 0 {
		t.Fatalf("empty-cwd heartbeat should persist nothing, got %d", len(got))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected no files written, got %d", len(entries))
	}
}

// TestRecoverPathRejectsTraversal ensures a recovery id containing path
// separators cannot escape the state directory.
func TestRecoverPathRejectsTraversal(t *testing.T) {
	if p := recoverPath("/state", "../evil"); p != "" {
		t.Errorf("expected traversal id rejected, got %q", p)
	}
	if p := recoverPath("/state", "a/b"); p != "" {
		t.Errorf("expected separator id rejected, got %q", p)
	}
	if p := recoverPath("/state", "abc123"); p != "/state/abc123.json" {
		t.Errorf("clean id path wrong: %q", p)
	}
}

// TestRecoverRemoveDeletesFile confirms removeByDeviceCwd deletes the on-disk
// file and drops the record so it is no longer pending.
func TestRecoverRemoveDeletesFile(t *testing.T) {
	dir := t.TempDir()
	store := newRecoverStore(dir)
	store.saveHeartbeat(protocol.Heartbeat{Device: "box1", CWD: "/r/a", Command: "claude"})
	id := recoveryID("box1", "/r/a")
	path := filepath.Join(dir, id+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("precondition: file should exist: %v", err)
	}
	if err := store.removeByDeviceCwd("box1", "/r/a"); err != nil {
		t.Fatalf("removeByDeviceCwd: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be deleted, stat err = %v", err)
	}
	if got := store.takePending("box1"); len(got) != 0 {
		t.Fatalf("removed record should not be pending, got %d", len(got))
	}
}

// TestRecoverSaveThrottled verifies the per-record throttle: a burst of
// heartbeats for the same workspace writes the file once, and its SavedAt does
// not advance until minGap elapses.
func TestRecoverSaveThrottled(t *testing.T) {
	dir := t.TempDir()
	store := newRecoverStore(dir)
	store.minGap = time.Hour // ensure the second save is throttled out

	hb := protocol.Heartbeat{Device: "box1", CWD: "/r/a", Command: "claude"}
	store.saveHeartbeat(hb)
	id := recoveryID("box1", "/r/a")
	path := filepath.Join(dir, id+".json")
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("first save should write file: %v", err)
	}

	// A second immediate save is throttled: the file is not rewritten.
	store.saveHeartbeat(hb)
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after throttled save: %v", err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Errorf("throttled save rewrote the file (mod times differ)")
	}
}
