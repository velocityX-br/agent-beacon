package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/local/agent-beacon/pkg/protocol"
)

// spawnConfig is the subset of Options a spawn handler needs to launch a new
// detached session on this host.
type spawnConfig struct {
	serverURL string
	token     string
	device    string
	pinned    bool
	roots     []string // allowed project roots; a spawn target must live under one
}

// SpawnConfig is the exported form of spawnConfig so other packages (the monitor
// daemon) can reuse the validated spawn+worktree launch logic.
type SpawnConfig struct {
	ServerURL string
	Token     string
	Device    string
	Pinned    bool
	Roots     []string
}

// Spawn launches a new detached managed session for a dashboard-originated
// request, reusing the same validation, worktree, and detach logic as the wrap
// agent's own spawn handling. The monitor daemon calls this when it receives a
// FrameSpawn over its role=monitor connection.
func Spawn(ctx context.Context, cfg SpawnConfig, msg *protocol.SpawnMsg) error {
	return handleSpawn(ctx, spawnConfig{
		serverURL: cfg.ServerURL,
		token:     cfg.Token,
		device:    cfg.Device,
		pinned:    cfg.Pinned,
		roots:     cfg.Roots,
	}, msg)
}

// handleSpawn validates a dashboard spawn request and launches a new detached
// `agent-beacon session -- <command>` in the target directory (optionally a
// fresh git worktree). It returns an error describing any refusal; callers log
// it. The new session connects to the same server on its own WebSocket.
func handleSpawn(ctx context.Context, cfg spawnConfig, msg *protocol.SpawnMsg) error {
	if msg == nil {
		return fmt.Errorf("spawn: empty message")
	}
	target, err := resolveSpawnTarget(cfg.roots, msg.ProjectPath)
	if err != nil {
		return err
	}

	workdir := target
	if msg.WorktreeBranch != "" {
		wt, err := createWorktree(ctx, target, msg.WorktreeBranch, msg.WorktreeLocation)
		if err != nil {
			return fmt.Errorf("spawn: worktree: %w", err)
		}
		workdir = wt
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("spawn: locate self: %w", err)
	}

	command := msg.Command
	if strings.TrimSpace(command) == "" {
		command = "claude"
	}

	// Build: agent-beacon session -- <command...>
	args := []string{"session"}
	if cfg.device != "" && cfg.pinned {
		args = append(args, "--device", cfg.device)
	}
	args = append(args, "--")
	args = append(args, strings.Fields(command)...)

	child := exec.Command(self, args...)
	child.Dir = workdir
	child.Env = append(os.Environ(),
		"AGENT_BEACON_URL="+cfg.serverURL,
		"AGENT_BEACON_AUTH_TOKEN="+cfg.token,
	)
	// Detach: the child manages its own lifecycle and connects independently.
	// We do not wire its stdio to ours; it runs headless (no local PTY input).
	child.Stdin = nil
	child.Stdout = nil
	child.Stderr = nil
	if err := child.Start(); err != nil {
		return fmt.Errorf("spawn: start child: %w", err)
	}
	// Release the child so it is not reaped as our own; it lives on its own.
	go func() { _ = child.Wait() }()
	return nil
}

// CreateWorktree is an exported wrapper around createWorktree so other packages
// (the orchestrator) can reuse the same validated worktree-add logic without
// duplicating it. It runs `git -C <repo> worktree add -B <branch> <path>`.
func CreateWorktree(ctx context.Context, repo, branch, location string) (string, error) {
	return createWorktree(ctx, repo, branch, location)
}

// resolveSpawnTarget verifies the requested project path is a real directory
// under one of the allowed roots (symlink-resolved), preventing traversal to
// arbitrary host paths from a dashboard request.
func resolveSpawnTarget(roots []string, projectPath string) (string, error) {
	if projectPath == "" {
		return "", fmt.Errorf("spawn: project_path required")
	}
	if len(roots) == 0 {
		return "", fmt.Errorf("spawn: no project roots configured on this host")
	}
	real, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return "", fmt.Errorf("spawn: bad project_path: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("spawn: project_path is not a directory")
	}
	for _, root := range roots {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		// real must be within realRoot (path-boundary safe).
		rel, err := filepath.Rel(realRoot, real)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return real, nil
		}
	}
	return "", fmt.Errorf("spawn: project_path escapes allowed roots")
}

// createWorktree runs `git worktree add` for branch off the target repo. The
// worktree is placed as a sibling directory (default) or a subdirectory named
// after the branch. Returns the new worktree path.
func createWorktree(ctx context.Context, repo, branch, location string) (string, error) {
	safeBranch := sanitizeBranch(branch)
	if safeBranch == "" {
		return "", fmt.Errorf("invalid branch name")
	}
	var wtPath string
	switch location {
	case "subdirectory":
		wtPath = filepath.Join(repo, ".worktrees", safeBranch)
	default: // "sibling" or empty
		wtPath = filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-"+safeBranch)
	}
	// -B creates or resets the branch, so re-spawns are idempotent.
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "-B", safeBranch, wtPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return wtPath, nil
}

// sanitizeBranch rejects branch names that could inject path or shell trouble.
// We allow a conservative subset used by real branches.
func sanitizeBranch(b string) string {
	b = strings.TrimSpace(b)
	if b == "" || strings.Contains(b, "..") || strings.HasPrefix(b, "-") {
		return ""
	}
	for _, r := range b {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '/' || r == '-' || r == '_' || r == '.'
		if !ok {
			return ""
		}
	}
	return b
}
