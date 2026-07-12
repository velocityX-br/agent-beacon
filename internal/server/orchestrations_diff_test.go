package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/local/agent-beacon/internal/orchestrator"
)

// gitInit creates a repo with one committed file at dir, returning nothing.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "base")
}

// TestHandleOrchDiffReturnsWorktreeChanges proves the endpoint diffs the
// subtask worktree — including a brand-new (staged-not-committed) file — rather
// than the clean main repo working tree, which was the previous bug.
func TestHandleOrchDiffReturnsWorktreeChanges(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)

	// A worktree on an orch/* branch with a new, uncommitted file — mirroring
	// what a real worker leaves behind.
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", "orch/x", wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(wt, "hello.txt"), []byte("hi there"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{AuthProvider: "none"}, nil)
	run := &orchRun{
		ID:          "run-diff",
		Repo:        repo,
		status:      statusDone,
		subscribers: make(map[int]chan orchestrator.Event),
		pending:     make(map[string]chan orchestrator.Decision),
	}
	run.finish(orchestrator.Report{
		Passed:   true,
		Subtasks: 1,
		Results: []orchestrator.WorkerResult{
			{Branch: "orch/x", WorktreePath: wt, Passed: true},
		},
	}, nil)
	s.orch.runs[run.ID] = run

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// No branch param: whole-run diff surfaces the new file.
	body := getBody(t, ts.URL+"/api/v1/orchestrations/run-diff/diff", http.StatusOK)
	if !strings.Contains(body, "hello.txt") || !strings.Contains(body, "+hi there") {
		t.Fatalf("diff missing new file changes:\n%s", body)
	}

	// Branch-scoped diff resolves the same worktree.
	body = getBody(t, ts.URL+"/api/v1/orchestrations/run-diff/diff?branch=orch/x", http.StatusOK)
	if !strings.Contains(body, "hello.txt") {
		t.Fatalf("branch-scoped diff missing file:\n%s", body)
	}
}

func TestHandleOrchDiffUnknownRun(t *testing.T) {
	s := New(Config{AuthProvider: "none"}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	getBody(t, ts.URL+"/api/v1/orchestrations/nope/diff", http.StatusNotFound)
}

func TestHandleOrchDiffInvalidBranch(t *testing.T) {
	s := New(Config{AuthProvider: "none"}, nil)
	run := &orchRun{
		ID:          "run-badbranch",
		Repo:        t.TempDir(),
		status:      statusDone,
		subscribers: make(map[int]chan orchestrator.Event),
		pending:     make(map[string]chan orchestrator.Decision),
	}
	run.finish(orchestrator.Report{Passed: true}, nil)
	s.orch.runs[run.ID] = run
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	getBody(t, ts.URL+"/api/v1/orchestrations/run-badbranch/diff?branch=a%3Brm", http.StatusBadRequest)
}

// TestHandleOrchDiffUnfinishedRun proves an in-progress run (no report yet)
// yields 409 rather than an empty or misleading diff.
func TestHandleOrchDiffUnfinishedRun(t *testing.T) {
	s := New(Config{AuthProvider: "none"}, nil)
	run := &orchRun{
		ID:          "run-inflight",
		Repo:        t.TempDir(),
		status:      statusRunning,
		subscribers: make(map[int]chan orchestrator.Event),
		pending:     make(map[string]chan orchestrator.Decision),
	}
	s.orch.runs[run.ID] = run
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	getBody(t, ts.URL+"/api/v1/orchestrations/run-inflight/diff", http.StatusConflict)
}

func getBody(t *testing.T, url string, wantStatus int) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: expected %d, got %d\n%s", url, wantStatus, resp.StatusCode, b)
	}
	return string(b)
}
