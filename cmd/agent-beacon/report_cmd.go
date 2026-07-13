package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/local/agent-beacon/internal/config"
	"github.com/local/agent-beacon/internal/monitor"
)

// reportCmd is a thin client the Claude hook invokes to enrich the observed
// session for the current process. It POSTs metadata (model, context %, task,
// state, event) to the monitor daemon's loopback report listener, which merges
// it onto the matching scanned process by pid/cwd. Failures are non-fatal so a
// missing or down monitor never blocks a Claude session.
func reportCmd() *cobra.Command {
	var (
		addr       string
		token      string
		pid        int
		cwd        string
		sessionID  string
		model      string
		contextPct float64
		task       string
		state      string
		event      string
		prURL      string
		prState    string
		mcpServers []string
	)
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Send Claude session metadata to the local monitor (used by hooks)",
		Long: "Posts optional session metadata to the monitor's loopback report " +
			"listener so the dashboard can enrich the observed process card. " +
			"Intended to be called from Claude Code hooks. Non-fatal on error.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Default correlation keys to this process's context so hooks can be
			// wired with no arguments and still attach to the right card.
			if pid == 0 {
				pid = os.Getppid()
			}
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			// Token precedence: --token -> env -> token file.
			tok := firstNonEmpty(token, os.Getenv("AGENT_BEACON_AUTH_TOKEN"))
			if tok == "" {
				if dataDir, err := config.DataDir(); err == nil {
					if cfg, err := config.Load(dataDir); err == nil && cfg.TokenPath != "" {
						tok, _ = config.ReadToken(cfg.TokenPath)
					}
				}
			}

			body := map[string]any{}
			if pid > 0 {
				body["pid"] = pid
			}
			if cwd != "" {
				body["cwd"] = cwd
			}
			if sessionID != "" {
				body["session_id"] = sessionID
			}
			if model != "" {
				body["model"] = model
			}
			if contextPct != 0 {
				body["context_pct"] = contextPct
			}
			if task != "" {
				body["task"] = task
			}
			if state != "" {
				body["state"] = state
			}
			if event != "" {
				body["event"] = event
			}

			// PR + MCP self-collection: gather best-effort context from the cwd
			// when the caller did not pass explicit values. Both swallow all
			// errors so a hook never blocks or fails a Claude session (matches
			// the "monitor unreachable is fine" philosophy below). PR uses a
			// short-timeout `gh` call; MCP reads config files directly. A missing
			// gh binary, no-PR repo, or absent config simply omits the fields.
			prNumber := 0
			if prURL == "" && prState == "" {
				prNumber, prState, prURL = collectPR(cwd)
			}
			if len(mcpServers) == 0 {
				mcpServers = collectMCPServers(cwd)
			}
			if prURL != "" {
				body["pr_url"] = prURL
			}
			if prState != "" {
				body["pr_state"] = prState
			}
			if prNumber != 0 {
				body["pr_number"] = prNumber
			}
			if len(mcpServers) > 0 {
				body["mcp_servers"] = mcpServers
			}

			data, err := json.Marshal(body)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/report", bytes.NewReader(data))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			if tok != "" {
				req.Header.Set("X-Agent-Beacon-Token", tok)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				// The monitor may not be running; that's fine for a hook.
				fmt.Fprintln(os.Stderr, "report: monitor unreachable:", err)
				return nil
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				fmt.Fprintln(os.Stderr, "report: monitor rejected report:", resp.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", monitor.DefaultReportAddr, "monitor report listener address")
	cmd.Flags().StringVar(&token, "token", "", "report token (or $AGENT_BEACON_AUTH_TOKEN)")
	cmd.Flags().IntVar(&pid, "pid", 0, "process id to enrich (default: parent pid)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory to enrich (default: current)")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Claude session id")
	cmd.Flags().StringVar(&model, "model", "", "model name")
	cmd.Flags().Float64Var(&contextPct, "context-pct", 0, "context window used, percent")
	cmd.Flags().StringVar(&task, "task", "", "current task description")
	cmd.Flags().StringVar(&state, "state", "", "session state (running|idle|...)")
	cmd.Flags().StringVar(&event, "event", "", "hook event (SessionStart|Stop|heartbeat|Notification)")
	cmd.Flags().StringVar(&prURL, "pr-url", "", "pull request URL (default: self-collect via `gh pr view`)")
	cmd.Flags().StringVar(&prState, "pr-state", "", "pull request state (default: self-collect via `gh pr view`)")
	cmd.Flags().StringArrayVar(&mcpServers, "mcp", nil, "connected MCP server name (repeatable; default: read from ~/.claude.json and .mcp.json)")
	return cmd
}

// collectPR runs `gh pr view --json number,state,url` in dir and parses the
// result. Best-effort: any error (gh absent, not a repo, no PR) yields zeroes.
func collectPR(dir string) (number int, state, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", "--json", "number,state,url")
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return 0, "", ""
	}
	var pr struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(out, &pr); err != nil {
		return 0, "", ""
	}
	return pr.Number, pr.State, pr.URL
}

// collectMCPServers returns the names of MCP servers configured for the session
// running in dir. It reads Claude's config files directly rather than shelling
// out to `claude mcp list` (which health-checks every server and can block for
// several seconds). Best-effort: missing or malformed files yield nil so a hook
// never blocks or fails a session.
//
// Three configuration scopes are merged (de-duplicated):
//   - global user servers:  ~/.claude.json  ->  mcpServers
//   - project-scoped servers: ~/.claude.json -> projects[<dir>].mcpServers
//   - shared project servers: <dir>/.mcp.json -> mcpServers
func collectMCPServers(dir string) []string {
	var claudeJSON []byte
	if home, err := os.UserHomeDir(); err == nil {
		claudeJSON, _ = os.ReadFile(filepath.Join(home, ".claude.json"))
	}
	var projectMCP []byte
	if dir != "" {
		projectMCP, _ = os.ReadFile(filepath.Join(dir, ".mcp.json"))
	}
	return mcpServersFromConfig(claudeJSON, projectMCP, dir)
}

// mcpServersFromConfig extracts the merged, de-duplicated set of MCP server
// names from the raw config bytes. Split out from collectMCPServers so the
// parsing is unit-testable. Order: global user servers, then project-scoped
// servers from ~/.claude.json, then shared servers from the project .mcp.json.
func mcpServersFromConfig(claudeJSON, projectMCPJSON []byte, dir string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(names []string) {
		for _, n := range names {
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}

	if len(claudeJSON) > 0 {
		var cfg struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
			Projects   map[string]struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			} `json:"projects"`
		}
		if json.Unmarshal(claudeJSON, &cfg) == nil {
			add(sortedKeys(cfg.MCPServers))
			if dir != "" {
				if p, ok := cfg.Projects[dir]; ok {
					add(sortedKeys(p.MCPServers))
				}
			}
		}
	}

	if len(projectMCPJSON) > 0 {
		var pm struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(projectMCPJSON, &pm) == nil {
			add(sortedKeys(pm.MCPServers))
		}
	}
	return out
}

// sortedKeys returns the map keys in stable sorted order so the reported server
// list is deterministic across runs.
func sortedKeys(m map[string]json.RawMessage) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
