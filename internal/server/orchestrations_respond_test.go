package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/agent-beacon/internal/orchestrator"
)

// TestOrchRespondApproveRoundTrip proves the Await/respond round-trip: a
// goroutine blocked in Await receives the Decision the POST delivers, including
// approval + guidance.
func TestOrchRespondApproveRoundTrip(t *testing.T) {
	run := &orchRun{
		ID:          "run-r1",
		status:      statusRunning,
		subscribers: make(map[int]chan orchestrator.Event),
		pending:     make(map[string]chan orchestrator.Decision),
	}

	got := make(chan orchestrator.Decision, 1)
	go func() {
		got <- run.Await(context.Background(), orchestrator.InterventionRequest{ReqID: "req-1", Kind: "auth"})
	}()

	// Wait until the pending entry is registered so respond can find it.
	waitPending(t, run, "req-1")

	if !run.respond("req-1", orchestrator.Decision{Approve: true, Guidance: "do X"}) {
		t.Fatal("respond returned false for a live pending request")
	}
	select {
	case d := <-got:
		if !d.Approve || d.Guidance != "do X" {
			t.Fatalf("decision not delivered: %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return after respond")
	}
	// After resolution the pending entry must be gone (respond now 409-worthy).
	if run.respond("req-1", orchestrator.Decision{Approve: true}) {
		t.Fatal("respond should fail once the request is resolved")
	}
}

// TestOrchRespondTimeoutDenies proves a bounded Await returns the safe default
// (deny / no guidance) when no response arrives before the timeout.
func TestOrchRespondTimeoutDenies(t *testing.T) {
	run := &orchRun{
		ID:                  "run-r2",
		status:              statusRunning,
		subscribers:         make(map[int]chan orchestrator.Event),
		pending:             make(map[string]chan orchestrator.Decision),
		interventionTimeout: 50 * time.Millisecond,
	}
	d := run.Await(context.Background(), orchestrator.InterventionRequest{ReqID: "req-2", Kind: "auth"})
	if d.Approve || d.Guidance != "" {
		t.Fatalf("timeout should deny with no guidance, got %+v", d)
	}
	// Status must be restored (not stuck on waiting) after the timeout.
	if v := run.view(); v.Status != statusRunning {
		t.Fatalf("status not restored after timeout: %s", v.Status)
	}
}

// TestHandleOrchRespondUnknownReqID proves the endpoint returns 409 when the
// req_id has no live pending request (already resolved or timed out).
func TestHandleOrchRespondUnknownReqID(t *testing.T) {
	s := New(Config{AuthProvider: "none"}, nil)
	run := &orchRun{
		ID:          "run-r3",
		status:      statusRunning,
		subscribers: make(map[int]chan orchestrator.Event),
		pending:     make(map[string]chan orchestrator.Decision),
	}
	s.orch.runs[run.ID] = run
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/orchestrations/run-r3/respond", "application/json",
		strings.NewReader(`{"req_id":"missing","approve":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for unknown req_id, got %d", resp.StatusCode)
	}
}

// TestHandleOrchRespondDeliversToAwait proves the HTTP path (not just the
// in-process respond) wakes a blocked Await with the posted decision.
func TestHandleOrchRespondDeliversToAwait(t *testing.T) {
	s := New(Config{AuthProvider: "none"}, nil)
	run := &orchRun{
		ID:          "run-r4",
		status:      statusRunning,
		subscribers: make(map[int]chan orchestrator.Event),
		pending:     make(map[string]chan orchestrator.Decision),
	}
	s.orch.runs[run.ID] = run
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	got := make(chan orchestrator.Decision, 1)
	go func() {
		got <- run.Await(context.Background(), orchestrator.InterventionRequest{ReqID: "req-4", Kind: "input"})
	}()
	waitPending(t, run, "req-4")

	resp, err := http.Post(ts.URL+"/api/v1/orchestrations/run-r4/respond", "application/json",
		strings.NewReader(`{"req_id":"req-4","approve":false,"guidance":"try harder"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 delivered, got %d", resp.StatusCode)
	}
	select {
	case d := <-got:
		if d.Approve || d.Guidance != "try harder" {
			t.Fatalf("posted decision not delivered: %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return after POST /respond")
	}
}

func TestHandleOrchRespondRequiresAuth(t *testing.T) {
	s := New(Config{AuthProvider: "password"}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/api/v1/orchestrations/x/respond", "application/json",
		strings.NewReader(`{"req_id":"r","approve":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

// waitPending blocks until reqID is registered in run.pending, failing the test
// if it does not appear promptly.
func waitPending(t *testing.T, run *orchRun, reqID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run.mu.RLock()
		_, ok := run.pending[reqID]
		run.mu.RUnlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending request %q never registered", reqID)
}
