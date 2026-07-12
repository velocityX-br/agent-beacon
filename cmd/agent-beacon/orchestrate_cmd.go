package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/local/agent-beacon/internal/anthropic"
	"github.com/local/agent-beacon/internal/config"
	"github.com/local/agent-beacon/internal/orchestrator"
)

// Default model roles. Worker/planner default to Sonnet (fast, cheap coding);
// the verifier defaults to a DIFFERENT model (Opus) so the cross-check is
// genuinely independent. All are overridable via flags/env/config.
const (
	defaultPlannerModel  = "claude-sonnet-4-6"
	defaultWorkerModel   = "claude-sonnet-4-6"
	defaultVerifierModel = "claude-opus-4-6"
)

func orchestrateCmd() *cobra.Command {
	var (
		task             string
		repo             string
		workers          int
		maxIters         int
		workerModel      string
		verifierModel    string
		plannerModel     string
		apiKey           string
		workerCmd        string
		buildCmd         string
		testCmd          string
		e2eCmd           string
		worktreeLocation string
		reportPath       string
		gateTimeout      time.Duration
		totalTimeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "orchestrate --task \"<goal>\" [--repo <path>]",
		Short: "Autonomously coordinate worker agents to complete a task with self-verification",
		Long: "Decomposes a development task into parallel subtasks, runs each in an " +
			"isolated git worktree with a self-repair loop (headless agent -> " +
			"deterministic build/test/e2e gates -> cross-model verifier), and loops " +
			"until every subtask passes or the iteration budget is exhausted.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(task) == "" {
				return errors.New("--task is required")
			}

			dataDir, err := config.DataDir()
			if err != nil {
				return err
			}
			cfg, err := config.Load(dataDir)
			if err != nil {
				return err
			}

			// Resolve the target repository. When --repo is omitted we do NOT
			// silently fall back to the cwd: multi-agent runs must have an
			// explicit, user-confirmed workspace so generated code never lands
			// somewhere unexpected. If stdin is an interactive TTY we prompt
			// (defaulting to cwd on empty input); otherwise we hard-fail so
			// non-interactive callers (CI, background tasks) declare --repo.
			repo, err = resolveRepoPath(cmd, repo)
			if err != nil {
				return err
			}
			repoAbs, err := os.Stat(repo)
			if err != nil || !repoAbs.IsDir() {
				return fmt.Errorf("--repo is not a directory: %s", repo)
			}

			// Credential precedence: --api-key -> $ANTHROPIC_API_KEY -> config
			// (direct-API, x-api-key style). If none is present, fall back to
			// $ANTHROPIC_AUTH_TOKEN (Claude Code proxy/gateway, Bearer style).
			key := firstNonEmpty(apiKey, os.Getenv("ANTHROPIC_API_KEY"), cfg.AnthropicKey)
			bearer := os.Getenv("ANTHROPIC_AUTH_TOKEN")
			if key == "" && bearer == "" {
				return errors.New("no Anthropic credentials (set --api-key, $ANTHROPIC_API_KEY, $ANTHROPIC_AUTH_TOKEN, or config)")
			}

			// Model precedence: flag -> config -> built-in default.
			planner := firstNonEmpty(plannerModel, cfg.PlannerModel, defaultPlannerModel)
			worker := firstNonEmpty(workerModel, cfg.WorkerModel, defaultWorkerModel)
			verifier := firstNonEmpty(verifierModel, cfg.VerifierModel, defaultVerifierModel)
			if worker == verifier {
				fmt.Fprintf(os.Stderr,
					"warning: worker and verifier use the same model (%s); the cross-model check is weakened\n", worker)
			}

			oc := orchestrator.Config{
				RepoDir:          repo,
				Task:             task,
				Workers:          workers,
				MaxIters:         maxIters,
				PlannerModel:     planner,
				WorkerModel:      worker,
				VerifierModel:    verifier,
				APIKey:           key,
				WorkerCmd:        workerCmd,
				BuildCmd:         splitArgs(buildCmd),
				TestCmd:          splitArgs(testCmd),
				E2ECmd:           splitArgs(e2eCmd),
				WorktreeLocation: worktreeLocation,
				GateTimeout:      gateTimeout,
			}

			client := anthropic.New(key).
				WithBaseURL(os.Getenv("ANTHROPIC_BASE_URL")).
				WithBearerToken(bearer)

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if totalTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, totalTimeout)
				defer cancel()
			}

			report, runErr := orchestrator.Run(ctx, oc, client)

			// Always emit the report even on error so the user sees partial work.
			fmt.Println(orchestrator.Summary(report))
			if reportPath != "" {
				if err := writeReport(reportPath, report); err != nil {
					fmt.Fprintln(os.Stderr, "failed to write report:", err)
				} else {
					fmt.Fprintln(os.Stderr, "report written to", reportPath)
				}
			}
			if runErr != nil {
				return runErr
			}
			if !report.Passed {
				return errors.New("orchestration finished with failing subtasks")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "development goal to accomplish (required)")
	cmd.Flags().StringVar(&repo, "repo", "", "target repository path (prompted interactively if omitted; required when non-interactive)")
	cmd.Flags().IntVar(&workers, "workers", 3, "max concurrent workers")
	cmd.Flags().IntVar(&maxIters, "max-iters", 4, "per-worker self-repair iteration budget")
	cmd.Flags().StringVar(&workerModel, "worker-model", "", "coding-agent model id (overrides config/default)")
	cmd.Flags().StringVar(&verifierModel, "verifier-model", "", "cross-model verifier model id (should differ from worker)")
	cmd.Flags().StringVar(&plannerModel, "planner-model", "", "planner model id (overrides config/default)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "Anthropic API key (or $ANTHROPIC_API_KEY)")
	cmd.Flags().StringVar(&workerCmd, "worker-cmd", "claude", "headless coding-agent binary")
	cmd.Flags().StringVar(&buildCmd, "build-cmd", "", "override build command (space-separated argv)")
	cmd.Flags().StringVar(&testCmd, "test-cmd", "", "override unit-test command (space-separated argv)")
	cmd.Flags().StringVar(&e2eCmd, "e2e-cmd", "", "product/e2e command run as an extra gate (space-separated argv)")
	cmd.Flags().StringVar(&worktreeLocation, "worktree-location", "sibling", "worktree placement: sibling|subdirectory")
	cmd.Flags().StringVar(&reportPath, "report", "", "write the JSON run report to this path")
	cmd.Flags().DurationVar(&gateTimeout, "gate-timeout", 10*time.Minute, "per-gate command timeout")
	cmd.Flags().DurationVar(&totalTimeout, "timeout", 0, "overall wall-clock budget (0 = none)")
	return cmd
}

