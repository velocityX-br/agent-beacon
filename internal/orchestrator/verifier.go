package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/local/agent-beacon/internal/anthropic"
)

// Verdict is the cross-model verifier's structured judgment of whether a
// subtask goal was met.
type Verdict struct {
	Pass    bool     `json:"pass"`
	Reasons []string `json:"reasons"`
	Missing []string `json:"missing"`
}

const (
	verdictToolName = "emit_verdict"
	verdictPass     = "pass"
	verdictFail     = "fail"
)

func verdictTool() anthropic.Tool {
	return anthropic.Tool{
		Name:        verdictToolName,
		Description: "Emit an independent verdict on whether the worker met the subtask goal and acceptance criteria.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"verdict": map[string]any{
					"type":        "string",
					"enum":        []string{verdictPass, verdictFail},
					"description": "pass only if the goal AND all acceptance criteria are demonstrably met.",
				},
				"reasons": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Concise justification for the verdict.",
				},
				"missing": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Specific gaps to fix if the verdict is fail.",
				},
			},
			"required": []string{"verdict", "reasons"},
		},
	}
}

// Verify asks the verifier model (a DIFFERENT model than the worker) to judge
// goal-completion from the subtask, the code diff, and the deterministic gate
// output — via a forced tool_choice so the response is always structured.
// Extended thinking is intentionally NOT enabled: it is incompatible with
// forced tool choice.
func Verify(ctx context.Context, cfg Config, st Subtask, diff, gateOutput string, m anthropic.Messenger) (Verdict, error) {
	sys := "You are an independent code reviewer. You did NOT write this code. Judge strictly whether the stated goal and every acceptance criterion are demonstrably satisfied by the diff and the passing deterministic gates. Do not give the benefit of the doubt. Call the emit_verdict tool exactly once."

	var b strings.Builder
	writeGoal(&b, st)
	if strings.TrimSpace(gateOutput) != "" {
		fmt.Fprintf(&b, "DETERMINISTIC GATE OUTPUT (build/test/e2e):\n%s\n\n", clip(gateOutput, 8000))
	}
	if strings.TrimSpace(diff) != "" {
		fmt.Fprintf(&b, "CODE DIFF:\n%s\n", clip(diff, 40000))
	} else {
		b.WriteString("CODE DIFF: (none captured)\n")
	}

	resp, err := m.Messages(ctx, anthropic.Request{
		Model:      cfg.VerifierModel,
		MaxTokens:  1024,
		System:     sys,
		Messages:   []anthropic.Message{{Role: "user", Content: b.String()}},
		Tools:      []anthropic.Tool{verdictTool()},
		ToolChoice: &anthropic.ToolChoice{Type: "tool", Name: verdictToolName},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("verify: %w", err)
	}
	raw, ok := resp.ToolInput(verdictToolName)
	if !ok {
		return Verdict{}, fmt.Errorf("verify: model did not call %s", verdictToolName)
	}
	var parsed struct {
		Verdict string      `json:"verdict"`
		Reasons flexStrings `json:"reasons"`
		Missing flexStrings `json:"missing"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Verdict{}, fmt.Errorf("verify: decode verdict: %w", err)
	}
	return Verdict{
		Pass:    strings.EqualFold(strings.TrimSpace(parsed.Verdict), verdictPass),
		Reasons: parsed.Reasons,
		Missing: parsed.Missing,
	}, nil
}

// flexStrings decodes a JSON field that should be a []string but which some
// models (notably Opus 4.6 in tool_use inputs) sometimes emit as a bare string.
// It accepts either shape: a JSON array of strings, or a single JSON string
// (wrapped into a one-element slice). Any other shape decodes to nil.
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*f = nil
		return nil
	}
	// Array form: the schema-correct shape.
	if data[0] == '[' {
		var arr []string
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		*f = arr
		return nil
	}
	// String form: a single justification returned as a bare string.
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			*f = nil
		} else {
			*f = flexStrings{s}
		}
		return nil
	}
	return fmt.Errorf("flexStrings: expected array or string, got %s", data)
}

// clip truncates s to at most n bytes, marking the cut so the model knows the
// content was abridged.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]..."
}

// writeGoal writes the shared GOAL / ACCEPTANCE CRITERIA block used by both the
// worker prompt and the verifier prompt.
func writeGoal(b *strings.Builder, st Subtask) {
	fmt.Fprintf(b, "GOAL:\n%s\n\n", st.Goal)
	if len(st.Acceptance) > 0 {
		b.WriteString("ACCEPTANCE CRITERIA:\n")
		for _, a := range st.Acceptance {
			b.WriteString("- " + a + "\n")
		}
		b.WriteString("\n")
	}
}
