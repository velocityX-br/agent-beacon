// Package registry maintains the in-memory set of live agent sessions and groups
// them by device. It is safe for concurrent use. Session state lives in-process
// only, matching the single-replica design constraint.
package registry

import (
	"sort"
	"sync"
	"time"

	"github.com/local/agent-beacon/pkg/protocol"
)

// Sender delivers a control frame to the agent that owns a session. The server
// wires this to the session's WebSocket write side.
type Sender func(protocol.Frame) error

// maxScrollback caps the per-session raw PTY output buffer kept for replay to
// newly attaching browser terminals. Older bytes are dropped from the front.
const maxScrollback = 256 * 1024

// Session is the server-side view of one agent session (managed) or one
// observed process (observed).
type Session struct {
	ID        string
	Kind      protocol.SessionKind
	Latest    protocol.Heartbeat
	Projects  []protocol.Project
	FirstSeen time.Time
	LastSeen  time.Time
	send      Sender
	// connID identifies the owning monitor WebSocket for observed sessions, so
	// a disconnect can drop exactly the observed cards that connection reported.
	// Empty for managed sessions.
	connID string
	mu     sync.RWMutex
	// terminal fan-out: browser attach connections receive PTY output here.
	// Only meaningful for managed sessions; observed sessions never publish.
	subscribers map[int]chan []byte
	nextSubID   int
	// scrollback is the tail of recent raw PTY output, capped at maxScrollback,
	// replayed to a browser on attach so a TUI repaints correctly. Guarded by mu.
	scrollback []byte
	// termSizes records each attached browser's requested PTY size, keyed by the
	// same subscriber id used for fan-out. The effective PTY size is the
	// element-wise minimum across all non-zero entries.
	termSizes map[int]protocol.ResizeMsg
	// localSize is the RAW size the local terminal (iTerm2) reported via
	// FrameLocalSize. It caps the PTY ONLY when no browser is attached; once any
	// browser subscribes, the browser(s) drive the negotiated size and localSize
	// is excluded so a small iTerm2 window cannot shrink the dashboard terminal.
	localSize protocol.ResizeMsg
	// curSize is the last size actually pushed to the agent.
	curSize protocol.ResizeMsg
}

// Registry is the concurrent session store.
type Registry struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	ttl      time.Duration
}

// New returns a Registry that marks sessions stale after ttl without a heartbeat.
func New(ttl time.Duration) *Registry {
	return &Registry{
		sessions: make(map[string]*Session),
		ttl:      ttl,
	}
}

// Register adds (or replaces) a session with the given id and control sender,
// typically called when an agent WebSocket connects.
func (r *Registry) Register(id string, send Sender) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	s := &Session{
		ID:          id,
		FirstSeen:   now,
		LastSeen:    now,
		send:        send,
		subscribers: make(map[int]chan []byte),
		termSizes:   make(map[int]protocol.ResizeMsg),
	}
	s.Kind = protocol.KindManaged
	s.Latest.State = protocol.StateStarting
	r.sessions[id] = s
	return s
}

// ReconcileObserved applies a full observed snapshot from one monitor
// connection. connID identifies the monitor WebSocket so that, on disconnect,
// RemoveObservedByConn drops exactly the sessions that connection owned. Each
// heartbeat's ObservedID(device,pid) is the stable session id: present ids are
// added/updated, and any observed session previously owned by connID but absent
// from this snapshot is removed. send routes spawn requests back to the
// monitor's agent.
func (r *Registry) ReconcileObserved(connID string, snapshot []protocol.Heartbeat, send Sender) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	seen := make(map[string]struct{}, len(snapshot))
	for _, hb := range snapshot {
		dev := hb.Device
		if dev == "" {
			dev = "unknown"
		}
		id := protocol.ObservedID(dev, hb.Pid)
		seen[id] = struct{}{}
		s := r.sessions[id]
		if s == nil {
			s = &Session{
				ID:          id,
				FirstSeen:   now,
				subscribers: make(map[int]chan []byte),
				termSizes:   make(map[int]protocol.ResizeMsg),
			}
			r.sessions[id] = s
		}
		hb.Kind = protocol.KindObserved
		s.mu.Lock()
		s.Kind = protocol.KindObserved
		s.connID = connID
		s.send = send
		s.Latest = hb
		s.LastSeen = now
		s.mu.Unlock()
	}
	// Drop observed sessions this connection owned that are no longer present.
	for id, s := range r.sessions {
		s.mu.RLock()
		owned := s.Kind == protocol.KindObserved && s.connID == connID
		s.mu.RUnlock()
		if owned {
			if _, ok := seen[id]; !ok {
				delete(r.sessions, id)
				s.closeSubscribers()
			}
		}
	}
}

