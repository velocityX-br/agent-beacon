package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/internal/agent"
	"github.com/local/agent-beacon/internal/anthropic"
	"github.com/local/agent-beacon/internal/orchestrator"
)

// maxEventReplay caps the per-run event buffer kept for replay to newly
// attaching browsers. Older events are dropped from the front; the terminal
// run-done event is always the last thing a live subscriber sees.
const maxEventReplay = 4096

// runStatus is the lifecycle state of an orchestration run.
type runStatus string

const (
	statusRunning runStatus = "running"
	statusWaiting runStatus = "waiting"
	statusDone    runStatus = "done"
	statusError   runStatus = "error"
)

// orchRun is one server-side orchestration run: its request metadata, a bounded
// replay buffer of emitted events, a live subscriber set (fan-out mirrors the
// registry's Subscribe shape), and the final report/error once finished.
type orchRun struct {
	ID        string
	Task      string
	Repo      string
	StartedAt time.Time

	mu          sync.RWMutex
	status      runStatus
	report      orchestrator.Report
	reportSet   bool
	err         string
	events      []orchestrator.Event
	subscribers map[int]chan orchestrator.Event
	nextSubID   int
	done        bool

	// pending maps an open intervention ReqID to the channel its Await is
	// blocked on. handleOrchRespond delivers the user's Decision here. Guarded
	// by mu. interventionTimeout bounds each Await so a paused run can never
	// hang forever.
	pending             map[string]chan orchestrator.Decision
	interventionTimeout time.Duration
}

// Await implements orchestrator.Intervener. It registers a buffered response
// channel under req.ReqID, flips the run status to "waiting", and blocks until
// handleOrchRespond delivers a Decision or the (timeout-bounded) ctx fires. On
// timeout it returns the safe default (deny / no guidance) so the run resumes
// and ends cleanly. The pause events themselves are emitted by the worker via
// Emit before Await is called, so they reach live + replaying subscribers.
func (r *orchRun) Await(ctx context.Context, req orchestrator.InterventionRequest) orchestrator.Decision {
	ch := make(chan orchestrator.Decision, 1)
	r.mu.Lock()
	if r.pending == nil {
		r.pending = make(map[string]chan orchestrator.Decision)
	}
	r.pending[req.ReqID] = ch
	prev := r.status
	r.status = statusWaiting
	timeout := r.interventionTimeout
	r.mu.Unlock()

	// Restore the prior status and drop the pending entry on every exit path.
	defer func() {
		r.mu.Lock()
		delete(r.pending, req.ReqID)
		if r.status == statusWaiting {
			r.status = prev
		}
		r.mu.Unlock()
	}()

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	select {
	case d := <-ch:
		return d
	case <-ctx.Done():
		return orchestrator.Decision{Approve: false}
	}
}

// respond delivers a user Decision to the Await blocked on reqID. It reports
// ok=false when no such request is pending (already resolved or timed out).
func (r *orchRun) respond(reqID string, d orchestrator.Decision) bool {
	r.mu.RLock()
	ch, ok := r.pending[reqID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case ch <- d:
		return true
	default:
		// Buffered chan already holds a decision; treat as resolved.
		return false
	}
}

// Emit implements orchestrator.Emitter: it appends to the replay buffer, fans
// the event out to all live subscribers (dropping for slow ones rather than
// blocking the run), and on run-done marks the run finished so subscribers can
// close. Safe for concurrent use — workers emit from goroutines.
func (r *orchRun) Emit(ev orchestrator.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	if len(r.events) > maxEventReplay {
		r.events = append([]orchestrator.Event(nil), r.events[len(r.events)-maxEventReplay:]...)
	}
	for _, ch := range r.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
	if ev.Kind == orchestrator.EventRunDone {
		r.done = true
	}
}

// Subscribe returns the buffered events so far plus a channel of subsequent
// live events and an unsubscribe func. It mirrors registry.Session.Subscribe so
// the WS handler can replay-then-stream identically. If the run has already
// finished, the channel is closed after the buffered snapshot so the caller
// drains history and exits.
func (r *orchRun) Subscribe() (backlog []orchestrator.Event, ch <-chan orchestrator.Event, unsubscribe func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	backlog = append([]orchestrator.Event(nil), r.events...)
	c := make(chan orchestrator.Event, 256)
	if r.done {
		close(c)
		return backlog, c, func() {}
	}
	id := r.nextSubID
	r.nextSubID++
	r.subscribers[id] = c
	unsub := func() {
		r.mu.Lock()
		if sc, ok := r.subscribers[id]; ok {
			delete(r.subscribers, id)
			close(sc)
		}
		r.mu.Unlock()
	}
	return backlog, c, unsub
}

