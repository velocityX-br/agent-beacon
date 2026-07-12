// Package orchestrator coordinates a virtual team of coding-agent workers to
// complete a development task autonomously. A planner model decomposes the task
// into isolated subtasks; each subtask runs in its own git worktree with a
// self-repair loop (headless agent -> deterministic gates -> cross-model
// verifier), looping until it passes or a per-worker iteration budget is spent.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/local/agent-beacon/internal/anthropic"
	"github.com/local/agent-beacon/internal/verify"
)

// Config is the fully-resolved orchestrator configuration (flags/env/file are
// merged by the CLI before construction).
type Config struct {
	RepoDir  string // validated absolute repo path
	Task     string // top-level goal
	Workers  int    // max concurrent workers
	MaxIters int    // per-worker self-repair iteration budget

	PlannerModel  string // model used to decompose the task
	WorkerModel   string // model the headless coding agent runs as
	VerifierModel string // cross-model judge (should differ from WorkerModel)

	APIKey    string // Anthropic key (never logged)
	WorkerCmd string // coding-agent binary, default "claude"

	// Gate command overrides (argv, space-split by the CLI). Empty => auto.
	BuildCmd []string
	TestCmd  []string
	E2ECmd   []string

	WorktreeLocation string // "sibling" (default) or "subdirectory"

	// GateTimeout bounds each individual gate command; 0 => no per-gate limit.
	GateTimeout time.Duration

	// Emitter, when non-nil, receives progress events as the run proceeds
	// (plan, per-worker iterations, gates, verdicts, done). It is optional: the
	// CLI leaves it nil for the original batch behavior. See events.go.
	Emitter Emitter

	// Intervener, when non-nil, is consulted when the worker loop pauses for a
	// user decision — authorization of a dangerous operation, or optional
	// guidance when the iteration budget is exhausted. The server implements
	// this to block on a per-run response channel. A nil Intervener (the CLI
	// path and all existing tests) means: authorization auto-DENIED (safe
	// headless default, which never had danger gating) and no guidance, so the
	// loop never pauses.
	Intervener Intervener
}

// Decision is the outcome of a user intervention: whether to approve the
// pending action and any free-text guidance to feed back to the coding agent.
type Decision struct {
	Approve  bool
	Guidance string
}

// InterventionRequest describes why the worker loop is pausing so the UI can
// present it. Kind is "auth" (dangerous op needs approval) or "input" (optional
// guidance, e.g. budget exhausted).
type InterventionRequest struct {
	ReqID     string
	SubtaskID string
	Branch    string
	Kind      string
	Summary   string
	Detail    string
}

// Intervener blocks until a user decision is available (or the ctx expires, in
// which case it must return a safe default: Approve=false / empty Guidance).
type Intervener interface {
	Await(ctx context.Context, req InterventionRequest) Decision
}

// awaitDecision consults cfg.Intervener when configured, else returns the safe
// headless default (deny / no guidance) so a nil Intervener preserves the
// original non-interactive behavior.
func awaitDecision(ctx context.Context, cfg Config, req InterventionRequest) Decision {
	if cfg.Intervener == nil {
		return Decision{Approve: false}
	}
	return cfg.Intervener.Await(ctx, req)
}

// Subtask is one unit of work assigned to a worker.
type Subtask struct {
	ID         string   `json:"id"`
	Goal       string   `json:"goal"`
	Acceptance []string `json:"acceptance"`
	Branch     string   `json:"branch"`
}

