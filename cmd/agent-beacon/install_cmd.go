package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/local/agent-beacon/internal/config"
)

// managedMarker tags the hooks we inject so uninstall can find and remove them
// without disturbing the user's own hand edits.
const managedMarker = "agent-beacon-managed"

func installCmd() *cobra.Command {
	var (
		url    string
		token  string
		device string
		yes    bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Claude Code hooks that enrich observed sessions",
		Long: "Resolves the dashboard URL and token, writes config.toml, and injects " +
			"managed report hooks into ~/.claude/settings.json so the monitor daemon " +
			"can label each observed Claude session with model, task, and state. " +
			"Idempotent — safe to re-run. Run `agent-beacon monitor` to see sessions.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, err := config.DataDir()
			if err != nil {
				return err
			}
			cfg, _ := config.Load(dataDir)

			// URL / token precedence: flag -> env -> existing config -> default.
			cfg.URL = firstNonEmpty(url, os.Getenv("AGENT_BEACON_URL"), cfg.URL, "http://localhost:8090")
			tok := firstNonEmpty(token, os.Getenv("AGENT_BEACON_AUTH_TOKEN"))
			if tok != "" {
				p, err := config.WriteToken(dataDir, tok)
				if err != nil {
					return err
				}
				cfg.TokenPath = p
			}
			if cfg.TokenPath == "" {
				return fmt.Errorf("no token provided; set --token or $AGENT_BEACON_AUTH_TOKEN")
			}
			if device != "" {
				cfg.DeviceName = device
			}
			self, err := os.Executable()
			if err == nil {
				cfg.BinaryPath = self
			}
			if err := config.Save(dataDir, cfg); err != nil {
				return err
			}

			if !yes {
				fmt.Printf("Will inject managed report hooks into %s. Continue? [y/N] ", claudeSettingsPath())
				var resp string
				_, _ = fmt.Scanln(&resp)
				if resp != "y" && resp != "Y" {
					fmt.Println("aborted; config.toml was still written")
					return nil
				}
			}

			if err := installClaudeHook(cfg.BinaryPath); err != nil {
				return err
			}
			fmt.Println("installed report hooks. Start the monitor with `agent-beacon monitor`;")
			fmt.Println("running Claude sessions will appear on the dashboard as observed cards.")
			fmt.Println("config:", config.Path(dataDir))
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "dashboard URL")
	cmd.Flags().StringVar(&token, "token", "", "agent PSK (or $AGENT_BEACON_AUTH_TOKEN)")
	cmd.Flags().StringVar(&device, "device", "", "persisted device name")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the consent prompt")
	return cmd
}

func uninstallCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the managed Claude Code hook",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := removeClaudeHook(); err != nil {
				return err
			}
			fmt.Println("removed managed hooks.")
			if purge {
				if dataDir, err := config.DataDir(); err == nil {
					_ = os.RemoveAll(dataDir)
					fmt.Println("purged data dir:", dataDir)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge-data", false, "also delete the data directory")
	return cmd
}

func claudeSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// installClaudeHook adds managed report hooks so each Claude session enriches
// its observed card on the dashboard. Rather than wrapping `claude`, the
// observe-first model injects lightweight hooks that POST session metadata to
// the local monitor's loopback report listener. It preserves existing settings
// and is idempotent: re-running replaces only our managed hook entries, which
// are tagged with managedMarker so uninstall can remove exactly them.
func installClaudeHook(binaryPath string) error {
	path := claudeSettingsPath()
	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	// event -> report --event value. SessionStart marks a session live, Stop
	// flips it idle promptly, UserPromptSubmit refreshes it as a heartbeat (which
	// also clears any pending "waiting" alert), and Notification fires exactly
	// when Claude blocks on the user (permission prompt / question) — we map it to
	// a "waiting" state so the dashboard can raise an intervention alert.
	events := map[string]string{
		"SessionStart":     "SessionStart",
		"Stop":             "Stop",
		"UserPromptSubmit": "heartbeat",
		"Notification":     "Notification",
	}
	for event, reportEvent := range events {
		// The Notification hook additionally sets state=waiting so the monitor can
		// flip the card to the intervention-needed state.
		extra := ""
		if reportEvent == "Notification" {
			extra = " --state waiting"
		}
		cmdStr := fmt.Sprintf("%s report --event %s%s", binaryPath, reportEvent, extra)
		entry := map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": cmdStr,
					"marker":  managedMarker,
				},
			},
		}
		hooks[event] = mergeManagedHook(hooks[event], entry)
	}
	settings["hooks"] = hooks

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// mergeManagedHook appends our managed entry to an event's existing hook list,
// first stripping any prior managed entry so re-installs don't accumulate.
func mergeManagedHook(existing any, managed map[string]any) []any {
	var out []any
	if list, ok := existing.([]any); ok {
		for _, e := range list {
			if !isManagedEntry(e) {
				out = append(out, e)
			}
		}
	}
	return append(out, managed)
}

// isManagedEntry reports whether a settings hook entry was injected by us,
// identified by the managedMarker on any of its inner command hooks.
func isManagedEntry(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if ok && hm["marker"] == managedMarker {
			return true
		}
	}
	return false
}

func removeClaudeHook() error {
	path := claudeSettingsPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	settings := map[string]any{}
	if err := json.Unmarshal(b, &settings); err != nil {
		return err
	}
	if hooks, ok := settings["hooks"].(map[string]any); ok {
		// Legacy: earlier versions stored a single managed hook under the marker
		// key directly; drop it if present.
		delete(hooks, managedMarker)
		// Strip our managed entries from every event's hook list, removing the
		// event entirely if nothing else remains.
		for event, v := range hooks {
			list, ok := v.([]any)
			if !ok {
				continue
			}
			var kept []any
			for _, e := range list {
				if !isManagedEntry(e) {
					kept = append(kept, e)
				}
			}
			if len(kept) == 0 {
				delete(hooks, event)
			} else {
				hooks[event] = kept
			}
		}
		settings["hooks"] = hooks
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}
