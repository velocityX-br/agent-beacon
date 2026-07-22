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
	var target string
	var err error
	if msg.TrustedCwd {
		target, err = resolveTrustedCwd(msg.ProjectPath)
	} else {
		target, err = resolveSpawnTarget(cfg.roots, msg.ProjectPath)
	}
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
	child.Env = append(cleanClaudeEnv(os.Environ()),
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

// cleanClaudeEnv strips Claude Code's nested-session guard variables from an
// inherited environment. When the beacon itself runs inside a Claude Code
// session, CLAUDECODE (and friends) leak through os.Environ() into any spawned
// `claude` child, which then refuses to start ("cannot be launched inside
// another Claude Code session"). Dropping these makes the spawned/cloned
// session a clean top-level session regardless of where the beacon was started.
func cleanClaudeEnv(env []string) []string {
	drop := map[string]bool{
		"CLAUDECODE":             true,
		"CLAUDE_CODE_ENTRYPOINT": true,
		"CLAUDE_CODE_SSE_PORT":   true,
	}
	out := env[:0:0] // fresh slice, do not mutate caller's backing array
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if drop[key] || strings.HasPrefix(key, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// CreateWorktree is an exported wrapper around createWorktree so other packages
// (the orchestrator) can reuse the same validated worktree-add logic without
// duplicating it. It runs `git -C <repo> worktree add -B <branch> <path>`.
func CreateWorktree(ctx context.Context, repo, branch, location string) (string, error) {
	return createWorktree(ctx, repo, branch, location)
}

// ResolveUnderRoot verifies that projectPath is a real directory under one of
// the allowed roots (symlink-resolved), returning the resolved absolute path.
// It is the exported form of the same path-boundary check the spawn handler
// uses, so browser-initiated orchestration runs can only target server-approved
// workspaces — never arbitrary host paths.
func ResolveUnderRoot(roots []string, projectPath string) (string, error) {
	return resolveSpawnTarget(roots, projectPath)
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

// resolveTrustedCwd validates a server-authoritative cwd for a clone. Trusted
// (a process already runs there) so it need not pass the roots allowlist; we
// still symlink-resolve and Stat it so a vanished/bad path fails cleanly.
func resolveTrustedCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("spawn: trusted cwd empty")
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("spawn: bad trusted cwd: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("spawn: trusted cwd is not a directory")
	}
	return real, nil
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
