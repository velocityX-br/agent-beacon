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
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/pkg/protocol"
)

// Options configures a session run.
type Options struct {
	ServerURL      string   // e.g. http://localhost:8080
	Token          string   // PSK bearer
	SessionID      string   // unique per session
	Device         string   // resolved device name
	Pinned         bool     // device name is flag/env-sourced
	Command        []string // wrapped command + args, e.g. ["claude"]
	Projects       []protocol.Project
	ProjectRoots   []string // allowed roots for dashboard-originated spawns
	HeartbeatEvery time.Duration
	// ReportAddr is the monitor's loopback report listener (host:port). When set,
	// the heartbeat loop polls GET /state there to learn the Claude Notification
	// hook's "waiting" signal for this session's cwd, so a managed (PTY-wrapped)
	// session surfaces the same intervention alert as an observed one. Empty or
	// unreachable simply leaves the state at running (best-effort, non-fatal).
	ReportAddr string
}

// errNotConnected is returned by runner.send when there is no live WebSocket.
// The child/PTY outlive any single connection, so a dropped frame here is
// expected while (re)connecting and must never tear down the session.
var errNotConnected = errors.New("agent: not connected")

// conn is a single live WebSocket to the server, with its own write mutex.
type conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

// runner owns the long-lived child process and PTY and treats the WebSocket as
// a replaceable, subordinate resource. The child+PTY lifecycle is decoupled
// from the connection lifecycle: a WS drop reconnects with backoff while Claude
// keeps running locally; only the child exiting (or SIGTERM) ends the session.
type runner struct {
	opts                     Options
	ptmx                     *os.File
	cmd                      *exec.Cmd
	mu                       sync.Mutex    // guards cur
	cur                      *conn         // live connection, nil while (re)connecting
	backoffStart, backoffMax time.Duration // injectable for tests (default 1s/30s)
	log                      *slog.Logger
}

