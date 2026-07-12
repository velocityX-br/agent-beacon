package verify

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Gate is a single deterministic check: a command (argv) run in Dir.
type Gate struct {
	Name string   // human label, e.g. "go build", "unit tests", "e2e"
	Cmd  []string // argv; Cmd[0] is the executable
	Dir  string   // working directory
}

// GateResult is the outcome of running one Gate.
type GateResult struct {
	Name   string `json:"name"`             // mirrors Gate.Name
	Passed bool   `json:"passed"`           // true when the command exited 0
	Output string `json:"output,omitempty"` // combined stdout+stderr (trimmed)
	Err    string `json:"err,omitempty"`    // non-empty when the command failed to run or exited non-zero
}

// GatesFromLanguages expands detected languages into ordered gates (build, then
// extra checks, then unit tests) rooted at dir. Overrides, when non-empty,
// replace the auto-detected build/test/e2e commands so arbitrary repos work.
// buildCmd/testCmd override the language defaults entirely; e2eCmd adds a
// distinct product/e2e gate on top.
func GatesFromLanguages(dir string, langs []Language, buildCmd, testCmd, e2eCmd []string) []Gate {
	var gates []Gate

	if len(buildCmd) > 0 || len(testCmd) > 0 {
		// Explicit overrides fully replace language-detected build/test.
		if len(buildCmd) > 0 {
			gates = append(gates, Gate{Name: "build", Cmd: buildCmd, Dir: dir})
		}
		if len(testCmd) > 0 {
			gates = append(gates, Gate{Name: "test", Cmd: testCmd, Dir: dir})
		}
	} else {
		for _, l := range langs {
			if len(l.Build) > 0 {
				gates = append(gates, Gate{Name: l.Name + " build", Cmd: l.Build, Dir: dir})
			}
			for _, ex := range l.Extra {
				label := l.Name + " check"
				if len(ex) > 1 {
					label = l.Name + " " + ex[1]
				}
				gates = append(gates, Gate{Name: label, Cmd: ex, Dir: dir})
			}
			if len(l.Test) > 0 {
				gates = append(gates, Gate{Name: l.Name + " test", Cmd: l.Test, Dir: dir})
			}
		}
	}

	if len(e2eCmd) > 0 {
		gates = append(gates, Gate{Name: "e2e", Cmd: e2eCmd, Dir: dir})
	}
	return gates
}

// RunGates executes gates in order, stopping at the first failure (fail-fast:
// there is no point testing when the build is broken). perGate bounds each
// command; <=0 means no per-gate limit (the parent ctx still applies).
func RunGates(ctx context.Context, gates []Gate, perGate time.Duration) []GateResult {
	results := make([]GateResult, 0, len(gates))
	for _, g := range gates {
		res := runGate(ctx, g, perGate)
		results = append(results, res)
		if !res.Passed {
			break
		}
	}
	return results
}

// runGate runs a single gate with an optional per-gate timeout.
func runGate(ctx context.Context, g Gate, perGate time.Duration) GateResult {
	if len(g.Cmd) == 0 {
		return GateResult{Name: g.Name, Passed: false, Err: "empty command"}
	}
	cctx := ctx
	if perGate > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, perGate)
		defer cancel()
	}
	cmd := exec.CommandContext(cctx, g.Cmd[0], g.Cmd[1:]...)
	cmd.Dir = g.Dir
	out, err := cmd.CombinedOutput()
	res := GateResult{Name: g.Name, Output: strings.TrimSpace(string(out))}
	if err != nil {
		res.Passed = false
		res.Err = err.Error()
		return res
	}
	res.Passed = true
	return res
}

// AllPassed reports whether every result passed. An empty slice is considered
// passing (no gates to fail).
func AllPassed(results []GateResult) bool {
	for _, r := range results {
		if !r.Passed {
			return false
		}
	}
	return true
}

// Summarize renders gate results as feedback text for a repair prompt: only
// failing gates' names and captured output are included.
func Summarize(results []GateResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Passed {
			continue
		}
		b.WriteString("GATE FAILED: ")
		b.WriteString(r.Name)
		b.WriteString("\n")
		if r.Err != "" {
			b.WriteString("error: " + r.Err + "\n")
		}
		if r.Output != "" {
			b.WriteString(r.Output + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
