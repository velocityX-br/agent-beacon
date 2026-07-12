package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/local/agent-beacon/internal/config"
	"github.com/local/agent-beacon/internal/monitor"
)

// reportCmd is a thin client the Claude hook invokes to enrich the observed
// session for the current process. It POSTs metadata (model, context %, task,
// state, event) to the monitor daemon's loopback report listener, which merges
// it onto the matching scanned process by pid/cwd. Failures are non-fatal so a
// missing or down monitor never blocks a Claude session.
func reportCmd() *cobra.Command {
	var (
		addr       string
		token      string
		pid        int
		cwd        string
		sessionID  string
		model      string
		contextPct float64
		task       string
		state      string
		event      string
	)
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Send Claude session metadata to the local monitor (used by hooks)",
		Long: "Posts optional session metadata to the monitor's loopback report " +
			"listener so the dashboard can enrich the observed process card. " +
			"Intended to be called from Claude Code hooks. Non-fatal on error.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Default correlation keys to this process's context so hooks can be
			// wired with no arguments and still attach to the right card.
			if pid == 0 {
				pid = os.Getppid()
			}
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			// Token precedence: --token -> env -> token file.
			tok := firstNonEmpty(token, os.Getenv("AGENT_BEACON_AUTH_TOKEN"))
			if tok == "" {
				if dataDir, err := config.DataDir(); err == nil {
					if cfg, err := config.Load(dataDir); err == nil && cfg.TokenPath != "" {
						tok, _ = config.ReadToken(cfg.TokenPath)
					}
				}
			}

			body := map[string]any{}
			if pid > 0 {
				body["pid"] = pid
			}
			if cwd != "" {
				body["cwd"] = cwd
			}
			if sessionID != "" {
				body["session_id"] = sessionID
			}
			if model != "" {
				body["model"] = model
			}
			if contextPct != 0 {
				body["context_pct"] = contextPct
			}
			if task != "" {
				body["task"] = task
			}
			if state != "" {
				body["state"] = state
			}
			if event != "" {
				body["event"] = event
			}

			data, err := json.Marshal(body)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/report", bytes.NewReader(data))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			if tok != "" {
				req.Header.Set("X-Agent-Beacon-Token", tok)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				// The monitor may not be running; that's fine for a hook.
				fmt.Fprintln(os.Stderr, "report: monitor unreachable:", err)
				return nil
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				fmt.Fprintln(os.Stderr, "report: monitor rejected report:", resp.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", monitor.DefaultReportAddr, "monitor report listener address")
	cmd.Flags().StringVar(&token, "token", "", "report token (or $AGENT_BEACON_AUTH_TOKEN)")
	cmd.Flags().IntVar(&pid, "pid", 0, "process id to enrich (default: parent pid)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory to enrich (default: current)")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Claude session id")
	cmd.Flags().StringVar(&model, "model", "", "model name")
	cmd.Flags().Float64Var(&contextPct, "context-pct", 0, "context window used, percent")
	cmd.Flags().StringVar(&task, "task", "", "current task description")
	cmd.Flags().StringVar(&state, "state", "", "session state (running|idle|...)")
	cmd.Flags().StringVar(&event, "event", "", "hook event (SessionStart|Stop|heartbeat|Notification)")
	return cmd
}