// send marshals and writes a frame over the current connection. It drops the
// frame (returning errNotConnected) when disconnected, so callers on hot paths
// (output tee, heartbeat) degrade to no-ops instead of failing.
func (rn *runner) send(f protocol.Frame) error {
	rn.mu.Lock()
	c := rn.cur
	rn.mu.Unlock()
	if c == nil {
		return errNotConnected
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(wctx, websocket.MessageText, data)
}

// reportStateResponse mirrors monitor.stateResponse: the hook-driven signal the
// managed session polls so it can raise the same waiting/idle alerts an observed
// session gets. Duplicated (small, stable) rather than shared to avoid importing
// the monitor package (which already imports agent — an import cycle).
type reportStateResponse struct {
	State    string `json:"state,omitempty"`
	Waiting  bool   `json:"waiting"`
	Stopping bool   `json:"stopping"`
}

// pollState queries the monitor's loopback /state endpoint for this session's
// hook-driven state (keyed by the Claude child pid and cwd) and maps it to a
// SessionState. It is best-effort: a disabled poll (no ReportAddr), an absent
// monitor, or any error returns ok=false so the caller keeps the running state.
// A short timeout guarantees the heartbeat loop never blocks on it.
func (rn *runner) pollState(pid int, cwd string) (protocol.SessionState, bool) {
	if rn.opts.ReportAddr == "" {
		return "", false
	}
	q := url.Values{}
	if pid > 0 {
		q.Set("pid", strconv.Itoa(pid))
	}
	if cwd != "" {
		q.Set("cwd", cwd)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+rn.opts.ReportAddr+"/state?"+q.Encode(), nil)
	if err != nil {
		return "", false
	}
	if rn.opts.Token != "" {
		req.Header.Set("X-Agent-Beacon-Token", rn.opts.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var sr reportStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", false
	}
	// Precedence mirrors the daemon's observed-session merge: Notification
	// (waiting) outranks Stop (idle) outranks an explicit hook state.
	switch {
	case sr.Waiting:
		return protocol.StateWaiting, true
	case sr.Stopping:
		return protocol.StateIdle, true
	case sr.State != "":
		return protocol.SessionState(sr.State), true
	}
	return "", false
}

// Run wraps the command in a PTY, tees its output to the local terminal and the
// server, applies control frames, emits heartbeats, and reconnects on WS drops.
// It blocks until the wrapped process exits (the only normal termination) or
// ctx is cancelled (SIGTERM -> clean child shutdown), returning the exit code.
func Run(ctx context.Context, opts Options) (int, error) {
	if len(opts.Command) == 0 {
		return 1, errors.New("no command to wrap")
	}
	if opts.HeartbeatEvery <= 0 {
		opts.HeartbeatEvery = 3 * time.Second
	}

	// Start the wrapped process in a PTY. Use exec.Command (NOT CommandContext)
	// so that cancelling ctx does not SIGKILL Claude out from under us; we
	// manage the child's shutdown explicitly via SIGTERM below.
	cmd := exec.Command(opts.Command[0], opts.Command[1:]...)
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

	// Put the local stdin in raw mode so control chars (^C/^Z/^\) pass through
	// as raw bytes to Claude's PTY instead of being interpreted by the local
	// terminal's line discipline. Guarded so a non-tty (CI, spawned child with
	// nil stdin) degrades gracefully. Terminal state is restored on exit.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			defer func() { _ = term.Restore(int(os.Stdin.Fd()), old) }()
		}
	}

	rn := &runner{
		opts:         opts,
		ptmx:         ptmx,
		cmd:          cmd,
		backoffStart: time.Second,
		backoffMax:   30 * time.Second,
		log:          slog.Default(),
	}

	// childCtx bounds every goroutine to the child's lifetime. It is cancelled
	// only when the child exits or the output PTY read fails (end of life),
	// NEVER on a WS teardown.
	childCtx, childCancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	// SIGTERM watcher: a cancelled ctx (from session_cmd's SIGTERM trap) means
	// "shut down cleanly" -> forward SIGTERM to the child; cmd.Wait then returns.
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		case <-childCtx.Done():
		}
	}()

	// (a) Output tee: PTY -> local stdout ALWAYS, and -> server when connected.
	// Live output is intentionally not buffered across reconnects; the server
	// replays its own scrollback to re-attaching browsers. A PTY read error is
	// end-of-life -> childCancel(), not a WS teardown.
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
				_ = rn.send(protocol.Frame{Type: protocol.FrameOutput, SessionID: opts.SessionID, Output: chunk})
			}
			if err != nil {
				childCancel()
				return
			}
		}
	}()

	// (b) Local stdin -> PTY. With raw mode, ^C/^Z/^\ arrive as raw bytes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ptmx, readerCtx{ctx: childCtx, r: os.Stdin})
	}()

	// (c) Connect loop with capped backoff: keep a live WS while the child runs,
	// reconnecting on drops. Never kills the child over connectivity.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rn.connectLoop(childCtx)
	}()

	// (d) Heartbeat loop: the tick after a reconnect re-registers the session
	// (the server Removes on WS close). This is the core Issue-1 fix.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(opts.HeartbeatEvery)
		defer t.Stop()
		cwd, _ := os.Getwd()
		cmdline := strings.Join(opts.Command, " ")
		// The wrapped Claude child's pid; the Notification hook reports this same
		// pid (its os.Getppid()) so the /state poll can match on it or on cwd.
		childPid := 0
		if cmd.Process != nil {
			childPid = cmd.Process.Pid
		}
		for {
			select {
			case <-childCtx.Done():
				return
			case <-t.C:
				// Re-read the branch on each tick so a `git checkout` inside the
				// session is reflected on the dashboard without a restart. It's a
				// cheap .git/HEAD read (no git exec) and best-effort (empty on a
				// detached HEAD or non-repo).
				//
				// State: default to running, but consult the monitor's hook-driven
				// signal so a managed session shows the same "waiting" (Claude
				// blocked on the user) / "idle" (Stop) alert as an observed one.
				state := protocol.StateRunning
				if st, ok := rn.pollState(childPid, cwd); ok {
					state = st
				}
				_ = rn.send(protocol.Frame{
					Type:      protocol.FrameHeartbeat,
					SessionID: opts.SessionID,
					Heartbeat: &protocol.Heartbeat{
						Device:       opts.Device,
						DevicePinned: opts.Pinned,
						Command:      cmdline,
						CWD:          cwd,
						Branch:       branchOf(cwd),
						State:        state,
						SentAt:       time.Now(),
					},
				})
			}
		}
	}()

	// (e) SIGWINCH watcher: report the raw local size initially and on each
	// resize so it participates in server-side min negotiation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGWINCH)
		defer signal.Stop(ch)
		rn.reportLocalSize()
		for {
			select {
			case <-childCtx.Done():
				return
			case <-ch:
				rn.reportLocalSize()
			}
		}
	}()

	// The wrapped process exiting is the ONLY normal termination.
	waitErr := cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		code = ee.ExitCode()
	}
	_ = rn.send(protocol.Frame{
		Type:      protocol.FrameExit,
		SessionID: opts.SessionID,
		Exit:      &protocol.ExitMsg{Code: code},
	})
	childCancel()
	_ = ptmx.Close()
	wg.Wait()
	return code, nil
}

