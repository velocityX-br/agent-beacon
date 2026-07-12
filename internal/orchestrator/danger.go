package orchestrator

import (
	"regexp"
	"strings"
)

// DangerHit records a dangerous command observed in the coding agent's tool_use
// stream, along with the denylist rule that matched. It is collected during a
// single agent invocation and surfaced to the worker loop so continuation can
// be gated on user authorization.
type DangerHit struct {
	Rule    string `json:"rule"`
	Command string `json:"command"`
}

// dangerRules is a conservative denylist of destructive shell patterns. Each
// entry has a human-readable rule label and a case-insensitive regexp. The
// patterns are prefix/word-boundary aware so ordinary commands (e.g.
// "rm file.txt", a Go "remove()" call) do not match. This is best-effort
// heuristic detection, not a security boundary — see Guardrails in the plan.
var dangerRules = []struct {
	rule string
	re   *regexp.Regexp
}{
	{"rm -rf", regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-?(rf|fr)\b`)},
	{"rmdir", regexp.MustCompile(`(?i)\brmdir\b`)},
	{"kill", regexp.MustCompile(`(?i)\b(kill|killall|pkill)\b`)},
	{"dd", regexp.MustCompile(`(?i)\bdd\s`)},
	{"mkfs", regexp.MustCompile(`(?i)\bmkfs\b`)},
	{"git push --force", regexp.MustCompile(`(?i)\bgit\s+push\b[^|;&]*(--force\b|--force-with-lease\b|\s-f\b)`)},
	{"git reset --hard", regexp.MustCompile(`(?i)\bgit\s+reset\b[^|;&]*--hard\b`)},
	{"overwrite block device", regexp.MustCompile(`(?i)>\s*/dev/sd`)},
	{"chmod -R 000", regexp.MustCompile(`(?i)\bchmod\s+-R\s+0{3}\b`)},
	{"fork bomb", regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`)},
}

// scanDangerous reports whether cmd matches any destructive denylist rule and
// returns the matched rule label. It is pure and unit-tested. Matching is
// conservative: only recognizably destructive invocations trip a rule.
func scanDangerous(cmd string) (matched bool, rule string) {
	s := strings.TrimSpace(cmd)
	if s == "" {
		return false, ""
	}
	for _, r := range dangerRules {
		if r.re.MatchString(s) {
			return true, r.rule
		}
	}
	return false, ""
}

// envErrorSignals are substrings that indicate a failure is an environment/
// infrastructure problem (missing tool, bad path, permissions) rather than a
// logic error in the produced code. Matched case-insensitively.
var envErrorSignals = []string{
	"command not found",
	"no such file or directory",
	"executable file not found",
	"permission denied",
	"start: ",
	"enoent",
	"not installed",
}

// classifyFailure returns "environment" when the agent/gate error text looks
// like a missing-tool or infra problem (so it is logged as a self-repair
// action), else "logic". It is pure and unit-tested.
func classifyFailure(errText string) (kind string) {
	s := strings.ToLower(errText)
	for _, sig := range envErrorSignals {
		if strings.Contains(s, sig) {
			return "environment"
		}
	}
	return "logic"
}
