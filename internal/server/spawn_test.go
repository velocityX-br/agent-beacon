package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/local/agent-beacon/internal/auth"
	"github.com/local/agent-beacon/pkg/protocol"
)

// TestSpawnRoutesToDeviceAgent verifies POST /api/v1/spawn finds a live session
// on the target device and delivers a spawn control frame to that agent.
func TestSpawnRoutesToDeviceAgent(t *testing.T) {
	s := New(Config{AgentToken: "psk", AuthProvider: string(auth.ModeNone), HeartbeatTTL: time.Minute}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// Register a fake agent session on device "box1" whose sender records frames.
	got := make(chan protocol.Frame, 1)
	sess := s.reg.Register("sess-x", func(f protocol.Frame) error {
		got <- f
		return nil
	})
	s.reg.Heartbeat(sess.ID, protocol.Heartbeat{Device: "box1", State: protocol.StateRunning})

	body := `{"device":"box1","project_path":"/tmp/repo","command":"claude","worktree_branch":"feature/x","worktree_location":"sibling"}`
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("spawn status = %d, want 202", resp.StatusCode)
	}

	select {
	case f := <-got:
		if f.Type != protocol.FrameSpawn {
			t.Fatalf("frame type = %q, want spawn", f.Type)
		}
		if f.Spawn == nil || f.Spawn.ProjectPath != "/tmp/repo" || f.Spawn.WorktreeBranch != "feature/x" {
			t.Fatalf("spawn payload = %+v, want project /tmp/repo branch feature/x", f.Spawn)
		}
	case <-time.After(time.Second):
		t.Fatal("no spawn frame delivered to agent")
	}
}

// TestSpawnUnknownDevice returns 404 when no agent is on the target device.
func TestSpawnUnknownDevice(t *testing.T) {
	s := New(Config{AgentToken: "psk", AuthProvider: string(auth.ModeNone), HeartbeatTTL: time.Minute}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	body := `{"device":"ghost","project_path":"/tmp/repo"}`
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("spawn status = %d, want 404", resp.StatusCode)
	}
}

// TestSpawnRequiresAuth ensures the endpoint is gated by browser auth.
func TestSpawnRequiresAuth(t *testing.T) {
	hash, err := auth.HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	ts := newAuthServer(t, Config{
		AgentToken:   "psk",
		AuthProvider: string(auth.ModePassword),
		Password:     auth.NewPasswordChecker(hash),
	})
	body := `{"device":"box1","project_path":"/tmp/repo"}`
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth spawn status = %d, want 401", resp.StatusCode)
	}
}
