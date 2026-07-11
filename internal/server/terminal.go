package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"nhooyr.io/websocket"
)

// termClientMsg is the browser->server message on the terminal WebSocket.
// The browser sends keystrokes as input and window changes as resize.
type termClientMsg struct {
	Type string `json:"type"` // "input" | "resize"
	Data string `json:"data,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// handleTerminalWS attaches a browser terminal to a live session. It subscribes
// to the session's PTY output fan-out (server->browser) and forwards browser
// keystrokes/resizes to the agent via the session's control sender.
func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	if !s.browserAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	sess, ok := s.reg.Get(sessionID)
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("terminal ws accept failed", "err", err)
		return
	}
	c.SetReadLimit(1 << 20)
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx := r.Context()
	subID, backlog, out, unsubscribe := sess.Subscribe()
	defer unsubscribe()

	// Replay recent PTY output so a TUI (e.g. Claude) repaints correctly after a
	// detach/re-attach. Sent as one initial binary frame before the pump starts.
	if len(backlog) > 0 {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.Write(wctx, websocket.MessageBinary, backlog)
		cancel()
		if err != nil {
			return
		}
	}

	// Pump PTY output to the browser as binary frames.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case b, ok := <-out:
				if !ok {
					return
				}
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.Write(wctx, websocket.MessageBinary, b)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()

	// Read browser input/resize and forward to the agent.
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) && !errors.Is(err, context.Canceled) {
				s.log.Debug("terminal read ended", "session", sessionID, "err", err)
			}
			return
		}
		if typ == websocket.MessageBinary {
			// Raw binary is treated as keystrokes.
			_ = sess.SendInput(data)
			continue
		}
		var m termClientMsg
		if err := json.Unmarshal(data, &m); err != nil {
			// Fall back to treating the text as raw keystrokes.
			_ = sess.SendInput(data)
			continue
		}
		switch m.Type {
		case "input":
			_ = sess.SendInput([]byte(m.Data))
		case "resize":
			if m.Rows > 0 && m.Cols > 0 {
				// Negotiate the PTY size as the minimum across all attached
				// browsers so a second tab can only shrink, never yank, the PTY.
				if rows, cols, changed := sess.SetSubscriberSize(subID, m.Rows, m.Cols); changed {
					_ = sess.SendResize(rows, cols)
				}
			}
		}
	}
}
