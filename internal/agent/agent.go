// Package agent implements the session wrapper: it runs a coding-agent process
// inside a PTY, connects outbound to the server over a WebSocket, streams PTY
// output up, applies input/resize/spawn control frames down, and emits periodic
// heartbeats.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/pkg/protocol"
)

// Options configures a session run.
type Options struct {
	ServerURL   string   // e.g. http://localhost:8080
	Token       string   // PSK bearer
	SessionID   string   // unique per session
	Device      string   // resolved device name
	Pinned      bool     // device name is flag/env-sourced
	Command     []string // wrapped command + args, e.g. ["claude"]
	Projects    []protocol.Project
	ProjectRoots []string // allowed roots for dashboard-originated spawns
	HeartbeatEvery time.Duration
}

// Run wraps the command, connects to the server, and blocks until the process
// exits or ctx is cancelled. It returns the process exit code.
func Run(ctx context.Context, opts Options) (int, error) {
	if len(opts.Command) == 0 {
		return 1, errors.New("no command to wrap")
	}
	if opts.HeartbeatEvery <= 0 {
		opts.HeartbeatEvery = 3 * time.Second
	}

	// Start the wrapped process in a PTY.
	cmd := exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	cmd.Env = os.Environ()
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return 1, err
	}
	defer func() { _ = ptmx.Close() }()

	// Mirror the current terminal size onto the PTY if we're attached to one.
	if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
		_ = pty.Setsize(ptmx, ws)
	}

	// Dial the server.
	wsURL := toWS(opts.ServerURL) + "/api/v1/agent/connect?session_id=" + opts.SessionID
	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()
	c, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + opts.Token}},
	})
	if err != nil {
		return 1, err
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	c.SetReadLimit(1 << 20)

	var writeMu sync.Mutex
	send := func(f protocol.Frame) error {
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		writeMu.Lock()
		defer writeMu.Unlock()
		return c.Write(wctx, websocket.MessageText, data)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Announce spawnable projects once at startup.
	if len(opts.Projects) > 0 {
		_ = send(protocol.Frame{Type: protocol.FrameProjects, SessionID: opts.SessionID, Projects: opts.Projects})
	}

	var wg sync.WaitGroup

	// PTY output -> local stdout AND -> server.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				_, _ = os.Stdout.Write(chunk)
				_ = send(protocol.Frame{Type: protocol.FrameOutput, SessionID: opts.SessionID, Output: chunk})
			}
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// Local stdin -> PTY (so the local terminal still works).
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ptmx, readerCtx{ctx: runCtx, r: os.Stdin})
	}()

	// Server control frames -> PTY / actions.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, data, err := c.Read(runCtx)
			if err != nil {
				cancel()
				return
			}
			var f protocol.Frame
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			switch f.Type {
			case protocol.FrameInput:
				_, _ = ptmx.Write(f.Input)
			case protocol.FrameResize:
				if f.Resize != nil {
					_ = pty.Setsize(ptmx, &pty.Winsize{Rows: f.Resize.Rows, Cols: f.Resize.Cols})
				}
			case protocol.FrameSpawn:
				sc := spawnConfig{
					serverURL: opts.ServerURL,
					token:     opts.Token,
					device:    opts.Device,
					pinned:    opts.Pinned,
					roots:     opts.ProjectRoots,
				}
				if err := handleSpawn(runCtx, sc, f.Spawn); err != nil {
					// Report the failure back to the server as PTY-visible text
					// so the dashboard operator sees why a spawn was refused.
					_ = send(protocol.Frame{
						Type:      protocol.FrameOutput,
						SessionID: opts.SessionID,
						Output:    []byte("\r\n[agent-beacon] spawn failed: " + err.Error() + "\r\n"),
					})
				}
			}
		}
	}()

	// Heartbeat loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(opts.HeartbeatEvery)
		defer t.Stop()
		cwd, _ := os.Getwd()
		cmdline := strings.Join(opts.Command, " ")
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				_ = send(protocol.Frame{
					Type:      protocol.FrameHeartbeat,
					SessionID: opts.SessionID,
					Heartbeat: &protocol.Heartbeat{
						Device:       opts.Device,
						DevicePinned: opts.Pinned,
						Command:      cmdline,
						CWD:          cwd,
						State:        protocol.StateRunning,
						SentAt:       time.Now(),
					},
				})
			}
		}
	}()

	// Wait for the process to exit.
	waitErr := cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		code = ee.ExitCode()
	}
	_ = send(protocol.Frame{
		Type:      protocol.FrameExit,
		SessionID: opts.SessionID,
		Exit:      &protocol.ExitMsg{Code: code},
	})
	cancel()
	_ = ptmx.Close()
	wg.Wait()
	return code, nil
}

// DiscoverProjects walks each root one level deep and returns directories that
// look like git repositories, resolving symlinks and rejecting escapes.
func DiscoverProjects(roots []string) []protocol.Project {
	var out []protocol.Project
	for _, root := range roots {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		entries, err := os.ReadDir(realRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(realRoot, e.Name())
			real, err := filepath.EvalSymlinks(p)
			if err != nil || !strings.HasPrefix(real, realRoot) {
				continue // symlink escaping the root
			}
			if _, err := os.Stat(filepath.Join(real, ".git")); err == nil {
				out = append(out, protocol.Project{Name: e.Name(), Path: real, Root: realRoot})
			}
		}
	}
	return out
}

func toWS(httpURL string) string {
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	}
	if strings.HasPrefix(httpURL, "http://") {
		return "ws://" + strings.TrimPrefix(httpURL, "http://")
	}
	return httpURL
}

// readerCtx makes an io.Reader stop when a context is cancelled by returning
// EOF on the next read attempt after cancellation.
type readerCtx struct {
	ctx context.Context
	r   io.Reader
}

func (rc readerCtx) Read(p []byte) (int, error) {
	if err := rc.ctx.Err(); err != nil {
		return 0, io.EOF
	}
	return rc.r.Read(p)
}