// RemoveObservedByConn drops every observed session owned by the given monitor
// connection. Called when a monitor WebSocket disconnects so its cards clear.
func (r *Registry) RemoveObservedByConn(connID string) {
	r.mu.Lock()
	var drop []*Session
	for id, s := range r.sessions {
		s.mu.RLock()
		owned := s.Kind == protocol.KindObserved && s.connID == connID
		s.mu.RUnlock()
		if owned {
			delete(r.sessions, id)
			drop = append(drop, s)
		}
	}
	r.mu.Unlock()
	for _, s := range drop {
		s.closeSubscribers()
	}
}

// SetProjectsByConn records the spawnable roots a monitor connection announced,
// applying them to every observed session that connection currently owns so the
// dashboard can offer them as spawn targets for that device.
func (r *Registry) SetProjectsByConn(connID string, ps []protocol.Project) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		s.mu.Lock()
		if s.Kind == protocol.KindObserved && s.connID == connID {
			s.Projects = ps
		}
		s.mu.Unlock()
	}
}

// Remove deletes a session (agent disconnected / process exited).
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	s := r.sessions[id]
	delete(r.sessions, id)
	r.mu.Unlock()
	if s != nil {
		s.closeSubscribers()
	}
}

// Heartbeat records the latest metadata for a session.
func (r *Registry) Heartbeat(id string, hb protocol.Heartbeat) {
	r.mu.RLock()
	s := r.sessions[id]
	r.mu.RUnlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Latest = hb
	s.LastSeen = time.Now()
	s.mu.Unlock()
}

// SetProjects records the spawnable repositories a session's host reported.
func (r *Registry) SetProjects(id string, ps []protocol.Project) {
	r.mu.RLock()
	s := r.sessions[id]
	r.mu.RUnlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Projects = ps
	s.mu.Unlock()
}

// Get returns a session by id.
func (r *Registry) Get(id string) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[id]
	return s, ok
}

// CloneTarget returns the cwd and device of a session, read under its lock.
func (s *Session) CloneTarget() (cwd, device string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Latest.CWD, s.Latest.Device
}

// DeviceGroup is a set of sessions sharing a device name, for the dashboard.
type DeviceGroup struct {
	Device   string             `json:"device"`
	Sessions []SessionView      `json:"sessions"`
	Projects []protocol.Project `json:"projects,omitempty"`
}

// SessionView is the JSON-safe snapshot of a session for the REST API.
type SessionView struct {
	ID         string                `json:"id"`
	Kind       protocol.SessionKind  `json:"kind"`
	Pid        int                   `json:"pid,omitempty"`
	Attachable bool                  `json:"attachable"`
	Heartbeat  protocol.Heartbeat    `json:"heartbeat"`
	State      protocol.SessionState `json:"state"`
	FirstSeen  time.Time             `json:"first_seen"`
	LastSeen   time.Time             `json:"last_seen"`
}