// finish records the terminal report/error and closes all subscriber channels.
// It is called by the run goroutine after orchestrator.Run returns; the
// run-done event has typically already been emitted by Run itself.
func (r *orchRun) finish(rep orchestrator.Report, runErr error) {
	r.mu.Lock()
	r.report = rep
	r.reportSet = true
	if runErr != nil {
		r.status = statusError
		r.err = runErr.Error()
	} else {
		r.status = statusDone
	}
	r.done = true
	subs := r.subscribers
	r.subscribers = make(map[int]chan orchestrator.Event)
	r.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
}

// runView is the JSON-safe summary of a run for the list/detail REST endpoints.
type runView struct {
	ID         string    `json:"id"`
	Task       string    `json:"task"`
	Repo       string    `json:"repo"`
	Status     runStatus `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Passed     bool      `json:"passed"`
	Subtasks   int       `json:"subtasks"`
	Error      string    `json:"error,omitempty"`
}

func (r *orchRun) view() runView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v := runView{
		ID:        r.ID,
		Task:      r.Task,
		Repo:      r.Repo,
		Status:    r.status,
		StartedAt: r.StartedAt,
		Passed:    r.reportSet && r.report.Passed,
		Subtasks:  r.report.Subtasks,
		Error:     r.err,
	}
	if r.reportSet {
		v.FinishedAt = r.report.FinishedAt
		v.DurationMS = r.report.DurationMS
	}
	return v
}

// detail returns the run view plus the full report (nil until finished).
func (r *orchRun) detail() (runView, *orchestrator.Report) {
	v := r.view()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.reportSet {
		return v, nil
	}
	rep := r.report
	return v, &rep
}

// errRunActive is returned by orchStore.delete when the caller tries to remove
// a run that is still running or waiting for user intervention. Deleting such a
// run would orphan its background goroutine and worktree, so the delete is
// rejected and the handler maps this to HTTP 409 Conflict.
var errRunActive = errors.New("run is active")

// orchStore is the in-process set of orchestration runs. Like the registry it
// is single-replica, RWMutex-guarded, and holds all state in memory. When
// persistDir is set, finished runs are written there as JSON and reloaded on
// startup so the run history survives a server restart.
type orchStore struct {
	mu         sync.RWMutex
	runs       map[string]*orchRun
	persistDir string
}

// newOrchStore builds the store, reloading any finished runs previously
// persisted under dir. An empty dir disables persistence (in-memory only),
// preserving the original behavior for tests and unconfigured deployments.
func newOrchStore(dir string) *orchStore {
	return &orchStore{runs: loadPersisted(dir), persistDir: dir}
}

// start creates a run, launches orchestrator.Run in a goroutine with the run's
// Emit wired as cfg.Emitter, and returns the run id. The run's context derives
// from context.Background() (with the given total timeout) — NOT an HTTP
// request context — so the run survives the POST returning. Total<=0 means no
// wall-clock limit.
func (s *orchStore) start(cfg orchestrator.Config, m anthropic.Messenger, total, interventionTimeout time.Duration) *orchRun {
	run := &orchRun{
		ID:                  newRunID(),
		Task:                cfg.Task,
		Repo:                cfg.RepoDir,
		StartedAt:           time.Now(),
		status:              statusRunning,
		subscribers:         make(map[int]chan orchestrator.Event),
		pending:             make(map[string]chan orchestrator.Decision),
		interventionTimeout: interventionTimeout,
	}
	cfg.Emitter = run
	cfg.Intervener = run

	s.mu.Lock()
	s.runs[run.ID] = run
	s.mu.Unlock()

	go func() {
		ctx := context.Background()
		if total > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, total)
			defer cancel()
		}
		rep, err := orchestrator.Run(ctx, cfg, m)
		run.finish(rep, err)
		// Persist the finished run so its report survives a restart. Best
		// effort: a write failure is logged by the caller path but never
		// affects the completed run in memory.
		_ = s.persist(run)
	}()
	return run
}

// get returns a run by id.
func (s *orchStore) get(id string) (*orchRun, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	return r, ok
}

// list returns all runs newest-first for the list endpoint.
func (s *orchStore) list() []runView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]runView, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// delete removes a finished run from the store and its persisted JSON file. It
// refuses to delete an active run (running/waiting) — returning errRunActive —
// so the background orchestrator goroutine (and its worktrees) is never
// orphaned by a mid-flight deletion. The (found, err) result lets the handler
// distinguish a 404 (found=false) from a 409 (errRunActive) from success.
func (s *orchStore) delete(id string) (found bool, err error) {
	s.mu.Lock()
	r, ok := s.runs[id]
	if !ok {
		s.mu.Unlock()
		return false, nil
	}
	r.mu.RLock()
	active := r.status == statusRunning || r.status == statusWaiting
	r.mu.RUnlock()
	if active {
		s.mu.Unlock()
		return true, errRunActive
	}
	delete(s.runs, id)
	s.mu.Unlock()
	// Remove the on-disk copy outside the lock; a failure here is returned so
	// the handler can surface it, but the run is already gone from memory.
	return true, s.removePersisted(id)
}

// newRunID returns a short random identifier for an orchestration run.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "run-" + time.Now().Format("20060102150405.000")
	}
	return "run-" + hex.EncodeToString(b[:])
}

// --- HTTP handlers (all browser-gated) ---

// orchStartReq is the POST body to launch a browser-driven orchestration run.
type orchStartReq struct {
	Task     string `json:"task"`
	Repo     string `json:"repo"`
	Workers  int    `json:"workers,omitempty"`
	MaxIters int    `json:"max_iters,omitempty"`
	// Optional per-run model overrides; empty => server defaults.
	PlannerModel  string `json:"planner_model,omitempty"`
	WorkerModel   string `json:"worker_model,omitempty"`
	VerifierModel string `json:"verifier_model,omitempty"`
	// Optional per-run gate command overrides (whitespace-split into argv).
	// When set, these fully replace the auto-detected build/test commands so a
	// run works in environments where the language default (e.g. bare `pytest`)
	// isn't on PATH. E2ECmd adds a distinct e2e gate on top.
	BuildCmd string `json:"build_cmd,omitempty"`
	TestCmd  string `json:"test_cmd,omitempty"`
	E2ECmd   string `json:"e2e_cmd,omitempty"`
}

// handleOrchStart validates the request, confirms the repo is under an allowed
// root, builds an orchestrator.Config from the server's model/credential
// defaults (merged with any per-run overrides), and launches the run in the
// background. It returns {id} immediately; the browser then attaches to the
// events WS. The Anthropic Messenger is server-owned and never exposed.
func (s *Server) handleOrchStart(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if s.msgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "orchestration not configured: no Anthropic credentials on server"})
		return
	}
	var body orchStartReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if strings.TrimSpace(body.Task) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task is required"})
		return
	}
	// Repo safety: the requested path must resolve under a server-configured
	// allowed root. This is the only place the browser can point a run at the
	// filesystem, so the check must never be skipped.
	repo, err := agent.ResolveUnderRoot(s.cfg.OrchestrationRoots, body.Repo)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo: " + err.Error()})
		return
	}

	cfg := orchestrator.Config{
		RepoDir:       repo,
		Task:          body.Task,
		Workers:       body.Workers,
		MaxIters:      body.MaxIters,
		PlannerModel:  firstNonEmpty(body.PlannerModel, s.cfg.PlannerModel),
		WorkerModel:   firstNonEmpty(body.WorkerModel, s.cfg.WorkerModel),
		VerifierModel: firstNonEmpty(body.VerifierModel, s.cfg.VerifierModel),
		WorkerCmd:     s.cfg.WorkerCmd,
		BuildCmd:      splitArgs(body.BuildCmd),
		TestCmd:       splitArgs(body.TestCmd),
		E2ECmd:        splitArgs(body.E2ECmd),
	}
	run := s.orch.start(cfg, s.msgr, s.cfg.OrchestrationTimeout, s.cfg.InterventionTimeout)
	s.log.Info("orchestration started", "run", run.ID, "repo", repo)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": run.ID})
}

// handleOrchList returns all runs, newest-first.
func (s *Server) handleOrchList(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, s.orch.list())
}

// orchDetailResp is the run summary plus the full report (null until finished).
type orchDetailResp struct {
	Run    runView              `json:"run"`
	Report *orchestrator.Report `json:"report"`
}

// handleOrchGet returns a single run's view plus its report when available.
func (s *Server) handleOrchGet(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	run, ok := s.orch.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such run"})
		return
	}
	v, rep := run.detail()
	writeJSON(w, http.StatusOK, orchDetailResp{Run: v, Report: rep})
}

// handleOrchDelete removes a finished run from the store and deletes its
// persisted JSON file, so it disappears from the list immediately (no restart
// needed). It is browser-gated like the other handlers. An unknown id returns
// 404; an active (running/waiting) run returns 409 and is left untouched; a
// successful delete returns 204 No Content.
func (s *Server) handleOrchDelete(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	found, err := s.orch.delete(r.PathValue("id"))
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such run"})
		return
	}
	if errors.Is(err, errRunActive) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run is still active; cannot delete"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete run"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// orchRespondReq is the POST body delivering a user's intervention decision:
// approval of a flagged dangerous operation, or optional guidance when the run
// paused for input. req_id correlates with the EventAuthNeeded/EventInputNeeded
// event the browser received.
type orchRespondReq struct {
	ReqID    string `json:"req_id"`
	Approve  bool   `json:"approve"`
	Guidance string `json:"guidance,omitempty"`
}

// handleOrchRespond delivers a user Decision to a run's blocked Await. It
// mirrors handleSpawn's shape (browser-gated, bounded body). A 409 means the
// referenced request is no longer pending (already resolved or timed out), so
// the browser should stop showing the modal. A 202 means the decision was
// delivered and the run will resume.
func (s *Server) handleOrchRespond(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	run, ok := s.orch.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such run"})
		return
	}
	var body orchRespondReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if strings.TrimSpace(body.ReqID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "req_id is required"})
		return
	}
	if !run.respond(body.ReqID, orchestrator.Decision{Approve: body.Approve, Guidance: body.Guidance}) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no pending request for req_id (already resolved or timed out)"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "delivered"})
}

// handleOrchEventsWS replays the run's buffered events then streams subsequent
// live events as JSON text frames until the run finishes, then closes. Mirrors
// handleTerminalWS's pump but carries orchestrator.Event JSON instead of raw
// PTY bytes. There is no browser->server direction; reads only detect close.
func (s *Server) handleOrchEventsWS(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	run, ok := s.orch.get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such run", http.StatusNotFound)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("orchestration ws accept failed", "err", err)
		return
	}
	c.SetReadLimit(1 << 20)
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx := r.Context()
	backlog, out, unsubscribe := run.Subscribe()
	defer unsubscribe()

	writeEvent := func(ev orchestrator.Event) error {
		data, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return c.Write(wctx, websocket.MessageText, data)
	}

	// Replay history first so a late subscriber sees the whole run.
	for _, ev := range backlog {
		if err := writeEvent(ev); err != nil {
			return
		}
	}

	// Pump live events until the channel closes (run finished/unsubscribed).
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-out:
				if !ok {
					// Run finished: close cleanly so the browser stops.
					c.Close(websocket.StatusNormalClosure, "run complete")
					return
				}
				if err := writeEvent(ev); err != nil {
					return
				}
			}
		}
	}()

	// Read loop exists only to observe the client closing the socket.
	for {
		if _, _, err := c.Read(ctx); err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) && !errors.Is(err, context.Canceled) {
				s.log.Debug("orchestration read ended", "run", run.ID, "err", err)
			}
			return
		}
	}
}

// handleOrchDiff returns the git diff of the delivered work as text/plain, so
// the browser can inspect what the workers produced. The changes live in each
// subtask's isolated worktree (on an orch/* branch), not in the main repo
// working tree — and workers stage but do not commit, so a plain
// `git diff HEAD` / `HEAD..branch` in the repo would show nothing. We therefore
// diff each worktree with orchestrator.WorktreeDiff, which records new files
// via intent-to-add so brand-new files appear (the same diff the verifier saw).
// Query param branch selects a single orch/* branch's worktree; otherwise every
// subtask worktree is diffed and concatenated with per-branch headers.
func (s *Server) handleOrchDiff(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	run, ok := s.orch.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such run"})
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch != "" && !isSafeBranchRef(branch) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid branch"})
		return
	}

	// The worktree paths come from the finished report's per-subtask results.
	_, rep := run.detail()
	if rep == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run not finished; no diff yet"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var b strings.Builder
	multi := len(rep.Results) > 1 && branch == ""
	for _, res := range rep.Results {
		if branch != "" && res.Branch != branch {
			continue
		}
		if res.WorktreePath == "" {
			continue
		}
		diff := orchestrator.WorktreeDiff(ctx, run.Repo, res.WorktreePath)
		if multi {
			// Delineate each subtask's changes when diffing the whole run.
			b.WriteString("=== " + res.Branch + " ===\n")
		}
		if strings.TrimSpace(diff) != "" {
			b.WriteString(diff)
			b.WriteString("\n")
		} else if multi {
			b.WriteString("(no changes)\n")
		}
		if multi {
			b.WriteString("\n")
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strings.TrimRight(b.String(), "\n")))
}

// isSafeBranchRef guards the branch query param against shell/flag injection
// into git. We allow a conservative slug used by orch/* branches only.
func isSafeBranchRef(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
		return false
	}
	for _, r := range b {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '/' || r == '-' || r == '_' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// splitArgs splits a whitespace-separated command string into an argv slice,
// returning nil for an empty string. Commands needing embedded spaces should be
// wrapped in a script. Mirrors the CLI's splitArgs so browser and CLI runs
// interpret gate overrides identically.
func splitArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}
