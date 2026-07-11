package monitor

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func postReport(t *testing.T, h http.Handler, token string, remote string, b reportBody) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(b)
	req := httptest.NewRequest(http.MethodPost, "/report", bytes.NewReader(data))
	if token != "" {
		req.Header.Set("X-Agent-Beacon-Token", token)
	}
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReportEnrichesByPidAndCWD(t *testing.T) {
	store := newEnrichStore(30 * time.Second)
	h := newReportServer(store, "secret", slog.Default())

	rec := postReport(t, h, "secret", "127.0.0.1:5555", reportBody{Pid: 4242, Model: "claude-opus", ContextPct: 42})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pid report code = %d, want 202", rec.Code)
	}
	if e, ok := store.lookup(4242, ""); !ok || e.Model != "claude-opus" || e.ContextPct != 42 {
		t.Fatalf("pid lookup failed: %+v ok=%v", e, ok)
	}

	rec = postReport(t, h, "secret", "127.0.0.1:5555", reportBody{CWD: "/tmp", Task: "refactor"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cwd report code = %d, want 202", rec.Code)
	}
	if e, ok := store.lookup(0, "/tmp"); !ok || e.Task != "refactor" {
		t.Fatalf("cwd lookup failed: %+v ok=%v", e, ok)
	}
}

func TestReportRejectsBadToken(t *testing.T) {
	store := newEnrichStore(30 * time.Second)
	h := newReportServer(store, "secret", slog.Default())
	rec := postReport(t, h, "wrong", "127.0.0.1:5555", reportBody{Pid: 1})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token code = %d, want 401", rec.Code)
	}
}

func TestReportRejectsNonLoopback(t *testing.T) {
	store := newEnrichStore(30 * time.Second)
	h := newReportServer(store, "", slog.Default())
	rec := postReport(t, h, "", "10.0.0.5:5555", reportBody{Pid: 1})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback code = %d, want 403", rec.Code)
	}
}

func TestReportRequiresKey(t *testing.T) {
	store := newEnrichStore(30 * time.Second)
	h := newReportServer(store, "", slog.Default())
	rec := postReport(t, h, "", "127.0.0.1:5555", reportBody{Model: "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no key code = %d, want 400", rec.Code)
	}
}

func TestEnrichmentTTLExpiry(t *testing.T) {
	store := newEnrichStore(10 * time.Millisecond)
	store.put(reportBody{Pid: 9, Model: "m"})
	if _, ok := store.lookup(9, ""); !ok {
		t.Fatal("expected fresh enrichment")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := store.lookup(9, ""); ok {
		t.Fatal("expected expired enrichment to be gone")
	}
}

func TestNotificationSetsThenClearsWaiting(t *testing.T) {
	store := newEnrichStore(30 * time.Second)

	// A Notification event marks the process as waiting on the user.
	store.put(reportBody{Pid: 77, Event: "Notification"})
	e, ok := store.lookup(77, "")
	if !ok || !e.Waiting {
		t.Fatalf("Notification did not set Waiting: %+v ok=%v", e, ok)
	}

	// A subsequent non-Notification report (heartbeat) clears it, because put
	// replaces the whole entry.
	store.put(reportBody{Pid: 77, Event: "heartbeat"})
	e, ok = store.lookup(77, "")
	if !ok || e.Waiting {
		t.Fatalf("heartbeat did not clear Waiting: %+v ok=%v", e, ok)
	}

	// A Stop event marks it stopping (idle), not waiting.
	store.put(reportBody{Pid: 77, Event: "Stop"})
	e, _ = store.lookup(77, "")
	if e.Waiting || !e.Stopping {
		t.Fatalf("Stop event state wrong: %+v", e)
	}
}
