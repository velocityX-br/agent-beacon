package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/creack/pty"
	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/pkg/protocol"
)

// newTestRunner builds a runner backed by a real (headless) PTY pair so
// connectOnce exercises the true FrameInput/FrameResize write paths without
// needing a controlling terminal. Backoff is tiny so the connect loop retries
// quickly.
func newTestRunner(t *testing.T, serverURL string) (*runner, func()) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80})
	rn := &runner{
		opts: Options{
			ServerURL: serverURL,
			Token:     "test-psk",
			SessionID: "sess-reconnect",
			Projects:  []protocol.Project{{Name: "demo", Path: "/tmp/demo", Root: "/tmp"}},
		},
		ptmx:         ptmx,
		backoffStart: 5 * time.Millisecond,
		backoffMax:   20 * time.Millisecond,
		log:          slog.Default(),
	}
	cleanup := func() {
		_ = tty.Close()
		_ = ptmx.Close()
	}
	return rn, cleanup
}

// TestConnectLoopRetriesAndReRegisters verifies the connect loop redials after
// a dropped connection and re-announces projects each time it connects. It uses
// an httptest WS server that closes the first connection immediately, then
// serves the second connection long enough to capture the re-announced frames.
// (reportLocalSize reads os.Stdin, which is not a tty under `go test`, so no
// FrameLocalSize is emitted here; the FrameLocalSize path is covered by the
// server-side local_size_test.go.)
func TestConnectLoopRetriesAndReRegisters(t *testing.T) {
	var mu sync.Mutex
	connCount := 0
	// framesOnSecond collects frames the agent sends on the SECOND connection.
	var framesOnSecond []protocol.Frame
	secondDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-psk" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		mu.Lock()
		connCount++
		n := connCount
		mu.Unlock()

		if n == 1 {
			// Drop the first connection immediately to force a reconnect.
			c.Close(websocket.StatusNormalClosure, "bye")
			return
		}
		// Second connection: read the re-announced frames the agent sends on
		// (re)connect (FrameProjects, FrameLocalSize), then close.
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		c.SetReadLimit(1 << 20)
		for i := 0; i < 2; i++ {
			_, data, err := c.Read(ctx)
			if err != nil {
				break
			}
			var f protocol.Frame
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			mu.Lock()
			framesOnSecond = append(framesOnSecond, f)
			mu.Unlock()
		}
		close(secondDone)
		c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	rn, cleanup := newTestRunner(t, srv.URL)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	go rn.connectLoop(ctx)

	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for second connection")
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if connCount < 2 {
		t.Fatalf("connCount = %d, want >= 2 (redial expected)", connCount)
	}
	var sawProjects bool
	for _, f := range framesOnSecond {
		if f.Type == protocol.FrameProjects {
			sawProjects = true
		}
	}
	if !sawProjects {
		t.Fatalf("second connection did not re-announce projects; frames=%v", framesOnSecond)
	}
}

// TestSendDroppedWhenDisconnected verifies send returns errNotConnected (and
// does not panic) when there is no live connection.
func TestSendDroppedWhenDisconnected(t *testing.T) {
	rn := &runner{opts: Options{SessionID: "x"}, log: slog.Default()}
	if err := rn.send(protocol.Frame{Type: protocol.FrameHeartbeat}); err != errNotConnected {
		t.Fatalf("send while disconnected = %v, want errNotConnected", err)
	}
}
