package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/pkg/protocol"
)

// TestBrowserDrivesSizeOverLocal verifies the browser drives the PTY size when
// attached: the agent-reported local terminal size is EXCLUDED from negotiation
// once any browser subscribes, so a browser LARGER than the local terminal grows
// the PTY past the local size (a small iTerm2 no longer caps the dashboard).
func TestBrowserDrivesSizeOverLocal(t *testing.T) {
	s, ts := newTestServerNoAuth(t, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connect an agent WS and register it as a managed session via heartbeat.
	agentURL := wsURL(ts.URL) + "/api/v1/agent/connect?session_id=sess-ls"
	agent, _, err := websocket.Dial(ctx, agentURL, &websocket.DialOptions{
		HTTPHeader: mustAuthHeader("test-psk"),
	})
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer agent.Close(websocket.StatusNormalClosure, "")

	hb := protocol.Frame{
		Type:      protocol.FrameHeartbeat,
		SessionID: "sess-ls",
		Heartbeat: &protocol.Heartbeat{
			Device: "laptop", Command: "claude", State: protocol.StateRunning, SentAt: time.Now(),
		},
	}
	hbData, _ := json.Marshal(hb)
	if err := agent.Write(ctx, websocket.MessageText, hbData); err != nil {
		t.Fatalf("write hb: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, ok := s.reg.Get("sess-ls")
		return ok
	})

	// Agent reports a SMALL local terminal size (24x80): a not-fully-expanded
	// iTerm2 window. With no browsers yet this is the only client.
	localSize := protocol.Frame{
		Type:      protocol.FrameLocalSize,
		SessionID: "sess-ls",
		Resize:    &protocol.ResizeMsg{Rows: 24, Cols: 80},
	}
	lsData, _ := json.Marshal(localSize)
	if err := agent.Write(ctx, websocket.MessageText, lsData); err != nil {
		t.Fatalf("write local size: %v", err)
	}

	// Attach a LARGER browser (50x160). Because a browser is now attached, the
	// local size is excluded from negotiation, so the negotiated size becomes
	// the browser's 50x160 (NOT capped to the local 24x80). The server must push
	// a FrameResize to the agent at 50x160.
	termURL := wsURL(ts.URL) + "/api/v1/terminal/sess-ls"
	term, _, err := websocket.Dial(ctx, termURL, nil)
	if err != nil {
		t.Fatalf("terminal dial: %v", err)
	}
	defer term.Close(websocket.StatusNormalClosure, "")

	resizeReq, _ := json.Marshal(termClientMsg{Type: "resize", Rows: 50, Cols: 160})
	if err := term.Write(ctx, websocket.MessageText, resizeReq); err != nil {
		t.Fatalf("browser resize write: %v", err)
	}

	// The agent WS must receive a FrameResize at the browser size (50x160),
	// proving the small local iTerm2 no longer caps the browser terminal.
	if !waitResize(t, ctx, agent, 50, 160) {
		t.Fatalf("agent never received a resize at the browser size (50,160); local size wrongly capped it")
	}
}

// waitResize reads text frames from the agent WS until a FrameResize with the
// wanted dimensions arrives, returning true, or the deadline passes.
func waitResize(t *testing.T, ctx context.Context, c *websocket.Conn, wantRows, wantCols uint16) bool {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		_, data, err := c.Read(rctx)
		if err != nil {
			return false
		}
		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type == protocol.FrameResize && f.Resize != nil &&
			f.Resize.Rows == wantRows && f.Resize.Cols == wantCols {
			return true
		}
	}
}
