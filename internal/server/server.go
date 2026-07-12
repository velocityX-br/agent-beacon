// Package server hosts the dashboard HTTP surface, the agent WebSocket endpoint,
// and (in later phases) the browser terminal WebSocket. It owns a registry of
// live sessions. State is in-process; run a single replica.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/internal/anthropic"
	"github.com/local/agent-beacon/internal/auth"
	"github.com/local/agent-beacon/internal/registry"
	"github.com/local/agent-beacon/pkg/protocol"
	"github.com/local/agent-beacon/web"
)

// Config controls server behaviour.
type Config struct {
	// AgentToken is the pre-shared key every agent must present as a bearer.
	AgentToken string
	// AuthProvider is the browser auth mode: password|oidc|proxy-header|none.
	AuthProvider string
	// ProviderName is a human label for the provider (shown on the login page).
	ProviderName string
	// StartURL / LogoutURL are surfaced via /api/v1/login-info.
	StartURL  string
	LogoutURL string
	// HeartbeatTTL marks a session stale after this long without a heartbeat.
	HeartbeatTTL time.Duration

	// --- Browser auth wiring (Phase 5) ---
	// Password holds the bcrypt checker for the password provider.
	Password *auth.PasswordChecker
	// ProxyHeader holds the trusted-proxy authenticator for proxy-header mode.
	ProxyHeader *auth.ProxyHeaderAuth
	// OIDC holds the discovered OpenID Connect provider for oidc mode.
	OIDC *auth.OIDCProvider
	// SessionTTL controls how long browser session cookies remain valid.
	SessionTTL time.Duration

	// --- Orchestration wiring (UI-driven multi-agent loop) ---
	// Anthropic credentials for the server-owned Messenger. AnthropicKey uses
	// x-api-key style; AnthropicBearer uses the Claude Code proxy/gateway
	// Bearer style; AnthropicBaseURL overrides the default endpoint. These live
	// only server-side and are never logged or returned to the browser.
	AnthropicKey     string
	AnthropicBearer  string
	AnthropicBaseURL string
	// Model roles for browser-launched orchestration runs. The verifier should
	// differ from the worker so the cross-model check is genuinely independent.
	PlannerModel  string
	WorkerModel   string
	VerifierModel string
	// WorkerCmd is the coding-agent binary the workers run (default "claude").
	WorkerCmd string
	// OrchestrationRoots is the allowlist of repository roots a browser run may
	// target. A requested repo must resolve (symlinks included) under one of
	// these, so the web can never write to arbitrary host paths.
	OrchestrationRoots []string
	// OrchestrationTimeout bounds a browser-launched run's total wall-clock.
	// 0 => no limit.
	OrchestrationTimeout time.Duration
	// InterventionTimeout bounds each user-intervention wait (dangerous-op
	// authorization or guidance-on-exhaustion). On timeout the run resumes with
	// the safe default (deny / no guidance) so it never hangs. Default 10m.
	InterventionTimeout time.Duration
	// OrchestrationStateDir is where finished run reports are persisted as
	// JSON and reloaded on startup, so run history survives a restart. Empty
	// disables persistence (in-memory only).
	OrchestrationStateDir string
}

// Server is the running dashboard server.
type Server struct {
	cfg      Config
	reg      *registry.Registry
	log      *slog.Logger
	sessions *auth.Store
	orch     *orchStore
	msgr     anthropic.Messenger
}

// New constructs a Server with sane defaults.
func New(cfg Config, log *slog.Logger) *Server {
	if cfg.HeartbeatTTL <= 0 {
		cfg.HeartbeatTTL = 15 * time.Second
	}
	if cfg.AuthProvider == "" {
		cfg.AuthProvider = string(auth.ModePassword)
	}
	if cfg.LogoutURL == "" {
		cfg.LogoutURL = "/api/v1/logout"
	}
	if cfg.InterventionTimeout <= 0 {
		cfg.InterventionTimeout = 10 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	// Build the server-owned Messenger for browser-launched orchestration runs
	// when credentials are configured. Left nil otherwise — the orchestration
	// routes then refuse with a clear error rather than panicking.
	var msgr anthropic.Messenger
	if cfg.AnthropicKey != "" || cfg.AnthropicBearer != "" {
		msgr = anthropic.New(cfg.AnthropicKey).
			WithBaseURL(cfg.AnthropicBaseURL).
			WithBearerToken(cfg.AnthropicBearer)
	}
	return &Server{
		cfg:      cfg,
		reg:      registry.New(cfg.HeartbeatTTL),
		log:      log,
		sessions: auth.NewStore(cfg.SessionTTL),
		orch:     newOrchStore(cfg.OrchestrationStateDir),
		msgr:     msgr,
	}
}

// Registry exposes the session store (used by terminal handlers, tests).
func (s *Server) Registry() *registry.Registry { return s.reg }

// Handler returns the root http.Handler with all routes mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/login-info", s.handleLoginInfo)
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", s.handleLogout)
	mux.HandleFunc("GET /api/v1/auth/start", s.handleAuthStart)
	mux.HandleFunc("GET /api/v1/auth/callback", s.handleAuthCallback)
	mux.HandleFunc("GET /api/v1/sessions", s.handleSessions)
	mux.HandleFunc("POST /api/v1/spawn", s.handleSpawn)
	// UI-driven multi-agent orchestration: launch a run, list/inspect runs,
	// stream live progress over WS, and fetch the delivered diff.
	mux.HandleFunc("POST /api/v1/orchestrations", s.handleOrchStart)
	mux.HandleFunc("GET /api/v1/orchestrations", s.handleOrchList)
	mux.HandleFunc("GET /api/v1/orchestrations/{id}", s.handleOrchGet)
	mux.HandleFunc("GET /api/v1/orchestrations/{id}/events", s.handleOrchEventsWS)
	mux.HandleFunc("GET /api/v1/orchestrations/{id}/diff", s.handleOrchDiff)
	mux.HandleFunc("POST /api/v1/orchestrations/{id}/respond", s.handleOrchRespond)
	mux.HandleFunc("/api/v1/agent/connect", s.handleAgentWS)
	mux.HandleFunc("/api/v1/terminal/{id}", s.handleTerminalWS)
	// Dashboard SPA: serve embedded static assets, falling back to index.html
	// so client-side routing works.
	mux.Handle("/", s.spaHandler())
	return mux
}

