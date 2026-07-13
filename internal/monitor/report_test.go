package monitor

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// getState issues a GET /state?cwd=&pid= against the report handler, mirroring
// how a managed agent polls for its own hook-driven waiting/idle signal.
func getState(t *testing.T, h http.Handler, remote, cwd string, pid int) (stateResponse, int) {
	t.Helper()
	q := "/state?cwd=" + cwd
	if pid > 0 {
		q += "&pid=" + strconv.Itoa(pid)
	}
	req := httptest.NewRequest(http.MethodGet, q, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var sr stateResponse
	_ = json.NewDecoder(rec.Body).Decode(&sr)
	return sr, rec.Code
}

// TestStateEndpointReflectsNotification verifies a managed session polling
// /state sees the Notification hook's waiting signal (and that a later heartbeat
// clears it), which is the mechanism that lets a PTY-wrapped session raise the
// same intervention alert an observed session does.
func TestStateEndpointReflectsNotification(t *testing.T) {
	store := newEnrichStore(30 * time.Second)
	h := newReportServer(store, "", slog.Default())

	// No enrichment yet: state is empty, not waiting.
	if sr, code := getState(t, h, "127.0.0.1:5555", "/work/repo", 0); code != http.StatusOK || sr.Waiting {
		t.Fatalf("baseline state = %+v code=%d, want empty/200", sr, code)
	}

	// A Notification report for the cwd flips waiting on.
	rec := postReport(t, h, "", "127.0.0.1:5555", reportBody{CWD: "/work/repo", Event: "Notification"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notification report code = %d, want 202", rec.Code)
	}
	if sr, code := getState(t, h, "127.0.0.1:5555", "/work/repo", 0); code != http.StatusOK || !sr.Waiting {
		t.Fatalf("post-notification state = %+v code=%d, want waiting", sr, code)
	}

	// A heartbeat report clears it (put replaces the whole entry).
	postReport(t, h, "", "127.0.0.1:5555", reportBody{CWD: "/work/repo", Event: "heartbeat"})
	if sr, _ := getState(t, h, "127.0.0.1:5555", "/work/repo", 0); sr.Waiting {
		t.Fatalf("post-heartbeat state still waiting: %+v", sr)
	}

	// A Stop report reports stopping (idle), not waiting.
	postReport(t, h, "", "127.0.0.1:5555", reportBody{CWD: "/work/repo", Event: "Stop"})
	if sr, _ := getState(t, h, "127.0.0.1:5555", "/work/repo", 0); sr.Waiting || !sr.Stopping {
		t.Fatalf("post-stop state = %+v, want stopping not waiting", sr)
	}
}

// TestStateEndpointRejectsNonLoopback ensures /state is loopback-guarded like
// /report so a remote host cannot probe local session state.
func TestStateEndpointRejectsNonLoopback(t *testing.T) {
	store := newEnrichStore(30 * time.Second)
	h := newReportServer(store, "", slog.Default())
	if _, code := getState(t, h, "10.0.0.5:5555", "/work/repo", 0); code != http.StatusForbidden {
		t.Fatalf("non-loopback /state code = %d, want 403", code)
	}
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

// TestReportCarriesPRAndMCP verifies the PR + MCP enrichment fields survive the
// put -> lookup round trip that sendSnapshot relies on to merge them onto the
// emitted Heartbeat.
func TestReportCarriesPRAndMCP(t *testing.T) {
	store := newEnrichStore(30 * time.Second)
	h := newReportServer(store, "", slog.Default())

	rec := postReport(t, h, "", "127.0.0.1:5555", reportBody{
		Pid:        1234,
		PRNumber:   42,
		PRState:    "OPEN",
		PRURL:      "https://github.com/acme/widget/pull/42",
		MCPServers: []string{"sap-jira", "github-tools"},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("report code = %d, want 202", rec.Code)
	}

	e, ok := store.lookup(1234, "")
	if !ok {
		t.Fatal("expected enrichment for pid 1234")
	}
	if e.PRNumber != 42 || e.PRState != "OPEN" ||
		e.PRURL != "https://github.com/acme/widget/pull/42" {
		t.Fatalf("PR fields not propagated: %+v", e)
	}
	if len(e.MCPServers) != 2 || e.MCPServers[0] != "sap-jira" || e.MCPServers[1] != "github-tools" {
		t.Fatalf("MCP servers not propagated: %+v", e.MCPServers)
	}
}
