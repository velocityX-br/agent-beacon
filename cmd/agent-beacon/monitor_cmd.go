package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/local/agent-beacon/internal/agent"
	"github.com/local/agent-beacon/internal/config"
	"github.com/local/agent-beacon/internal/monitor"
	"github.com/local/agent-beacon/pkg/protocol"
)

// monitorCmd runs the persistent monitor daemon: it scans the local process
// table for running Claude agents, ingests optional hook reports on a loopback
// listener, reports them to the server as read-only observed sessions, and stays
// connected to receive spawn requests. Unlike `session`, it wraps no command.
func monitorCmd() *cobra.Command {
	var (
		dashboardURL string
		token        string
		device       string
		reportAddr   string
	)
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Discover and report locally-running Claude agents to the dashboard",
		RunE: func(cmd *cobra.Command, _ []string) error {
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

			var projects []protocol.Project
			var projectRoots []string
			if pd := firstNonEmpty(os.Getenv("AGENT_BEACON_PROJECTS_DIR"), cfg.ProjectsDir); pd != "" {
				projectRoots = config.ProjectRoots(pd)
				projects = agent.DiscoverProjects(projectRoots)
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			return monitor.Run(ctx, monitor.Options{
				ServerURL:    url,
				Token:        tok,
				Device:       name,
				Pinned:       pinned,
				ProjectRoots: projectRoots,
				Projects:     projects,
				ReportAddr:   reportAddr,
			})
		},
	}
	cmd.Flags().StringVar(&dashboardURL, "dashboard-url", "", "dashboard base URL (overrides env/config)")
	cmd.Flags().StringVar(&token, "token", "", "agent PSK (overrides env/config)")
	cmd.Flags().StringVar(&device, "device", "", "device name (pins the name, immune to remote rename)")
	cmd.Flags().StringVar(&reportAddr, "report-addr", monitor.DefaultReportAddr, "loopback address for the Claude hook report listener")
	return cmd
}
