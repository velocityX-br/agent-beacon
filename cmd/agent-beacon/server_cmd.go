package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/local/agent-beacon/internal/auth"
	"github.com/local/agent-beacon/internal/config"
	"github.com/local/agent-beacon/internal/server"
)

func serverCmd() *cobra.Command {
	var (
		address      string
		token        string
		authMode     string
		ttl          time.Duration
		sessionTTL   time.Duration
		startURL     string
		providerName string

		// password
		passwordHash string
		password     string

		// proxy-header
		proxyHeader    string
		proxyAllowlist []string

		// oidc
		oidcIssuer       string
		oidcClientID     string
		oidcClientSecret string
		oidcRedirectURL  string
		oidcScopes       []string

		// orchestration (UI-driven multi-agent loop)
		orchRoots       []string
		orchAPIKey      string
		orchPlanner     string
		orchWorker      string
		orchVerifier    string
		orchWorkerCmd   string
		orchTotalTimout time.Duration
		orchInputTimout time.Duration
		orchStateDir    string

		// built-in Cloudflare quick tunnel (optional public exposure)
		tunnel         bool
		tunnelCmd      string
		tunnelProtocol string
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the dashboard server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if token == "" {
				token = os.Getenv("AGENT_BEACON_AUTH_TOKEN")
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))

			cfg := server.Config{
				AgentToken:   token,
				AuthProvider: authMode,
				ProviderName: providerName,
				StartURL:     startURL,
				HeartbeatTTL: ttl,
				SessionTTL:   sessionTTL,
			}

			// Orchestration wiring (UI-driven multi-agent loop). Precedence for
			// each value: flag -> env -> config.toml -> built-in default. The
			// credentials live only server-side; they are never logged.
			//
			// Credentials mirror the CLI's orchestrate command: an x-api-key
			// (--orchestrate-api-key / $ANTHROPIC_API_KEY / config) takes
			// precedence, else the Claude Code proxy Bearer ($ANTHROPIC_AUTH_TOKEN).
			var fileCfg config.Config
			if dataDir, derr := config.DataDir(); derr == nil {
				fileCfg, _ = config.Load(dataDir)
			}
			cfg.AnthropicKey = firstNonEmpty(orchAPIKey, os.Getenv("ANTHROPIC_API_KEY"), fileCfg.AnthropicKey)
			cfg.AnthropicBearer = os.Getenv("ANTHROPIC_AUTH_TOKEN")
			cfg.AnthropicBaseURL = os.Getenv("ANTHROPIC_BASE_URL")
			cfg.PlannerModel = firstNonEmpty(orchPlanner, fileCfg.PlannerModel, defaultPlannerModel)
			cfg.WorkerModel = firstNonEmpty(orchWorker, fileCfg.WorkerModel, defaultWorkerModel)
			cfg.VerifierModel = firstNonEmpty(orchVerifier, fileCfg.VerifierModel, defaultVerifierModel)
			cfg.WorkerCmd = orchWorkerCmd
			cfg.OrchestrationTimeout = orchTotalTimout
			// Bounded wait for each user intervention (dangerous-op auth /
			// guidance). flag -> env -> server default (10m applied in New).
			cfg.InterventionTimeout = orchInputTimout
			if cfg.InterventionTimeout <= 0 {
				if d, derr := time.ParseDuration(os.Getenv("AGENT_BEACON_ORCH_INPUT_TIMEOUT")); derr == nil {
					cfg.InterventionTimeout = d
				}
			}
			// Allowed roots: explicit --orchestrate-root flags, else the
			// config's projects_dir list (the same roots the monitor spawns
			// under). A browser run must target a path under one of these.
			cfg.OrchestrationRoots = orchRoots
			if len(cfg.OrchestrationRoots) == 0 {
				if pd := firstNonEmpty(os.Getenv("AGENT_BEACON_PROJECTS_DIR"), fileCfg.ProjectsDir); pd != "" {
					cfg.OrchestrationRoots = config.ProjectRoots(pd)
				}
			}
			// Persist finished run reports so history survives a restart.
			// Location precedence: --orchestrate-state-dir flag ->
			// $AGENT_BEACON_ORCH_STATE_DIR -> <dataDir>/orchestrations.
			cfg.OrchestrationStateDir = firstNonEmpty(orchStateDir, os.Getenv("AGENT_BEACON_ORCH_STATE_DIR"))
			if cfg.OrchestrationStateDir == "" {
				if dataDir, derr := config.DataDir(); derr == nil {
					cfg.OrchestrationStateDir = filepath.Join(dataDir, "orchestrations")
				}
			}

			switch auth.Mode(authMode) {
			case auth.ModePassword:
				hash := firstNonEmpty(passwordHash, os.Getenv("AGENT_BEACON_PASSWORD_HASH"))
				if hash == "" {
					plain := firstNonEmpty(password, os.Getenv("AGENT_BEACON_PASSWORD"))
					if plain == "" {
						return fmt.Errorf("password auth requires --password, --password-hash, $AGENT_BEACON_PASSWORD, or $AGENT_BEACON_PASSWORD_HASH")
					}
					h, err := auth.HashPassword(plain)
					if err != nil {
						return err
					}
					hash = h
				}
				cfg.Password = auth.NewPasswordChecker(hash)
			case auth.ModeProxyHeader:
				cfg.ProxyHeader = auth.NewProxyHeaderAuth(proxyHeader, proxyAllowlist)
			case auth.ModeOIDC:
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				p, err := auth.NewOIDCProvider(ctx, auth.OIDCConfig{
					Issuer:       firstNonEmpty(oidcIssuer, os.Getenv("AGENT_BEACON_OIDC_ISSUER")),
					ClientID:     firstNonEmpty(oidcClientID, os.Getenv("AGENT_BEACON_OIDC_CLIENT_ID")),
					ClientSecret: firstNonEmpty(oidcClientSecret, os.Getenv("AGENT_BEACON_OIDC_CLIENT_SECRET")),
					RedirectURL:  firstNonEmpty(oidcRedirectURL, os.Getenv("AGENT_BEACON_OIDC_REDIRECT_URL")),
					Scopes:       oidcScopes,
				})
				if err != nil {
					return err
				}
				cfg.OIDC = p
			case auth.ModeNone:
				// open access
			default:
				return fmt.Errorf("unknown --auth mode %q (want password|oidc|proxy-header|none)", authMode)
			}

			// Refuse to expose the server to the public Internet with no browser
			// auth: a Cloudflare quick tunnel gives anyone the URL full,
			// unauthenticated control of the agents. Require a real auth mode.
			if tunnel && auth.Mode(authMode) == auth.ModeNone {
				return fmt.Errorf("--tunnel refuses --auth=none: exposing the server publicly without auth would grant anyone control; use --auth=password/--password (or oidc)")
			}

			s := server.New(cfg, log)

			srv := &http.Server{
				Addr:              address,
				Handler:           s.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			errc := make(chan error, 1)
			go func() {
				log.Info("server listening", "address", address, "auth", authMode)
				errc <- srv.ListenAndServe()
			}()

			// Optional built-in Cloudflare quick tunnel. It runs cloudflared as a
			// child bound to the same shutdown ctx (auto-killed on Ctrl-C), scans
			// its output for the *.trycloudflare.com URL, and prints a QR code.
			var tunnelProc *exec.Cmd
			if tunnel {
				log.Warn("public tunnel active: the server is reachable from the Internet — auth is enforced but treat the URL as sensitive")
				port := address
				if _, p, perr := net.SplitHostPort(address); perr == nil && p != "" {
					port = p
				}
				tc, terr := startTunnel(ctx, tunnelCmd, port, tunnelProtocol, log)
				if terr != nil {
					return terr
				}
				tunnelProc = tc
				// Reap the child so an early cloudflared exit is logged, not left
				// as a zombie or treated as fatal to the server.
				go func() {
					if werr := tunnelProc.Wait(); werr != nil && ctx.Err() == nil {
						log.Warn("cloudflared exited", "err", werr)
					}
				}()
			}

			select {
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if tunnelProc != nil && tunnelProc.Process != nil {
					_ = tunnelProc.Process.Kill()
				}
				return srv.Shutdown(shutdownCtx)
			case err := <-errc:
				if err == http.ErrServerClosed {
					return nil
				}
				return err
			}
		},
	}
	cmd.Flags().StringVar(&address, "address", ":8080", "HTTP listen address")
	cmd.Flags().StringVar(&token, "auth-token", "", "agent PSK bearer token (or $AGENT_BEACON_AUTH_TOKEN)")
	cmd.Flags().StringVar(&authMode, "auth", "password", "browser auth: password|oidc|proxy-header|none")
	cmd.Flags().DurationVar(&ttl, "heartbeat-ttl", 15*time.Second, "mark sessions stale after this idle time")
	cmd.Flags().DurationVar(&sessionTTL, "session-ttl", 12*time.Hour, "browser session cookie lifetime")
	cmd.Flags().StringVar(&startURL, "start-url", "", "login start URL surfaced via login-info")
	cmd.Flags().StringVar(&providerName, "provider-name", "", "human label for the auth provider (login page)")

	// built-in Cloudflare quick tunnel (optional public exposure)
	cmd.Flags().BoolVar(&tunnel, "tunnel", false, "expose the server over the Internet via a Cloudflare quick tunnel (requires cloudflared; prints a *.trycloudflare.com URL + QR; refused with --auth=none)")
	cmd.Flags().StringVar(&tunnelCmd, "tunnel-cmd", "cloudflared", "cloudflared binary path/name used for --tunnel")
	cmd.Flags().StringVar(&tunnelProtocol, "tunnel-protocol", "http2", "cloudflared edge transport for --tunnel: http2 (TCP/443, works behind firewalls that block QUIC) or auto (cloudflared's QUIC-first default)")

	// password
	cmd.Flags().StringVar(&password, "password", "", "browser password (plaintext; hashed at startup, or $AGENT_BEACON_PASSWORD)")
	cmd.Flags().StringVar(&passwordHash, "password-hash", "", "bcrypt hash for browser password (or $AGENT_BEACON_PASSWORD_HASH)")

	// proxy-header
	cmd.Flags().StringVar(&proxyHeader, "proxy-header", "X-Forwarded-User", "trusted identity header for proxy-header auth")
	cmd.Flags().StringSliceVar(&proxyAllowlist, "proxy-allow", nil, "allowlist of subjects for proxy-header auth (empty => any)")

	// oidc
	cmd.Flags().StringVar(&oidcIssuer, "oidc-issuer", "", "OIDC issuer URL")
	cmd.Flags().StringVar(&oidcClientID, "oidc-client-id", "", "OIDC client ID")
	cmd.Flags().StringVar(&oidcClientSecret, "oidc-client-secret", "", "OIDC client secret")
	cmd.Flags().StringVar(&oidcRedirectURL, "oidc-redirect-url", "", "OIDC redirect URL (this server's /api/v1/auth/callback)")
	cmd.Flags().StringSliceVar(&oidcScopes, "oidc-scopes", nil, "OIDC scopes (default openid,email,profile)")

	// orchestration (UI-driven multi-agent loop)
	cmd.Flags().StringSliceVar(&orchRoots, "orchestrate-root", nil, "allowed repository root for browser-launched orchestration (repeatable; defaults to config projects_dir)")
	cmd.Flags().StringVar(&orchAPIKey, "orchestrate-api-key", "", "Anthropic API key for browser-launched orchestration (or $ANTHROPIC_API_KEY / config; else $ANTHROPIC_AUTH_TOKEN)")
	cmd.Flags().StringVar(&orchPlanner, "orchestrate-planner-model", "", "planner model id (overrides config/default)")
	cmd.Flags().StringVar(&orchWorker, "orchestrate-worker-model", "", "coding-agent model id (overrides config/default)")
	cmd.Flags().StringVar(&orchVerifier, "orchestrate-verifier-model", "", "cross-model verifier model id (should differ from worker)")
	cmd.Flags().StringVar(&orchWorkerCmd, "orchestrate-worker-cmd", "", "coding-agent binary the workers run (default \"claude\")")
	cmd.Flags().DurationVar(&orchTotalTimout, "orchestrate-timeout", 0, "total wall-clock budget per browser-launched run (0 => no limit)")
	cmd.Flags().DurationVar(&orchInputTimout, "orchestrate-intervention-timeout", 0, "bounded wait per user intervention (dangerous-op auth / guidance); 0 => server default 10m (or $AGENT_BEACON_ORCH_INPUT_TIMEOUT)")
	cmd.Flags().StringVar(&orchStateDir, "orchestrate-state-dir", "", "directory to persist finished run reports (defaults to <data-dir>/orchestrations; empty disables persistence)")
	return cmd
}
