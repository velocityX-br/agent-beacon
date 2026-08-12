package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/pkg/protocol"
)

// TestTerminalWSRejectsCrossOrigin verifies the OriginPatterns guard on the
// browser-facing terminal WebSocket: a handshake whose Origin host does not
// match the server Host is rejected, while a same-origin handshake succeeds.
// This is the CSRF protection that stops a page a victim visits from opening a
// socket into their authenticated session.
func TestTerminalWSRejectsCrossOrigin(t *testing.T) {
	s, ts := newTestServerNoAuth(t, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Register a managed session via the agent WS so the terminal endpoint has
	// something to attach to.
	agentURL := wsURL(ts.URL) + "/api/v1/agent/connect?session_id=sess-origin"
	agent, _, err := websocket.Dial(ctx, agentURL, &websocket.DialOptions{
		HTTPHeader: mustAuthHeader("test-psk"),
	})
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer agent.Close(websocket.StatusNormalClosure, "")
	hb := protocol.Frame{
		Type:      protocol.FrameHeartbeat,
		SessionID: "sess-origin",
		Heartbeat: &protocol.Heartbeat{
			Device: "laptop", Command: "claude", State: protocol.StateRunning, SentAt: time.Now(),
		},
	}
	hbData, _ := json.Marshal(hb)
	if err := agent.Write(ctx, websocket.MessageText, hbData); err != nil {
		t.Fatalf("write hb: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, ok := s.reg.Get("sess-origin")
		return ok
	})

	termURL := wsURL(ts.URL) + "/api/v1/terminal/sess-origin"

	// Cross-origin handshake: forge an Origin from a different host. The Accept
	// call rejects it (HTTP 403), so Dial returns an error.
	badConn, _, err := websocket.Dial(ctx, termURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Origin": {"https://evil.example.com"}},
	})
	if err == nil {
		badConn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("cross-origin terminal handshake succeeded, want rejection")
	}

	// Same-origin handshake: set Origin to the server's own scheme+host. This
	// must be accepted (we can read the initial state without error).
	goodConn, _, err := websocket.Dial(ctx, termURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("same-origin terminal handshake failed: %v", err)
	}
	goodConn.Close(websocket.StatusNormalClosure, "")
}
