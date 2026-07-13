package monitor

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// DefaultReportAddr is the loopback address the report listener binds to. Bound
// to 127.0.0.1 only so only local processes (the Claude hook / `report` command)
// can reach it.
const DefaultReportAddr = "127.0.0.1:47615"

// reportBody is the JSON a Claude hook (via the `report` subcommand) POSTs to
// /report. All fields are optional except that at least one correlation key
// (Pid or CWD) must be present to attach the enrichment to a scanned process.
type reportBody struct {
	Pid        int      `json:"pid,omitempty"`
	CWD        string   `json:"cwd,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	Model      string   `json:"model,omitempty"`
	ContextPct float64  `json:"context_pct,omitempty"`
	Task       string   `json:"task,omitempty"`
	State      string   `json:"state,omitempty"`
	Event      string   `json:"event,omitempty"` // SessionStart | Stop | heartbeat | Notification
	PRNumber   int      `json:"pr_number,omitempty"`
	PRState    string   `json:"pr_state,omitempty"`
	PRURL      string   `json:"pr_url,omitempty"`
	MCPServers []string `json:"mcp_servers,omitempty"`
}

// enrichment is hook-provided metadata merged onto a scanned process.
type enrichment struct {
	Model      string
	ContextPct float64
	Task       string
	State      string
	SessionID  string
	PRNumber   int
	PRState    string
	PRURL      string
	MCPServers []string
	Stopping   bool // event == Stop: reflect idle/exited promptly
	Waiting    bool // event == Notification: Claude is blocked on the user
	expires    time.Time
}

// enrichStore holds hook enrichment keyed by "pid:<n>" and "cwd:<path>" with a
// short TTL so stale metadata does not linger after a hook stops reporting.
type enrichStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]enrichment
}

func newEnrichStore(ttl time.Duration) *enrichStore {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &enrichStore{ttl: ttl, m: make(map[string]enrichment)}
}

func (s *enrichStore) put(b reportBody) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The Notification hook fires when Claude is blocked waiting on the user; any
	// other event (heartbeat / Stop / SessionStart) means it is not blocked, so a
	// fresh non-Notification report naturally clears the waiting flag because put
	// replaces the whole entry.
	e := enrichment{
		Model:      b.Model,
		ContextPct: b.ContextPct,
		Task:       b.Task,
		State:      b.State,
		SessionID:  b.SessionID,
		PRNumber:   b.PRNumber,
		PRState:    b.PRState,
		PRURL:      b.PRURL,
		MCPServers: b.MCPServers,
		Stopping:   b.Event == "Stop",
		Waiting:    b.Event == "Notification",
		expires:    time.Now().Add(s.ttl),
	}
	if b.Pid > 0 {
		s.m["pid:"+strconv.Itoa(b.Pid)] = e
	}
	if b.CWD != "" {
		s.m["cwd:"+normalizeCWD(b.CWD)] = e
	}
}

// lookup finds enrichment for a scanned process, preferring a pid match then a
// cwd match. Expired entries are ignored (and lazily dropped).
func (s *enrichStore) lookup(pid int, cwd string) (enrichment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if e, ok := s.m["pid:"+strconv.Itoa(pid)]; ok {
		if now.Before(e.expires) {
			return e, true
		}
		delete(s.m, "pid:"+strconv.Itoa(pid))
	}
	if cwd != "" {
		key := "cwd:" + normalizeCWD(cwd)
		if e, ok := s.m[key]; ok {
			if now.Before(e.expires) {
				return e, true
			}
			delete(s.m, key)
		}
	}
	return enrichment{}, false
}

// normalizeCWD resolves symlinks so a hook's cwd and a scan's cwd compare equal.
func normalizeCWD(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// reportServer is the loopback HTTP listener that ingests hook reports.
type reportServer struct {
	store *enrichStore
	token string
	log   *slog.Logger
}

// serveReports starts the loopback report listener on addr and blocks until it
// errors or the listener is closed. It returns the *http.Server so the caller
// can shut it down.
func newReportServer(store *enrichStore, token string, log *slog.Logger) http.Handler {
	rs := &reportServer{store: store, token: token, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /report", rs.handleReport)
	mux.HandleFunc("GET /state", rs.handleState)
	return mux
}

func (rs *reportServer) handleReport(w http.ResponseWriter, r *http.Request) {
	// Loopback-only: reject anything not from localhost, even if bound wider.
	if !isLoopbackRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Constant-time token check when a token is configured.
	if rs.token != "" {
		got := r.Header.Get("X-Agent-Beacon-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(rs.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	var b reportBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&b); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if b.Pid == 0 && b.CWD == "" {
		http.Error(w, "pid or cwd required", http.StatusBadRequest)
		return
	}
	rs.store.put(b)
	w.WriteHeader(http.StatusAccepted)
}

// stateResponse is the JSON /state returns so a managed session (which sends its
// own WS heartbeat and bypasses the scan-based enrichment merge) can learn the
// hook-driven waiting/idle signal for its own cwd/pid. Empty when no enrichment
// exists (the caller then keeps its default running state).
type stateResponse struct {
	State    string `json:"state,omitempty"` // hook-reported explicit state, if any
	Waiting  bool   `json:"waiting"`         // Notification hook: blocked on the user
	Stopping bool   `json:"stopping"`        // Stop hook: session went idle
}

// handleState answers a managed agent's poll for its own hook-driven state,
// keyed by pid and/or cwd query params. It mirrors the enrichStore lookup the
// daemon does for observed processes so a managed session gets the same
// Notification-driven "waiting" alert. Loopback + token guarded like /report.
func (rs *reportServer) handleState(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if rs.token != "" {
		got := r.Header.Get("X-Agent-Beacon-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(rs.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
	cwd := r.URL.Query().Get("cwd")
	var resp stateResponse
	if e, ok := rs.store.lookup(pid, cwd); ok {
		resp = stateResponse{State: e.State, Waiting: e.Waiting, Stopping: e.Stopping}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// isLoopbackRequest reports whether the request originated from the loopback
// interface.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
