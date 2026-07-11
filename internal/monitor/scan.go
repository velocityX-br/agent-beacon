// Package monitor implements the observe-first side of agent-beacon: a persistent
// daemon that discovers locally-running Claude processes (via a process-table scan
// and optional Claude hook reports), reports them to the server as read-only
// observed sessions, and stays connected to receive explicit spawn requests.
package monitor

import (
	"os"
	"path/filepath"
	"strings"
)

// Process is one discovered claude process. It is read-only metadata; observed
// processes never get a PTY or a browser terminal.
type Process struct {
	Pid     int
	PPid    int
	Command string // full command line (argv joined by spaces)
	CWD     string // working directory, best-effort
	Branch  string // git branch derived from CWD, best-effort
}

// rawProc is the minimal per-process record the platform lister returns before
// filtering and enrichment.
type rawProc struct {
	Pid     int
	PPid    int
	Cmdline string
}

// claudeExeNames are the executable base names we consider a Claude coding agent.
var claudeExeNames = []string{"claude", "claude-code"}

// excludeSubstrings suppress false positives: our own wrapper (which already
// reports as a managed session), and common MCP/helper processes that merely
// mention "claude" in their path or arguments.
var excludeSubstrings = []string{
	"agent-beacon", // our managed wrap path reports itself; don't double-count
	"claude-mem",   // memory/observer helper
	"chroma-mcp",   // vector-store MCP helper
	"mcp-server",
}

// Scan lists local processes and returns those that look like a Claude agent,
// enriched with working directory and git branch where obtainable.
func Scan() ([]Process, error) {
	raws, err := listProcesses()
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, rp := range raws {
		if !isClaudeProcess(rp.Cmdline) {
			continue
		}
		p := Process{Pid: rp.Pid, PPid: rp.PPid, Command: rp.Cmdline}
		if cwd, err := processCWD(rp.Pid); err == nil && cwd != "" {
			p.CWD = cwd
			p.Branch = branchOf(cwd)
		}
		out = append(out, p)
	}
	return out, nil
}

// isClaudeProcess reports whether a command line is a Claude agent process.
// It matches on the executable (first argv token) basename rather than any
// occurrence of "claude" anywhere, so that arguments like "--model claude-..."
// on an unrelated shell do not produce false positives. Exclusion substrings
// win over a match.
func isClaudeProcess(cmdline string) bool {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		return false
	}
	for _, ex := range excludeSubstrings {
		if strings.Contains(cmdline, ex) {
			return false
		}
	}
	// The executable is the first whitespace-delimited token.
	exe := cmdline
	if i := strings.IndexByte(cmdline, ' '); i >= 0 {
		exe = cmdline[:i]
	}
	base := filepath.Base(exe)
	for _, name := range claudeExeNames {
		if base == name {
			return true
		}
	}
	return false
}

// branchOf derives the current git branch from a working directory by reading
// .git/HEAD (no git exec). It handles the worktree case where .git is a file
// containing "gitdir: <path>". Returns "" when it cannot determine a branch.
func branchOf(cwd string) string {
	dir := cwd
	for i := 0; i < 40 && dir != "" && dir != "/"; i++ {
		gitPath := filepath.Join(dir, ".git")
		info, err := os.Stat(gitPath)
		if err == nil {
			if info.IsDir() {
				return headBranch(filepath.Join(gitPath, "HEAD"))
			}
			// .git is a file (worktree/submodule): "gitdir: <path>".
			if gd := readGitdir(gitPath); gd != "" {
				return headBranch(filepath.Join(gd, "HEAD"))
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// headBranch reads a HEAD file and returns the short branch name for a symbolic
// ref ("ref: refs/heads/<name>"). Detached HEADs return "".
func headBranch(headPath string) string {
	data, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const p = "ref: refs/heads/"
	if strings.HasPrefix(line, p) {
		return strings.TrimPrefix(line, p)
	}
	return ""
}

// readGitdir resolves the target of a ".git" file ("gitdir: <path>"). Relative
// targets are resolved against the file's directory.
func readGitdir(gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const p = "gitdir: "
	if !strings.HasPrefix(line, p) {
		return ""
	}
	target := strings.TrimPrefix(line, p)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitFile), target)
	}
	return target
}