// Snapshot returns all sessions grouped by device, with stale sessions marked.
func (r *Registry) Snapshot() []DeviceGroup {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now()
	byDevice := map[string]*DeviceGroup{}
	for _, s := range r.sessions {
		s.mu.RLock()
		hb := s.Latest
		state := hb.State
		if state != protocol.StateExited && now.Sub(s.LastSeen) > r.ttl {
			state = protocol.StateStale
		}
		dev := hb.Device
		if dev == "" {
			dev = "unknown"
		}
		g := byDevice[dev]
		if g == nil {
			g = &DeviceGroup{Device: dev}
			byDevice[dev] = g
		}
		g.Sessions = append(g.Sessions, SessionView{
			ID:         s.ID,
			Kind:       s.Kind,
			Pid:        hb.Pid,
			Attachable: s.Kind == protocol.KindManaged,
			Heartbeat:  hb,
			State:      state,
			FirstSeen:  s.FirstSeen,
			LastSeen:   s.LastSeen,
		})
		// A device's projects are the union reported by any of its sessions;
		// take the longest list seen (agents report the same roots).
		if len(s.Projects) > len(g.Projects) {
			g.Projects = s.Projects
		}
		s.mu.RUnlock()
	}
	out := make([]DeviceGroup, 0, len(byDevice))
	for _, g := range byDevice {
		sort.Slice(g.Sessions, func(i, j int) bool {
			return g.Sessions[i].FirstSeen.Before(g.Sessions[j].FirstSeen)
		})
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

// SendInput forwards keystrokes to the session's PTY via the agent WS.
func (s *Session) SendInput(b []byte) error {
	return s.send(protocol.Frame{Type: protocol.FrameInput, SessionID: s.ID, Input: b})
}

// SendResize forwards a PTY resize to the agent.
func (s *Session) SendResize(rows, cols uint16) error {
	return s.send(protocol.Frame{
		Type:      protocol.FrameResize,
		SessionID: s.ID,
		Resize:    &protocol.ResizeMsg{Rows: rows, Cols: cols},
	})
}

// SendSpawn asks the agent that owns this session to launch a new session.
func (s *Session) SendSpawn(msg protocol.SpawnMsg) error {
	return s.send(protocol.Frame{
		Type:      protocol.FrameSpawn,
		SessionID: s.ID,
		Spawn:     &msg,
	})
}

// SendKill asks the agent that owns this session to gracefully terminate its
// wrapped process (SIGTERM). The session ends once the child exits.
func (s *Session) SendKill() error {
	return s.send(protocol.Frame{Type: protocol.FrameKill, SessionID: s.ID})
}

// IsManaged reports whether this is an interactive managed session (has a PTY
// and can be killed), as opposed to a read-only observed process.
func (s *Session) IsManaged() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Kind == protocol.KindManaged
}

// AnyOnDevice returns a live session whose latest heartbeat reports the given
// device name, preferring an observed (monitor daemon) session since its send
// routes to the long-lived monitor connection that can launch new processes on
// that host. Falls back to any session on the device. Returns nil if none match.
func (r *Registry) AnyOnDevice(device string) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var fallback *Session
	for _, s := range r.sessions {
		s.mu.RLock()
		dev := s.Latest.Device
		kind := s.Kind
		s.mu.RUnlock()
		if dev == "" {
			dev = "unknown"
		}
		if dev != device {
			continue
		}
		if kind == protocol.KindObserved {
			return s
		}
		if fallback == nil {
			fallback = s
		}
	}
	return fallback
}

// HasManagedOnDeviceCwd reports whether a managed session is currently live on
// the given device with the given working directory. Session recovery uses this
// to skip re-spawning a workspace whose session is already running — the case
// where the server restarted but the agent survived (no reboot) and reconnected
// on its own.
func (r *Registry) HasManagedOnDeviceCwd(device, cwd string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		s.mu.RLock()
		match := s.Kind == protocol.KindManaged && s.Latest.CWD == cwd && s.Latest.Device == device
		s.mu.RUnlock()
		if match {
			return true
		}
	}
	return false
}

