// Package protocol defines the wire format shared between the agent-beacon
// server and the agent-beacon session wrapper. Messages travel as JSON frames
// over a single WebSocket connection per session.
package protocol

import (
	"strconv"
	"time"
)

// FrameType discriminates the payload carried by a Frame.
type FrameType string

const (
	// FrameHeartbeat is sent agent->server periodically with session metadata.
	FrameHeartbeat FrameType = "heartbeat"
	// FrameOutput is sent agent->server carrying raw PTY output bytes.
	FrameOutput FrameType = "output"
	// FrameInput is sent server->agent carrying keystrokes for the PTY stdin.
	FrameInput FrameType = "input"
	// FrameResize is sent server->agent to resize the PTY window.
	FrameResize FrameType = "resize"
	// FrameSpawn is sent server->agent requesting a new session be launched.
	FrameSpawn FrameType = "spawn"
	// FrameProjects is sent agent->server listing spawnable repositories.
	FrameProjects FrameType = "projects"
	// FrameExit is sent agent->server when the wrapped process ends.
	FrameExit FrameType = "exit"
	// FrameObserved is sent monitor->server carrying a full snapshot of the
	// claude processes currently observed on the device. The server diffs each
	// snapshot against the previous one to add/remove observed sessions.
	FrameObserved FrameType = "observed"
)

// SessionKind distinguishes a passively observed process (read-only, no PTY)
// from an interactive session the user explicitly opened (bidirectional PTY).
type SessionKind string

const (
	// KindObserved is a read-only process discovered by scan and/or hook. It has
	// metadata only: no PTY, no terminal attach.
	KindObserved SessionKind = "observed"
	// KindManaged is an interactive session launched under our PTY. It has a full
	// bidirectional terminal. This is the default for a bare Heartbeat (Kind == "")
	// so legacy `session --` agents and existing tests keep working unchanged.
	KindManaged SessionKind = "managed"
)

// Session data-source hints, surfaced in the UI.
const (
	// SourceScan marks metadata derived purely from the process table scan.
	SourceScan = "scan"
	// SourceHook marks metadata enriched by a Claude Code hook report.
	SourceHook = "hook"
)

// ObservedID derives a stable session id for an observed process. The monitor
// daemon and the server both compute the id this way so a process keeps the same
// card identity across scans (no flicker) and the server can diff snapshots.
func ObservedID(device string, pid int) string {
	return device + "/observed/" + strconv.Itoa(pid)
}

// Frame is the envelope for every WebSocket message in either direction.
// Exactly one of the payload pointers is non-nil, matching Type.
type Frame struct {
	Type      FrameType    `json:"type"`
	SessionID string       `json:"session_id,omitempty"`
	Heartbeat *Heartbeat   `json:"heartbeat,omitempty"`
	Output    []byte       `json:"output,omitempty"`
	Input     []byte       `json:"input,omitempty"`
	Resize    *ResizeMsg   `json:"resize,omitempty"`
	Spawn     *SpawnMsg    `json:"spawn,omitempty"`
	Projects  []Project    `json:"projects,omitempty"`
	Exit      *ExitMsg     `json:"exit,omitempty"`
	// Observed is the full set of observed processes in a FrameObserved snapshot.
	// Each element carries Kind == KindObserved and a Pid.
	Observed []Heartbeat `json:"observed,omitempty"`
}

// SessionState is the coarse lifecycle/activity state of a session, surfaced
// on the dashboard card.
type SessionState string

const (
	StateStarting SessionState = "starting"
	StateRunning  SessionState = "running"
	StateWaiting  SessionState = "waiting" // agent is waiting on the user (e.g. permission prompt)
	StateIdle     SessionState = "idle"
	StateExited   SessionState = "exited"
	StateStale    SessionState = "stale" // no heartbeat within TTL
)

// Heartbeat carries the metadata a session reports on each tick. Fields mirror
// the documented ai-beacon heartbeat surface but the struct is our own.
type Heartbeat struct {
	Device      string       `json:"device"`
	Command     string       `json:"command"`      // the wrapped command line, e.g. "claude"
	Model       string       `json:"model,omitempty"`
	Tokens      int          `json:"tokens,omitempty"`
	ContextPct  float64      `json:"context_pct,omitempty"`
	State       SessionState `json:"state"`
	Task        string       `json:"task,omitempty"`   // short description of current work
	Branch      string       `json:"branch,omitempty"`
	CWD         string       `json:"cwd,omitempty"`
	GitHubOwner string       `json:"github_owner,omitempty"`
	GitHubRepo  string       `json:"github_repo,omitempty"`
	PRNumber    int          `json:"pr_number,omitempty"`
	PRState     string       `json:"pr_state,omitempty"`
	DevicePinned bool        `json:"device_pinned,omitempty"` // name came from flag/env -> immune to rename
	// Kind marks observed (read-only) vs managed (interactive) sessions. Empty is
	// treated as KindManaged for backward compatibility with wrap agents.
	Kind SessionKind `json:"kind,omitempty"`
	// Pid is the OS process id, set for observed processes.
	Pid int `json:"pid,omitempty"`
	// SessionSource is "scan" or "hook", indicating how observed metadata was
	// obtained (see SourceScan / SourceHook).
	SessionSource string    `json:"session_source,omitempty"`
	SentAt        time.Time `json:"sent_at"`
}

// ResizeMsg carries new PTY dimensions.
type ResizeMsg struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// SpawnMsg is a dashboard-originated request to start a new session on the agent.
type SpawnMsg struct {
	ProjectPath      string `json:"project_path"`
	Command          string `json:"command"`           // e.g. "claude"; empty -> agent default
	WorktreeBranch   string `json:"worktree_branch,omitempty"`
	WorktreeLocation string `json:"worktree_location,omitempty"` // "sibling" | "subdirectory"
}

// Project is a spawnable repository discovered under a projects root.
type Project struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Root string `json:"root"` // which configured root it was found under
}

// ExitMsg reports the wrapped process's termination.
type ExitMsg struct {
	Code int    `json:"code"`
	Err  string `json:"err,omitempty"`
}
