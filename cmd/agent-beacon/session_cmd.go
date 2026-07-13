package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/local/agent-beacon/internal/agent"
	"github.com/local/agent-beacon/internal/config"
	"github.com/local/agent-beacon/internal/monitor"
	"github.com/local/agent-beacon/pkg/protocol"
)

func sessionCmd() *cobra.Command {
	var (
		dashboardURL string
		token        string
		device       string
		sessionID    string
	)
	cmd := &cobra.Command{
		Use:   "session -- <command> [args...]",
		Short: "Wrap a coding-agent process and report to the dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("no command to wrap; usage: session -- <command> [args...]")
			}

			dataDir, err := config.DataDir()
			if err != nil {
				return err
			}
			cfg, err := config.Load(dataDir)
			if err != nil {
				return err
			}

			// URL precedence: --dashboard-url -> $AGENT_BEACON_URL -> config.toml.
			url := firstNonEmpty(dashboardURL, os.Getenv("AGENT_BEACON_URL"), cfg.URL)
			if url == "" {
				return errors.New("no dashboard URL (set --dashboard-url, $AGENT_BEACON_URL, or run install)")
			}

			// Token precedence: --token -> $AGENT_BEACON_AUTH_TOKEN -> token file.
			tok := firstNonEmpty(token, os.Getenv("AGENT_BEACON_AUTH_TOKEN"))
			if tok == "" && cfg.TokenPath != "" {
				tok, _ = config.ReadToken(cfg.TokenPath)
			}
			if tok == "" {
				return errors.New("no auth token (set --token, $AGENT_BEACON_AUTH_TOKEN, or run install)")
			}

			name, pinned := config.ResolveDeviceName(device, cfg)

			if sessionID == "" {
				sessionID = fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
			}

			var projects []protocol.Project
			var projectRoots []string
			if pd := firstNonEmpty(os.Getenv("AGENT_BEACON_PROJECTS_DIR"), cfg.ProjectsDir); pd != "" {
				projectRoots = config.ProjectRoots(pd)
				projects = agent.DiscoverProjects(projectRoots)
			}

			// Do NOT trap os.Interrupt: with the wrapper's stdin in raw mode,
			// Ctrl+C must flow through as a raw 0x03 byte to Claude's PTY rather
			// than being turned into a SIGINT that tears down the wrapper. Only
			// SIGTERM triggers a clean shutdown (the wrapper signals the child).
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
			defer stop()

			code, err := agent.Run(ctx, agent.Options{
				ServerURL:    url,
				Token:        tok,
				SessionID:    sessionID,
				Device:       name,
				Pinned:       pinned,
				Command:      args,
				Projects:     projects,
				ProjectRoots: projectRoots,
				// Poll the monitor's loopback report listener so a managed session
				// picks up the Claude Notification hook's "waiting" signal (the
				// same source observed sessions use) and can alert the operator.
				ReportAddr: monitor.DefaultReportAddr,
			})
			if err != nil {
				return err
			}
			os.Exit(code)
			return nil
		},
	}
	cmd.Flags().StringVar(&dashboardURL, "dashboard-url", "", "dashboard base URL (overrides env/config)")
	cmd.Flags().StringVar(&token, "token", "", "agent PSK (overrides env/config)")
	cmd.Flags().StringVar(&device, "device", "", "device name (pins the name, immune to remote rename)")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "explicit session id (default: generated)")
	return cmd
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