// WorkerResult captures the outcome of running one subtask to completion (or
// budget exhaustion).
type WorkerResult struct {
	Subtask      Subtask   `json:"subtask"`
	Passed       bool      `json:"passed"`
	Iterations   int       `json:"iterations"`
	WorktreePath string    `json:"worktree_path"`
	Branch       string    `json:"branch"`
	Log          []string  `json:"log"`
	VerdictNotes []string  `json:"verdict_notes,omitempty"`
	Error        string    `json:"error,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	DurationMS   int64     `json:"duration_ms"`
	// Gates holds the final iteration's deterministic Gate 1 results — the
	// "testing done" surfaced in the UI.
	Gates []verify.GateResult `json:"gates,omitempty"`
	// Artifacts lists the worktree-relative paths changed by the worker,
	// derived from the final iteration's diff.
	Artifacts []string `json:"artifacts,omitempty"`
	// Repairs records each recoverable environment/infra failure the loop
	// auto-fixed (self-repair), for the report and persisted history.
	Repairs []RepairRecord `json:"repairs,omitempty"`
	// Interventions records each user authorization/guidance decision.
	Interventions []InterventionRecord `json:"interventions,omitempty"`
}

// RepairRecord is one auto-self-repair action taken by the worker loop when a
// recoverable environment/infra failure was classified.
type RepairRecord struct {
	Iteration int       `json:"iteration"`
	Kind      string    `json:"kind"`   // "environment" | "logic"
	Reason    string    `json:"reason"` // short human summary
	Action    string    `json:"action"` // what was fed back
	Time      time.Time `json:"time"`
}

// InterventionRecord captures one user decision (authorization or guidance) at
// a pause point in the worker loop.
type InterventionRecord struct {
	ReqID    string    `json:"req_id"`
	Kind     string    `json:"kind"` // "auth" | "input"
	Summary  string    `json:"summary"`
	Approved bool      `json:"approved"`
	Guidance string    `json:"guidance,omitempty"`
	Time     time.Time `json:"time"`
}

// Report is the structured result of a full orchestration run.
type Report struct {
	Task       string         `json:"task"`
	RepoDir    string         `json:"repo_dir"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	DurationMS int64          `json:"duration_ms"`
	Passed     bool           `json:"passed"`
	Subtasks   int            `json:"subtasks"`
	Results    []WorkerResult `json:"results"`
	Languages  []string       `json:"languages"`
}

// planTool is the forced tool the planner must call to emit its decomposition.
const planToolName = "emit_plan"

func planTool() anthropic.Tool {
	return anthropic.Tool{
		Name:        planToolName,
		Description: "Emit the decomposition of the development task into independent subtasks that can be worked on in parallel.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"subtasks": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]any{
							"goal":       map[string]any{"type": "string", "description": "A precise, self-contained goal for one worker."},
							"branch":     map[string]any{"type": "string", "description": "A short git branch slug (letters, digits, dash)."},
							"acceptance": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Concrete acceptance criteria used to judge completion."},
						},
						"required": []string{"goal", "branch", "acceptance"},
					},
				},
			},
			"required": []string{"subtasks"},
		},
	}
}