// resolveRepoPath returns the target repository path for an orchestration run.
// If repo is empty, the user did not specify a workspace: on an interactive TTY
// we prompt for one (empty input accepts the current working directory);
// without a TTY we refuse, forcing non-interactive callers to pass --repo so a
// multi-agent run never writes generated code to an unintended location. The
// result is always normalized (tilde-expanded and made absolute) so the
// downstream orchestrator's RepoDir honors its "absolute path" contract and
// worktrees are placed deterministically rather than relative to the cwd.
func resolveRepoPath(cmd *cobra.Command, repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		cwd, _ := os.Getwd()
		if !readerIsTerminal(cmd.InOrStdin()) {
			return "", errors.New("--repo is required: specify the target repository path for the multi-agent run (no interactive terminal available to prompt)")
		}
		fmt.Fprintf(cmd.OutOrStdout(), "No --repo given. Enter the target repository path for this multi-agent run\n[default: %s]: ", cwd)
		line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		repo = strings.TrimSpace(line)
		if repo == "" {
			if cwd == "" {
				return "", errors.New("could not determine current directory; pass --repo explicitly")
			}
			repo = cwd
		}
	}
	return normalizePath(repo)
}

// normalizePath expands a leading "~" (no shell does it for prompt/flag input)
// and resolves the path to an absolute one.
func normalizePath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
	}
	return filepath.Abs(p)
}

// readerIsTerminal reports whether r is an interactive character device (a TTY)
// rather than a pipe/file/redirect. It inspects the same reader the prompt will
// read from — so injecting a pipe via cmd.SetIn both disables the prompt and is
// honored consistently. Uses only stdlib so no new dependency is introduced.
func readerIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// splitArgs splits a space-separated command string into an argv slice,
// returning nil for an empty string. This is a simple whitespace split; commands
// needing embedded spaces should be wrapped in a script.
func splitArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// writeReport serializes the report as indented JSON to path.
func writeReport(path string, r orchestrator.Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