// connectLoop repeatedly dials the server and services one connection at a time,
// backing off (capped) between attempts. It returns only when ctx is cancelled
// (child exiting). A dropped connection logs a warning and reconnects; Claude
// keeps running throughout.
func (rn *runner) connectLoop(ctx context.Context) {
	backoff := rn.backoffStart
	if backoff <= 0 {
		backoff = time.Second
	}
	max := rn.backoffMax
	if max <= 0 {
		max = 30 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		err := rn.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		rn.log.Warn("connection down; claude keeps running; retrying",
			"session", rn.opts.SessionID, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < max {
			backoff *= 2
			if backoff > max {
				backoff = max
			}
		}
	}
}

// connectOnce dials the server, installs the live connection, re-announces
// projects, reports the local size, then reads control frames until the socket
// drops or ctx is cancelled. It always clears rn.cur before returning so the
// connect loop reconnects.
func (rn *runner) connectOnce(ctx context.Context) error {
	wsURL := toWS(rn.opts.ServerURL) + "/api/v1/agent/connect?session_id=" + rn.opts.SessionID
	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()
	ws, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + rn.opts.Token}},
	})
	if err != nil {
		return err
	}
	ws.SetReadLimit(1 << 20)

	rn.mu.Lock()
	rn.cur = &conn{ws: ws}
	rn.mu.Unlock()
	defer func() {
		rn.mu.Lock()
		rn.cur = nil
		rn.mu.Unlock()
		ws.Close(websocket.StatusNormalClosure, "")
	}()

	// Re-announce spawnable projects and report the local size on every
	// (re)connection so a restarted server relearns them.
	if len(rn.opts.Projects) > 0 {
		_ = rn.send(protocol.Frame{Type: protocol.FrameProjects, SessionID: rn.opts.SessionID, Projects: rn.opts.Projects})
	}
	rn.reportLocalSize()

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Type {
		case protocol.FrameInput:
			_, _ = rn.ptmx.Write(f.Input)
		case protocol.FrameResize:
			if f.Resize != nil {
				_ = pty.Setsize(rn.ptmx, &pty.Winsize{Rows: f.Resize.Rows, Cols: f.Resize.Cols})
			}
		case protocol.FrameSpawn:
			sc := spawnConfig{
				serverURL: rn.opts.ServerURL,
				token:     rn.opts.Token,
				device:    rn.opts.Device,
				pinned:    rn.opts.Pinned,
				roots:     rn.opts.ProjectRoots,
			}
			if err := handleSpawn(ctx, sc, f.Spawn); err != nil {
				// Report the failure back as PTY-visible text so the dashboard
				// operator sees why a spawn was refused.
				_ = rn.send(protocol.Frame{
					Type:      protocol.FrameOutput,
					SessionID: rn.opts.SessionID,
					Output:    []byte("\r\n[agent-beacon] spawn failed: " + err.Error() + "\r\n"),
				})
			}
		}
	}
}

// reportLocalSize reads the RAW local terminal size, applies it to the local
// PTY, and sends a FrameLocalSize (raw size only — never the negotiated min, so
// the server->agent FrameResize can never oscillate). No-op when stdin is not a
// terminal or while disconnected (send drops the frame).
func (rn *runner) reportLocalSize() {
	ws, err := pty.GetsizeFull(os.Stdin)
	if err != nil || ws == nil || ws.Rows == 0 || ws.Cols == 0 {
		return
	}
	_ = pty.Setsize(rn.ptmx, ws)
	_ = rn.send(protocol.Frame{
		Type:      protocol.FrameLocalSize,
		SessionID: rn.opts.SessionID,
		Resize:    &protocol.ResizeMsg{Rows: ws.Rows, Cols: ws.Cols},
	})
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

// branchOf derives the current git branch from a working directory by reading
// .git/HEAD (no git exec). It handles the worktree case where .git is a file
// containing "gitdir: <path>". Returns "" when it cannot determine a branch.
// The logic mirrors monitor.branchOf; it is duplicated here (rather than shared)
// to avoid an import cycle, since the monitor package already imports agent.
func branchOf(cwd string) string {
	dir := cwd
	for i := 0; i < 40 && dir != "" && dir != "/"; i++ {
		gitPath := filepath.Join(dir, ".git")
		info, err := os.Stat(gitPath)
		if err == nil {
			if info.IsDir() {
				return headBranch(filepath.Join(gitPath, "HEAD"))
			}
			// .git is a file (worktree/submodule): "gitdir: <path>".
			if gd := readGitdir(gitPath); gd != "" {
				return headBranch(filepath.Join(gd, "HEAD"))
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// headBranch reads a HEAD file and returns the short branch name for a symbolic
// ref ("ref: refs/heads/<name>"). Detached HEADs return "".
func headBranch(headPath string) string {
	data, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const p = "ref: refs/heads/"
	if strings.HasPrefix(line, p) {
		return strings.TrimPrefix(line, p)
	}
	return ""
}

// readGitdir resolves the target of a ".git" file ("gitdir: <path>"). Relative
// targets are resolved against the file's directory.
func readGitdir(gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const p = "gitdir: "
	if !strings.HasPrefix(line, p) {
		return ""
	}
	target := strings.TrimPrefix(line, p)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitFile), target)
	}
	return target
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
