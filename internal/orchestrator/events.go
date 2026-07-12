package orchestrator

import "time"

// EventKind enumerates the progress events an orchestration run can emit. They
// mirror the points where the worker/run loops already append to a result log,
// so a UI can render live progress without changing the batch return contract.
type EventKind string

const (
	// EventPlan is emitted once after the planner decomposes the task, with the
	// subtask count in Detail.
	EventPlan EventKind = "plan"
	// EventWorkerStart is emitted when a worker begins a subtask (after its
	// worktree is created).
	EventWorkerStart EventKind = "worker-start"
	// EventIteration is emitted at the top of each self-repair iteration.
	EventIteration EventKind = "iteration"
	// EventAgent is emitted around the headless coding-agent invocation
	// (including agent errors, which are recoverable and fed back).
	EventAgent EventKind = "agent"
	// EventGate is emitted after the deterministic Gate 1 build/test/e2e run,
	// with Passed set.
	EventGate EventKind = "gate"
	// EventVerdict is emitted after the Gate 2 cross-model verifier judges the
	// work, with Passed set.
	EventVerdict EventKind = "verdict"
	// EventWorkerDone is emitted when a worker finishes (pass, budget
	// exhaustion, or error), with Passed set.
	EventWorkerDone EventKind = "worker-done"
	// EventLog carries an arbitrary human-readable line for the live log pane.
	EventLog EventKind = "log"
	// EventSelfRepair records a recoverable environment/infra failure the loop
	// auto-fixed by feeding it back for another iteration. Message is a short
	// reason; Detail is what was fed back.
	EventSelfRepair EventKind = "self-repair"
	// EventAuthNeeded is emitted when a dangerous operation was observed and
	// the run is paused awaiting explicit user authorization. Message is a
	// human summary; Detail is the matched command/rule. Carries ReqID so the
	// browser can echo it back on /respond.
	EventAuthNeeded EventKind = "auth-needed"
	// EventInputNeeded is emitted when the run is paused awaiting optional user
	// guidance (e.g. the iteration budget is exhausted). Carries ReqID.
	EventInputNeeded EventKind = "input-needed"
	// EventRunDone is emitted once when the whole run finishes, with Passed set.
	// It is the terminal event a subscriber can key on to stop streaming.
	EventRunDone EventKind = "run-done"
)

// Event is a single progress record from an orchestration run. It is JSON-safe
// for streaming to the browser over a WebSocket. SubtaskID/Branch scope the
// event to a worker when relevant; run-level events (plan, run-done) leave them
// empty. Passed is a pointer so "unknown/not-applicable" is distinguishable
// from an explicit false in gate/verdict/done events.
type Event struct {
	Kind      EventKind `json:"kind"`
	Time      time.Time `json:"time"`
	SubtaskID string    `json:"subtask_id,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Iteration int       `json:"iteration,omitempty"`
	Passed    *bool     `json:"passed,omitempty"`
	Message   string    `json:"message,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	// ReqID correlates a pause event (auth-needed/input-needed) with the
	// browser's response on /respond. Empty for all other events.
	ReqID string `json:"req_id,omitempty"`
}

// Emitter receives progress events from a run. The server implements this to
// fan events out to browser subscribers; the CLI leaves it nil (no-op).
type Emitter interface {
	Emit(Event)
}

// emit sends an event through cfg.Emitter when one is configured. It stamps the
// event time and is nil-safe: with no Emitter (the CLI path and all existing
// tests) it is a no-op, so instrumenting the loops changes no behavior.
func emit(cfg Config, ev Event) {
	if cfg.Emitter == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	cfg.Emitter.Emit(ev)
}

// boolPtr returns a pointer to b, for the Passed field of gate/verdict/done
// events.
func boolPtr(b bool) *bool { return &b }
