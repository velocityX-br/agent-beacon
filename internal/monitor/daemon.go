package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"log/slog"

	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/internal/agent"
	"github.com/local/agent-beacon/pkg/protocol"
)

// Options configures the monitor daemon.
type Options struct {
	ServerURL      string // e.g. http://localhost:8080
	Token          string // PSK bearer (also required on the local report listener)
	Device         string // resolved device name
	Pinned         bool   // device name is flag/env-sourced
	ProjectRoots   []string
	Projects       []protocol.Project
	ScanEvery      time.Duration // process-scan cadence (default 3s)
	HeartbeatEvery time.Duration // reserved; the scan tick doubles as the heartbeat
	ReportAddr     string        // local report listener (default 127.0.0.1:47615)
	Log            *slog.Logger
}

// Run starts the monitor daemon: it launches the local report listener, then
// connects to the server over a role=monitor WebSocket and, on each scan tick,
// pushes a snapshot of observed claude processes. It reconnects with backoff and
// blocks until ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	if opts.ScanEvery <= 0 {
		opts.ScanEvery = 3 * time.Second
	}
	if opts.ReportAddr == "" {
		opts.ReportAddr = DefaultReportAddr
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.ServerURL == "" {
		return errors.New("monitor: no server URL")
	}
	if opts.Token == "" {
		return errors.New("monitor: no auth token")
	}

	store := newEnrichStore(30 * time.Second)

	// Local report listener (loopback only) for Claude hook enrichment.
	reportSrv := &http.Server{
		Addr:              opts.ReportAddr,
		Handler:           newReportServer(store, opts.Token, opts.Log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", opts.ReportAddr)
	if err != nil {
		opts.Log.Warn("report listener disabled", "addr", opts.ReportAddr, "err", err)
	} else {
		go func() { _ = reportSrv.Serve(ln) }()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = reportSrv.Shutdown(sctx)
		}()
		opts.Log.Info("report listener up", "addr", opts.ReportAddr)
	}

	// Connect loop with capped backoff. Unlike the wrap agent (one-shot), the
	// monitor is long-lived and must survive server restarts.
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		err := runOnce(ctx, opts, store)
		if err != nil && ctx.Err() == nil {
			opts.Log.Warn("monitor connection ended; retrying", "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		backoff = time.Second
	}
}

// runOnce holds one WebSocket connection: dial, announce projects, then loop
// scanning + reading control frames until the socket drops or ctx is cancelled.
func runOnce(ctx context.Context, opts Options, store *enrichStore) error {
	wsURL := toWS(opts.ServerURL) + "/api/v1/agent/connect?role=monitor&device=" + urlQueryEscape(opts.Device)
	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()
	c, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + opts.Token}},
	})
	if err != nil {
		return err
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	c.SetReadLimit(1 << 20)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeMu sync.Mutex
	send := func(f protocol.Frame) error {
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		wctx, wcancel := context.WithTimeout(connCtx, 10*time.Second)
		defer wcancel()
		writeMu.Lock()
		defer writeMu.Unlock()
		return c.Write(wctx, websocket.MessageText, data)
	}

	// Announce spawnable projects once per connection.
	if len(opts.Projects) > 0 {
		_ = send(protocol.Frame{Type: protocol.FrameProjects, Projects: opts.Projects})
	}

	// Reader: handle spawn control frames from the server. Input/resize are
	// meaningless for observed sessions and are ignored.
	go func() {
		for {
			_, data, err := c.Read(connCtx)
			if err != nil {
				cancel()
				return
			}
			var f protocol.Frame
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			if f.Type == protocol.FrameSpawn {
				if err := agent.Spawn(connCtx, agent.SpawnConfig{
					ServerURL: opts.ServerURL,
					Token:     opts.Token,
					Device:    opts.Device,
					Pinned:    opts.Pinned,
					Roots:     opts.ProjectRoots,
				}, f.Spawn); err != nil {
					opts.Log.Warn("spawn failed", "err", err)
				}
			}
		}
	}()

	// Scan loop: push one FrameObserved snapshot per tick.
	t := time.NewTicker(opts.ScanEvery)
	defer t.Stop()
	// Send an immediate first snapshot so a card appears without waiting a tick.
	if err := sendSnapshot(send, opts, store); err != nil {
		return err
	}
	for {
		select {
		case <-connCtx.Done():
			return connCtx.Err()
		case <-t.C:
			if err := sendSnapshot(send, opts, store); err != nil {
				return err
			}
		}
	}
}

// sendSnapshot scans, merges hook enrichment, and emits one FrameObserved.
func sendSnapshot(send func(protocol.Frame) error, opts Options, store *enrichStore) error {
	procs, err := Scan()
	if err != nil {
		// A scan failure (e.g. unsupported platform) is non-fatal: report an
		// empty set so the server clears any stale observed cards.
		opts.Log.Debug("scan error", "err", err)
	}
	now := time.Now()
	observed := make([]protocol.Heartbeat, 0, len(procs))
	for _, p := range procs {
		hb := protocol.Heartbeat{
			Device:        opts.Device,
			DevicePinned:  opts.Pinned,
			Command:       p.Command,
			CWD:           p.CWD,
			Branch:        p.Branch,
			Kind:          protocol.KindObserved,
			Pid:           p.Pid,
			State:         protocol.StateRunning,
			SessionSource: protocol.SourceScan,
			SentAt:        now,
		}
		if e, ok := store.lookup(p.Pid, p.CWD); ok {
			hb.SessionSource = protocol.SourceHook
			if e.Model != "" {
				hb.Model = e.Model
			}
			if e.ContextPct != 0 {
				hb.ContextPct = e.ContextPct
			}
			if e.Task != "" {
				hb.Task = e.Task
			}
			// State precedence: a Stop event (idle) is terminal for the tick; a
			// Notification event (Claude blocked on the user) raises the
			// intervention "waiting" state; otherwise honour any explicit state.
			switch {
			case e.Stopping:
				hb.State = protocol.StateIdle
			case e.Waiting:
				hb.State = protocol.StateWaiting
			case e.State != "":
				hb.State = protocol.SessionState(e.State)
			}
		}
		observed = append(observed, hb)
	}
	return send(protocol.Frame{Type: protocol.FrameObserved, Observed: observed})
}

// toWS converts an http(s) base URL to a ws(s) URL.
func toWS(httpURL string) string {
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	}
	if strings.HasPrefix(httpURL, "http://") {
		return "ws://" + strings.TrimPrefix(httpURL, "http://")
	}
	return httpURL
}

// urlQueryEscape escapes a query value without pulling in net/url for one call.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '~' {
			b.WriteRune(r)
			continue
		}
		for _, by := range []byte(string(r)) {
			b.WriteByte('%')
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[by>>4])
			b.WriteByte(hex[by&0x0f])
		}
	}
	return b.String()
}