// Plan asks the planner model to decompose cfg.Task into subtasks via a forced
// tool call, returning them with sanitized, unique branch names.
func Plan(ctx context.Context, cfg Config, m anthropic.Messenger) ([]Subtask, error) {
	sys := "You are a senior engineering lead. Decompose the development task into the smallest set of INDEPENDENT subtasks that can be implemented in parallel git worktrees without stepping on each other. Prefer 1 subtask for a small task. Each subtask needs a precise goal, a short branch slug, and concrete acceptance criteria. Call the emit_plan tool exactly once."
	user := fmt.Sprintf("Repository: %s\n\nTask: %s", cfg.RepoDir, cfg.Task)

	resp, err := m.Messages(ctx, anthropic.Request{
		Model:      cfg.PlannerModel,
		MaxTokens:  2048,
		System:     sys,
		Messages:   []anthropic.Message{{Role: "user", Content: user}},
		Tools:      []anthropic.Tool{planTool()},
		ToolChoice: &anthropic.ToolChoice{Type: "tool", Name: planToolName},
	})
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	raw, ok := resp.ToolInput(planToolName)
	if !ok {
		return nil, fmt.Errorf("plan: model did not call %s", planToolName)
	}
	var parsed struct {
		Subtasks []struct {
			Goal       string   `json:"goal"`
			Branch     string   `json:"branch"`
			Acceptance []string `json:"acceptance"`
		} `json:"subtasks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("plan: decode plan: %w", err)
	}
	if len(parsed.Subtasks) == 0 {
		return nil, fmt.Errorf("plan: empty plan")
	}

	seen := map[string]int{}
	out := make([]Subtask, 0, len(parsed.Subtasks))
	for i, s := range parsed.Subtasks {
		slug := slugify(s.Branch)
		if slug == "" {
			slug = fmt.Sprintf("task-%d", i+1)
		}
		// Ensure branch uniqueness across subtasks.
		if n := seen[slug]; n > 0 {
			seen[slug] = n + 1
			slug = fmt.Sprintf("%s-%d", slug, n+1)
		} else {
			seen[slug] = 1
		}
		out = append(out, Subtask{
			ID:         fmt.Sprintf("st-%d", i+1),
			Goal:       strings.TrimSpace(s.Goal),
			Acceptance: s.Acceptance,
			Branch:     "orch/" + slug,
		})
	}
	return out, nil
}

// Run plans the task, dispatches workers concurrently (bounded by cfg.Workers),
// aggregates their results, and returns a Report. The parent ctx bounds total
// wall-clock so the loop can never run forever.
func Run(ctx context.Context, cfg Config, m anthropic.Messenger) (Report, error) {
	report := Report{
		Task:      cfg.Task,
		RepoDir:   cfg.RepoDir,
		StartedAt: time.Now(),
	}
	for _, l := range verify.Detect(cfg.RepoDir) {
		report.Languages = append(report.Languages, l.Name)
	}

	subtasks, err := Plan(ctx, cfg, m)
	if err != nil {
		report.FinishedAt = time.Now()
		report.DurationMS = report.FinishedAt.Sub(report.StartedAt).Milliseconds()
		emit(cfg, Event{Kind: EventRunDone, Passed: boolPtr(false), Message: "planning failed", Detail: err.Error()})
		return report, err
	}
	report.Subtasks = len(subtasks)
	emit(cfg, Event{Kind: EventPlan, Message: fmt.Sprintf("planned %d subtask(s)", len(subtasks)), Detail: strconv.Itoa(len(subtasks))})

	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	results := make([]WorkerResult, len(subtasks))
	var wg sync.WaitGroup

	for i, st := range subtasks {
		wg.Add(1)
		go func(idx int, st Subtask) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[idx] = WorkerResult{Subtask: st, Error: ctx.Err().Error()}
				return
			}
			results[idx] = RunWorker(ctx, cfg, st, m)
		}(i, st)
	}
	wg.Wait()

	report.Results = results
	report.Passed = true
	for _, r := range results {
		if !r.Passed {
			report.Passed = false
			break
		}
	}
	report.FinishedAt = time.Now()
	report.DurationMS = report.FinishedAt.Sub(report.StartedAt).Milliseconds()
	emit(cfg, Event{Kind: EventRunDone, Passed: boolPtr(report.Passed), Message: Summary(report)})
	return report, nil
}

// slugify converts an arbitrary string into a conservative branch slug:
// lowercase alnum with single dashes, no leading/trailing dash.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Summary renders a short human-readable summary of a Report.
func Summary(r Report) string {
	var b strings.Builder
	status := "FAIL"
	if r.Passed {
		status = "PASS"
	}
	fmt.Fprintf(&b, "Orchestration %s — %d subtask(s), %s\n", status, r.Subtasks, r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
	if len(r.Languages) > 0 {
		fmt.Fprintf(&b, "Languages: %s\n", strings.Join(r.Languages, ", "))
	}
	// Stable ordering by subtask ID for readable output.
	res := append([]WorkerResult(nil), r.Results...)
	sort.SliceStable(res, func(i, j int) bool { return res[i].Subtask.ID < res[j].Subtask.ID })
	for _, w := range res {
		mark := "✗"
		if w.Passed {
			mark = "✓"
		}
		fmt.Fprintf(&b, "  %s [%s] %s (iters=%d, branch=%s)\n", mark, w.Subtask.ID, w.Subtask.Goal, w.Iterations, w.Branch)
		if w.Error != "" {
			fmt.Fprintf(&b, "      error: %s\n", w.Error)
		}
		for _, n := range w.VerdictNotes {
			fmt.Fprintf(&b, "      note: %s\n", n)
		}
	}
	return strings.TrimSpace(b.String())
}