// Subscribe registers a browser terminal listener. It returns the subscriber
// id, a snapshot of the current scrollback (to replay so a TUI repaints), the
// fan-out channel, and an unsubscribe func. The unsubscribe func drops the
// subscriber's fan-out channel and its size entry, then recomputes the
// negotiated PTY size; if detaching allows the PTY to grow back to the
// remaining browsers' minimum, it pushes a resize to the agent. Output frames
// from the agent are fanned out to all subs.
func (s *Session) Subscribe() (subID int, backlog []byte, ch <-chan []byte, unsubscribe func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSubID
	s.nextSubID++
	c := make(chan []byte, 256)
	s.subscribers[id] = c
	if len(s.scrollback) > 0 {
		backlog = make([]byte, len(s.scrollback))
		copy(backlog, s.scrollback)
	}
	unsub := func() {
		s.mu.Lock()
		if sc, ok := s.subscribers[id]; ok {
			delete(s.subscribers, id)
			close(sc)
		}
		delete(s.termSizes, id)
		rows, cols, changed := s.recomputeSizeLocked()
		s.mu.Unlock()
		if changed {
			_ = s.SendResize(rows, cols)
		}
	}
	return id, backlog, c, unsub
}

// SetSubscriberSize records the PTY size requested by the given browser
// subscriber and recomputes the negotiated (minimum) size across all attached
// browsers. It returns the negotiated dimensions and whether they changed from
// the size last pushed to the agent. The caller should SendResize when changed.
func (s *Session) SetSubscriberSize(subID int, rows, cols uint16) (nrows, ncols uint16, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.termSizes[subID] = protocol.ResizeMsg{Rows: rows, Cols: cols}
	return s.recomputeSizeLocked()
}

// SetLocalSize records the RAW size reported by the local terminal (iTerm2) and
// recomputes the negotiated (minimum) size across all clients. It mirrors
// SetSubscriberSize: the caller should SendResize when changed.
func (s *Session) SetLocalSize(rows, cols uint16) (nrows, ncols uint16, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localSize = protocol.ResizeMsg{Rows: rows, Cols: cols}
	return s.recomputeSizeLocked()
}

// recomputeSizeLocked computes the negotiated PTY size and updates curSize if it
// changed. Callers must hold s.mu. When no client reports a usable size, curSize
// is left unchanged.
//
// The browser drives the size whenever any browser terminal is attached: the
// negotiated size is the element-wise minimum across the attached browsers only,
// and the local terminal (iTerm2) size is intentionally EXCLUDED. This lets the
// dashboard fill its panel regardless of how large or small the local iTerm2
// window is (a small iTerm2 no longer caps the browser). The local terminal is
// folded in only when no browser is attached, so a purely local managed session
// still matches the physical terminal.
func (s *Session) recomputeSizeLocked() (rows, cols uint16, changed bool) {
	var minRows, minCols uint16
	fold := func(sz protocol.ResizeMsg) {
		if sz.Rows == 0 || sz.Cols == 0 {
			return
		}
		if minRows == 0 || sz.Rows < minRows {
			minRows = sz.Rows
		}
		if minCols == 0 || sz.Cols < minCols {
			minCols = sz.Cols
		}
	}
	for _, sz := range s.termSizes {
		fold(sz)
	}
	// Only let the local terminal cap the PTY when no browser is attached; a
	// browser attach hands sizing authority to the browser(s).
	if len(s.termSizes) == 0 {
		fold(s.localSize)
	}
	if minRows == 0 || minCols == 0 {
		// No client is reporting a usable size; keep the PTY as-is.
		return s.curSize.Rows, s.curSize.Cols, false
	}
	if minRows == s.curSize.Rows && minCols == s.curSize.Cols {
		return minRows, minCols, false
	}
	s.curSize = protocol.ResizeMsg{Rows: minRows, Cols: minCols}
	return minRows, minCols, true
}

// PublishOutput fans PTY output out to all subscribed browser terminals and
// appends it to the session scrollback for replay on later attaches. Slow
// subscribers drop bytes rather than blocking the agent read loop.
func (s *Session) PublishOutput(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subscribers {
		select {
		case ch <- b:
		default:
		}
	}
	s.scrollback = append(s.scrollback, b...)
	if len(s.scrollback) > maxScrollback {
		// Keep the tail; drop from the front.
		s.scrollback = append([]byte(nil), s.scrollback[len(s.scrollback)-maxScrollback:]...)
	}
}

func (s *Session) closeSubscribers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subscribers {
		delete(s.subscribers, id)
		close(ch)
	}
}
