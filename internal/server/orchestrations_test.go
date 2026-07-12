package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/agent-beacon/internal/anthropic"
	"github.com/local/agent-beacon/internal/orchestrator"
)

// --- store fan-out / replay / done-close ---

func TestOrchRunSubscribeReplaysThenStreams(t *testing.T) {
	run := &orchRun{
		ID:          "run-x",
		status:      statusRunning,
		subscribers: make(map[int]chan orchestrator.Event),
	}
	// Two events emitted before anyone subscribes must be replayed.
	run.Emit(orchestrator.Event{Kind: orchestrator.EventPlan})
	run.Emit(orchestrator.Event{Kind: orchestrator.EventWorkerStart})

	backlog, ch, unsub := run.Subscribe()
	defer unsub()
	if len(backlog) != 2 {
		t.Fatalf("expected 2 backlog events, got %d", len(backlog))
	}
	if backlog[0].Kind != orchestrator.EventPlan || backlog[1].Kind != orchestrator.EventWorkerStart {
		t.Fatalf("backlog order wrong: %+v", backlog)
	}

	// A live event fans out to the subscriber.
	run.Emit(orchestrator.Event{Kind: orchestrator.EventGate})
	select {
	case ev := <-ch:
		if ev.Kind != orchestrator.EventGate {
			t.Fatalf("expected gate, got %v", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
	}
}

func TestOrchRunSubscribeAfterDoneClosesChannel(t *testing.T) {
	run := &orchRun{
		ID:          "run-y",
		status:      statusRunning,
		subscribers: make(map[int]chan orchestrator.Event),
	}
	run.Emit(orchestrator.Event{Kind: orchestrator.EventPlan})
	run.Emit(orchestrator.Event{Kind: orchestrator.EventRunDone, Passed: boolPtrT(true)})

	backlog, ch, unsub := run.Subscribe()
	defer unsub()
	if len(backlog) != 2 {
		t.Fatalf("expected 2 backlog events, got %d", len(backlog))
	}
	// Because the run is done, the live channel must be already closed so a
	// late subscriber drains history and exits.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel for a finished run")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was not closed for a finished run")
	}
}

func TestOrchRunFinishClosesLiveSubscribers(t *testing.T) {
	run := &orchRun{
		ID:          "run-z",
		status:      statusRunning,
		subscribers: make(map[int]chan orchestrator.Event),
	}
	_, ch, unsub := run.Subscribe()
	defer unsub()

	run.finish(orchestrator.Report{Passed: true, Subtasks: 1}, nil)

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected subscriber channel closed on finish")
		}
	case <-time.After(time.Second):
		t.Fatal("finish did not close subscriber channel")
	}
	v := run.view()
	if v.Status != statusDone || !v.Passed {
		t.Fatalf("view after finish wrong: %+v", v)
	}
}

func TestEventReplayBufferIsBounded(t *testing.T) {
	run := &orchRun{
		ID:          "run-b",
		status:      statusRunning,
		subscribers: make(map[int]chan orchestrator.Event),
	}
	for i := 0; i < maxEventReplay+50; i++ {
		run.Emit(orchestrator.Event{Kind: orchestrator.EventLog})
	}
	backlog, _, unsub := run.Subscribe()
	unsub()
	if len(backlog) != maxEventReplay {
		t.Fatalf("expected buffer capped at %d, got %d", maxEventReplay, len(backlog))
	}
}

// --- route auth / validation ---

func boolPtrT(b bool) *bool { return &b }

// stubServerMessenger is a no-op Messenger sufficient to prove the start route
// gets past the "no credentials" guard without performing real network calls.
type stubServerMessenger struct{}

func (stubServerMessenger) Messages(context.Context, anthropic.Request) (anthropic.Response, error) {
	return anthropic.Response{}, nil
}

func newOrchTestServer(t *testing.T, roots []string, withMsgr bool) *httptest.Server {
	t.Helper()
	s := New(Config{AuthProvider: "none", OrchestrationRoots: roots}, nil)
	if withMsgr {
		s.msgr = stubServerMessenger{}
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestOrchStartRequiresAuth(t *testing.T) {
	s := New(Config{AuthProvider: "password"}, nil)
	s.msgr = stubServerMessenger{}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/orchestrations", "application/json",
		strings.NewReader(`{"task":"t","repo":"/tmp"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

func TestOrchStartRejectsMissingCredentials(t *testing.T) {
	ts := newOrchTestServer(t, []string{t.TempDir()}, false /* no messenger */)
	resp, err := http.Post(ts.URL+"/api/v1/orchestrations", "application/json",
		strings.NewReader(`{"task":"t","repo":"`+t.TempDir()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without credentials, got %d", resp.StatusCode)
	}
}

func TestOrchStartRejectsEmptyTask(t *testing.T) {
	ts := newOrchTestServer(t, []string{t.TempDir()}, true)
	resp, err := http.Post(ts.URL+"/api/v1/orchestrations", "application/json",
		strings.NewReader(`{"task":"  ","repo":"`+t.TempDir()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty task, got %d", resp.StatusCode)
	}
}

func TestOrchStartRejectsRepoOutsideRoots(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, not under root
	ts := newOrchTestServer(t, []string{root}, true)
	resp, err := http.Post(ts.URL+"/api/v1/orchestrations", "application/json",
		strings.NewReader(`{"task":"t","repo":"`+outside+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for repo outside roots, got %d", resp.StatusCode)
	}
}

func TestOrchListRequiresAuth(t *testing.T) {
	s := New(Config{AuthProvider: "password"}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/api/v1/orchestrations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestOrchListEmpty(t *testing.T) {
	ts := newOrchTestServer(t, nil, true)
	resp, err := http.Get(ts.URL + "/api/v1/orchestrations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var runs []runView
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected empty run list, got %d", len(runs))
	}
}

func TestOrchGetNotFound(t *testing.T) {
	ts := newOrchTestServer(t, nil, true)
	resp, err := http.Get(ts.URL + "/api/v1/orchestrations/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown run, got %d", resp.StatusCode)
	}
}

func TestIsSafeBranchRef(t *testing.T) {
	good := []string{"orch/task-1", "feature_x", "a.b", "main"}
	bad := []string{"", "-rf", "a..b", "a b", "a;rm", "a$(x)"}
	for _, g := range good {
		if !isSafeBranchRef(g) {
			t.Errorf("expected %q safe", g)
		}
	}
	for _, b := range bad {
		if isSafeBranchRef(b) {
			t.Errorf("expected %q unsafe", b)
		}
	}
}