// spaHandler serves the embedded SPA assets. Unknown paths (that are not asset
// files) fall back to index.html so the single page can handle routing.
func (s *Server) spaHandler() http.Handler {
	fsys := web.FS()
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := fsys.Open(p); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// Fallback to the SPA entry point.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// loginInfo mirrors the documented public shape so existing clients/tests can
// reason about it; the values come from our own config.
type loginInfo struct {
	Provider     string `json:"provider"`
	ProviderName string `json:"provider_name"`
	StartURL     string `json:"start_url"`
	LogoutURL    string `json:"logout_url"`
}

func (s *Server) handleLoginInfo(w http.ResponseWriter, _ *http.Request) {
	startURL := s.cfg.StartURL
	if s.cfg.AuthProvider == string(auth.ModeOIDC) && startURL == "" {
		startURL = "/api/v1/auth/start"
	}
	writeJSON(w, http.StatusOK, loginInfo{
		Provider:     s.cfg.AuthProvider,
		ProviderName: s.cfg.ProviderName,
		StartURL:     startURL,
		LogoutURL:    s.cfg.LogoutURL,
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, s.reg.Snapshot())
}

// handleSpawn routes a dashboard spawn request to a live agent on the target
// device. The agent validates the project path against its allowed roots and
// launches a new detached session (optionally in a fresh git worktree).
func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var body struct {
		Device           string `json:"device"`
		ProjectPath      string `json:"project_path"`
		Command          string `json:"command"`
		WorktreeBranch   string `json:"worktree_branch"`
		WorktreeLocation string `json:"worktree_location"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if body.Device == "" || body.ProjectPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device and project_path are required"})
		return
	}
	sess := s.reg.AnyOnDevice(body.Device)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no live agent on device " + body.Device})
		return
	}
	err := sess.SendSpawn(protocol.SpawnMsg{
		ProjectPath:      body.ProjectPath,
		Command:          body.Command,
		WorktreeBranch:   body.WorktreeBranch,
		WorktreeLocation: body.WorktreeLocation,
	})
	if err != nil {
		s.log.Warn("spawn send failed", "device", body.Device, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not reach agent"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "spawn requested"})
}

// browserAuthorized reports whether a browser request is permitted. The "none"
// provider is open; proxy-header trusts the reverse-proxy identity header; all
// other providers require a valid session cookie.
func (s *Server) browserAuthorized(r *http.Request) bool {
	switch auth.Mode(s.cfg.AuthProvider) {
	case auth.ModeNone:
		return true
	case auth.ModeProxyHeader:
		if s.cfg.ProxyHeader == nil {
			return false
		}
		_, ok := s.cfg.ProxyHeader.Subject(r.Header.Get(s.cfg.ProxyHeader.Header))
		return ok
	default: // password, oidc
		if tok := auth.CookieToken(r); tok != "" {
			if _, ok := s.sessions.Lookup(tok); ok {
				return true
			}
		}
		return false
	}
}

// isTLS reports whether the request arrived over TLS, so cookies can be marked
// Secure. Honors X-Forwarded-Proto for reverse-proxy deployments.
func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// handleLogin authenticates a password submission and issues a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if auth.Mode(s.cfg.AuthProvider) != auth.ModePassword {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password login not enabled"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if s.cfg.Password == nil || !s.cfg.Password.Verify(body.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid password"})
		return
	}
	sess, err := s.sessions.Issue("password-user")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session error"})
		return
	}
	auth.SetCookie(w, sess, isTLS(r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLogout revokes the current session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := auth.CookieToken(r); tok != "" {
		s.sessions.Revoke(tok)
	}
	auth.ClearCookie(w, isTLS(r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAuthStart redirects the browser to the OIDC provider's authorize URL.
func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OIDC == nil {
		http.Error(w, "oidc not enabled", http.StatusBadRequest)
		return
	}
	u, err := s.cfg.OIDC.AuthCodeURL()
	if err != nil {
		http.Error(w, "auth start failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

// handleAuthCallback completes the OIDC code exchange and issues a cookie.
func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OIDC == nil {
		http.Error(w, "oidc not enabled", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "auth error: "+e, http.StatusUnauthorized)
		return
	}
	subject, err := s.cfg.OIDC.Exchange(r.Context(), q.Get("code"), q.Get("state"))
	if err != nil {
		s.log.Warn("oidc exchange failed", "err", err)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}
	sess, err := s.sessions.Issue(subject)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	auth.SetCookie(w, sess, isTLS(r))
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAgentWS accepts an agent connection after verifying the PSK bearer.
// Two roles are supported:
//   - role=monitor: one long-lived connection carries N observed sessions; each
//     FrameObserved snapshot is diffed against the current set (register new,
//     remove absent). Used by the monitor daemon.
//   - default (legacy wrap): one connection == one managed session; the socket
//     registers a single session and pumps its PTY frames.
func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	if !s.agentAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Query().Get("role") == "monitor" {
		s.handleMonitorWS(w, r)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("agent ws accept failed", "err", err)
		return
	}
	// Generous read limit for PTY output frames.
	c.SetReadLimit(1 << 20)

	ctx := r.Context()
	send := func(f protocol.Frame) error {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		return c.Write(wctx, websocket.MessageText, data)
	}

	sess := s.reg.Register(sessionID, send)
	s.log.Info("agent connected", "session", sessionID)
	defer func() {
		s.reg.Remove(sessionID)
		s.log.Info("agent disconnected", "session", sessionID)
		c.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) && !errors.Is(err, context.Canceled) {
				s.log.Debug("agent read ended", "session", sessionID, "err", err)
			}
			return
		}
		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			s.log.Warn("bad agent frame", "session", sessionID, "err", err)
			continue
		}
		s.dispatchAgentFrame(sess, f)
	}
}

// handleMonitorWS serves a role=monitor connection: the monitor daemon pushes
// FrameObserved snapshots and may receive FrameSpawn control frames. Each
// snapshot is reconciled against this connection's observed-session set (keyed
// by a per-connection connID) so the dashboard reflects processes appearing and
// exiting. All of this connection's observed sessions are torn down when the
// socket drops.
func (s *Server) handleMonitorWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("monitor ws accept failed", "err", err)
		return
	}
	c.SetReadLimit(1 << 20)

	ctx := r.Context()
	send := func(f protocol.Frame) error {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		return c.Write(wctx, websocket.MessageText, data)
	}

	device := r.URL.Query().Get("device")
	connID := newConnID()
	s.log.Info("monitor connected", "device", device, "conn", connID)
	defer func() {
		s.reg.RemoveObservedByConn(connID)
		s.log.Info("monitor disconnected", "device", device, "conn", connID)
		c.Close(websocket.StatusNormalClosure, "")
	}()

	// The daemon announces its spawnable roots once at connect, before any
	// observed sessions exist. Cache them so they can be (re)applied to this
	// connection's sessions after every snapshot reconcile.
	var projects []protocol.Project

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) && !errors.Is(err, context.Canceled) {
				s.log.Debug("monitor read ended", "device", device, "err", err)
			}
			return
		}
		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			s.log.Warn("bad monitor frame", "device", device, "err", err)
			continue
		}
		switch f.Type {
		case protocol.FrameObserved:
			s.reg.ReconcileObserved(connID, f.Observed, send)
			if len(projects) > 0 {
				s.reg.SetProjectsByConn(connID, projects)
			}
		case protocol.FrameProjects:
			projects = f.Projects
			s.reg.SetProjectsByConn(connID, projects)
		default:
			s.log.Debug("ignoring monitor frame", "type", f.Type)
		}
	}
}

func (s *Server) dispatchAgentFrame(sess *registry.Session, f protocol.Frame) {
	switch f.Type {
	case protocol.FrameHeartbeat:
		if f.Heartbeat != nil {
			s.reg.Heartbeat(sess.ID, *f.Heartbeat)
		}
	case protocol.FrameOutput:
		sess.PublishOutput(f.Output)
	case protocol.FrameProjects:
		s.reg.SetProjects(sess.ID, f.Projects)
	case protocol.FrameExit:
		// Mark exited; the deferred Remove will clean up on socket close.
		hb := sess.Latest
		hb.State = protocol.StateExited
		s.reg.Heartbeat(sess.ID, hb)
	default:
		s.log.Debug("ignoring agent frame", "type", f.Type)
	}
}

// agentAuthorized checks the Bearer PSK in constant time.
func (s *Server) agentAuthorized(r *http.Request) bool {
	if s.cfg.AgentToken == "" {
		return false // never allow agents when no token is configured
	}
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return false
	}
	got := strings.TrimPrefix(h, p)
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AgentToken)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// newConnID returns a short random identifier for a monitor WebSocket
// connection, used to scope which observed sessions that connection owns.
func newConnID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "conn-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "conn-" + hex.EncodeToString(b[:])
}
