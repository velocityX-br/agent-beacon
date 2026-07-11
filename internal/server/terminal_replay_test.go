package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/pkg/protocol"
)

// TestTerminalReplaysScrollbackOnReattach verifies the browser terminal WS
// replays accumulated PTY output as an initial binary frame when a browser
// (re)attaches — the fix for detach/attach losing history. It drives the real
// handleTerminalWS handler over an httptest server.
func TestTerminalReplaysScrollbackOnReattach(t *testing.T) {
	s, ts := newTestServerNoAuth(t, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connect an agent WS so the session is registered as managed with a live
	// control sender (its PTY input/resize would route back over this WS).
	agentURL := wsURL(ts.URL) + "/api/v1/agent/connect?session_id=sess-term"
	agent, _, err := websocket.Dial(ctx, agentURL, &websocket.DialOptions{
		HTTPHeader: mustAuthHeader("test-psk"),
	})
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer agent.Close(websocket.StatusNormalClosure, "")

	// Register a heartbeat so the session shows up as managed/running.
	hb := protocol.Frame{
		Type:      protocol.FrameHeartbeat,
		SessionID: "sess-term",
		Heartbeat: &protocol.Heartbeat{
			Device: "laptop", Command: "claude", State: protocol.StateRunning, SentAt: time.Now(),
		},
	}
	hbData, _ := json.Marshal(hb)
	if err := agent.Write(ctx, websocket.MessageText, hbData); err != nil {
		t.Fatalf("write hb: %v", err)
	}

	// Wait until the session is present in the registry.
	waitFor(t, 2*time.Second, func() bool {
		_, ok := s.reg.Get("sess-term")
		return ok
	})
	sess, _ := s.reg.Get("sess-term")

	// Simulate PTY output before any browser is attached — this must be captured
	// in scrollback even though there is no subscriber yet.
	sess.PublishOutput([]byte("line-one\r\n"))

	termURL := wsURL(ts.URL) + "/api/v1/terminal/sess-term"

	// First attach: read the initial backlog frame.
	term1, _, err := websocket.Dial(ctx, termURL, nil)
	if err != nil {
		t.Fatalf("terminal dial 1: %v", err)
	}
	backlog1 := readBinaryFrame(t, ctx, term1)
	if string(backlog1) != "line-one\r\n" {
		t.Fatalf("first attach backlog = %q, want %q", backlog1, "line-one\r\n")
	}
	// Detach.
	term1.Close(websocket.StatusNormalClosure, "")

	// More output arrives while detached.
	sess.PublishOutput([]byte("line-two\r\n"))

	// Re-attach: the replayed backlog must contain the FULL history.
	term2, _, err := websocket.Dial(ctx, termURL, nil)
	if err != nil {
		t.Fatalf("terminal dial 2: %v", err)
	}
	defer term2.Close(websocket.StatusNormalClosure, "")
	backlog2 := readBinaryFrame(t, ctx, term2)
	if string(backlog2) != "line-one\r\nline-two\r\n" {
		t.Fatalf("re-attach backlog = %q, want %q", backlog2, "line-one\r\nline-two\r\n")
	}
}

func mustAuthHeader(token string) map[string][]string {
	return map[string][]string{"Authorization": {"Bearer " + token}}
}

// newTestServerNoAuth mirrors the live server's --auth none mode so browser
// terminal WS connections are authorized without a session cookie.
func newTestServerNoAuth(t *testing.T, ttl time.Duration) (*Server, *httptest.Server) {
	t.Helper()
	s := New(Config{AgentToken: "test-psk", HeartbeatTTL: ttl, AuthProvider: "none"}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func readBinaryFrame(t *testing.T, ctx context.Context, c *websocket.Conn) []byte {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	typ, data, err := c.Read(rctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("frame type = %v, want binary", typ)
	}
	return data
}
