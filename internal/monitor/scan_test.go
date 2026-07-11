package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsClaudeProcess(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{"bare claude", "claude", true},
		{"absolute claude", "/Users/x/.local/bin/claude --output-format stream-json", true},
		{"claude-code", "/usr/local/bin/claude-code", true},
		{"our wrapper is excluded", "/path/agent-beacon session --device x -- bash -i", false},
		{"claude-mem helper excluded", "/Users/x/.bun/bin/bun claude-mem/worker.cjs", false},
		{"chroma mcp excluded", "uv tool uvx chroma-mcp --data-dir /x", false},
		{"model arg must not match", "/bin/zsh -c eval 'run --model claude-sonnet-4-6'", false},
		{"unrelated", "/usr/sbin/cupsd -l", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClaudeProcess(tc.cmdline); got != tc.want {
				t.Fatalf("isClaudeProcess(%q) = %v, want %v", tc.cmdline, got, tc.want)
			}
		})
	}
}

func TestBranchOf(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/feature-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := branchOf(dir); got != "feature-x" {
		t.Fatalf("branchOf standard repo = %q, want feature-x", got)
	}

	// Sub-directory should walk up to find the repo root.
	sub := filepath.Join(dir, "pkg", "inner")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := branchOf(sub); got != "feature-x" {
		t.Fatalf("branchOf subdir = %q, want feature-x", got)
	}

	// Detached HEAD -> empty branch.
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("deadbeefdeadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := branchOf(dir); got != "" {
		t.Fatalf("branchOf detached = %q, want empty", got)
	}

	// No repo at all.
	if got := branchOf(t.TempDir()); got != "" {
		t.Fatalf("branchOf non-repo = %q, want empty", got)
	}
}

func TestBranchOfWorktree(t *testing.T) {
	// Simulate a linked worktree: .git is a FILE pointing at a gitdir whose HEAD
	// carries the branch.
	repo := t.TempDir()
	realGitdir := filepath.Join(repo, "realgit")
	if err := os.MkdirAll(realGitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realGitdir, "HEAD"), []byte("ref: refs/heads/wt-branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(repo, "worktree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+realGitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := branchOf(wt); got != "wt-branch" {
		t.Fatalf("branchOf worktree = %q, want wt-branch", got)
	}
}
