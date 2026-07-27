package server

import (
	"testing"
	"time"

	"github.com/local/agent-beacon/internal/auth"
	"github.com/local/agent-beacon/pkg/protocol"
)

// TestWithContinue exercises the claude --continue rewrite matrix.
func TestWithContinue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude", "claude --continue"},
		{"claude --continue", "claude --continue"}, // idempotent
		{"claude -c", "claude -c"},                 // -c shorthand unchanged
		{"claude --model sonnet", "claude --model sonnet --continue"},
		{"aider", "aider"},                                            // non-claude verbatim
		{"", "claude --continue"},                                     // empty defaults to claude
		{"/usr/local/bin/claude", "/usr/local/bin/claude --continue"}, // base name matched
	}
	for _, c := range cases {
		if got := withContinue(c.in); got != c.want {
			t.Errorf("withContinue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// newRecoverTestServer builds a server with an enabled recovery store rooted at
// a temp dir and returns it.
func newRecoverTestServer(t *testing.T) *Server {
	t.Helper()
	return New(Config{
		AgentToken:              "psk",
		AuthProvider:            string(auth.ModeNone),
		HeartbeatTTL:            time.Minute,
		SessionRecoveryStateDir: t.TempDir(),
	}, nil)
}

// TestRecoverDeviceSpawnsPending re-spawns a pending record when no live
// managed session occupies the workspace, and asserts the SpawnMsg shape.
func TestRecoverDeviceSpawnsPending(t *testing.T) {
	s := newRecoverTestServer(t)
	s.recover.saveHeartbeat(protocol.Heartbeat{Device: "box1", CWD: "/repo/a", Command: "claude"})

	var got []protocol.SpawnMsg
	s.recoverDevice("box1", func(m protocol.SpawnMsg) error {
		got = append(got, m)
		return nil
	})

	if len(got) != 1 {
		t.Fatalf("expected exactly one spawn, got %d", len(got))
	}
	m := got[0]
	if m.ProjectPath != "/repo/a" {
		t.Errorf("ProjectPath = %q, want /repo/a", m.ProjectPath)
	}
	if !m.TrustedCwd {
		t.Error("TrustedCwd must be true for recovery spawns")
	}
	if m.WorktreeBranch != "" {
		t.Errorf("WorktreeBranch = %q, want empty (reuse cwd)", m.WorktreeBranch)
	}
	if m.Command != "claude --continue" {
		t.Errorf("Command = %q, want claude --continue", m.Command)
	}
}

// TestRecoverDeviceSkipsLiveSession verifies that a workspace already occupied
// by a live managed session (server restart without reboot) is not re-spawned.
func TestRecoverDeviceSkipsLiveSession(t *testing.T) {
	s := newRecoverTestServer(t)
	s.recover.saveHeartbeat(protocol.Heartbeat{Device: "box1", CWD: "/repo/a", Command: "claude"})

	// Register a live managed session in the same workspace.
	sess := s.reg.Register("sess-1", func(protocol.Frame) error { return nil })
	s.reg.Heartbeat(sess.ID, protocol.Heartbeat{Device: "box1", CWD: "/repo/a", State: protocol.StateRunning})

	var spawns int
	s.recoverDevice("box1", func(protocol.SpawnMsg) error { spawns++; return nil })
	if spawns != 0 {
		t.Fatalf("expected no spawn for live workspace, got %d", spawns)
	}
}

// TestRecoverDeviceRecoversOnce ensures a record is drained after the first
// recoverDevice call: a second call spawns nothing.
func TestRecoverDeviceRecoversOnce(t *testing.T) {
	s := newRecoverTestServer(t)
	s.recover.saveHeartbeat(protocol.Heartbeat{Device: "box1", CWD: "/repo/a", Command: "claude"})

	var spawns int
	spawn := func(protocol.SpawnMsg) error { spawns++; return nil }
	s.recoverDevice("box1", spawn)
	s.recoverDevice("box1", spawn)
	if spawns != 1 {
		t.Fatalf("expected exactly one spawn across two calls, got %d", spawns)
	}
}
