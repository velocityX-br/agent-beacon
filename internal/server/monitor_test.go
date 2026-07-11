package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/internal/auth"
	"github.com/local/agent-beacon/internal/registry"
	"github.com/local/agent-beacon/pkg/protocol"
)

// dialMonitor opens a role=monitor agent WebSocket against ts and returns it.
func dialMonitor(t *testing.T, wsURL, token, device string) *websocket.Conn {
	t.Helper()
	u := strings.Replace(wsURL, "http", "ws", 1) +
		"/api/v1/agent/connect?role=monitor&device=" + device
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("dial monitor: %v", err)
	}
	return c
}

// writeFrame marshals and sends a frame on c.
func writeFrame(t *testing.T, c *websocket.Conn, f protocol.Frame) {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// findObserved polls the snapshot for an observed session with the given pid on
// device, returning it. Frame processing is asynchronous to the WS write, so we
// poll briefly.
func findObserved(s *Server, device string, pid int) (registry.SessionView, bool) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, g := range s.reg.Snapshot() {
			if g.Device != device {
				continue
			}
			for _, sv := range g.Sessions {
				if sv.Pid == pid {
					return sv, true
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return registry.SessionView{}, false
}

// waitGone polls until the observed pid disappears from the snapshot.
func waitGone(s *Server, device string, pid int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gone := true
		for _, g := range s.reg.Snapshot() {
			if g.Device != device {
				continue
			}
			for _, sv := range g.Sessions {
				if sv.Pid == pid {
					gone = false
				}
			}
		}
		if gone {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestMonitorObservedLifecycle verifies a role=monitor connection registers
// observed sessions from a snapshot, removes ones that drop out of a later
// snapshot, and clears all of them when the socket disconnects.
func TestMonitorObservedLifecycle(t *testing.T) {
	s := New(Config{AgentToken: "psk", AuthProvider: string(auth.ModeNone), HeartbeatTTL: time.Minute}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := dialMonitor(t, ts.URL, "psk", "box1")

	// First snapshot: two observed processes.
	writeFrame(t, c, protocol.Frame{
		Type: protocol.FrameObserved,
		Observed: []protocol.Heartbeat{
			{Device: "box1", Pid: 111, Command: "claude", State: protocol.StateRunning},
			{Device: "box1", Pid: 222, Command: "claude", State: protocol.StateRunning},
		},
	})

	v1, ok := findObserved(s, "box1", 111)
	if !ok {
		t.Fatal("observed pid 111 not registered")
	}
	if v1.Kind != protocol.KindObserved {
		t.Fatalf("pid 111 kind = %q, want observed", v1.Kind)
	}
	if v1.Attachable {
		t.Fatal("observed session must not be attachable")
	}
	if _, ok := findObserved(s, "box1", 222); !ok {
		t.Fatal("observed pid 222 not registered")
	}

	// Second snapshot: pid 222 gone, pid 111 remains.
	writeFrame(t, c, protocol.Frame{
		Type: protocol.FrameObserved,
		Observed: []protocol.Heartbeat{
			{Device: "box1", Pid: 111, Command: "claude", State: protocol.StateRunning},
		},
	})
	if !waitGone(s, "box1", 222) {
		t.Fatal("observed pid 222 should have been removed")
	}
	if _, ok := findObserved(s, "box1", 111); !ok {
		t.Fatal("observed pid 111 should still be present")
	}

	// Disconnect clears all observed sessions this connection owned.
	c.Close(websocket.StatusNormalClosure, "")
	if !waitGone(s, "box1", 111) {
		t.Fatal("observed sessions should clear on monitor disconnect")
	}
}

// TestMonitorRejectsBadToken ensures the monitor endpoint enforces the PSK.
func TestMonitorRejectsBadToken(t *testing.T) {
	s := New(Config{AgentToken: "psk", AuthProvider: string(auth.ModeNone), HeartbeatTTL: time.Minute}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	u := strings.Replace(ts.URL, "http", "ws", 1) + "/api/v1/agent/connect?role=monitor&device=box1"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}},
	})
	if err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected monitor dial with bad token to fail")
	}
}
