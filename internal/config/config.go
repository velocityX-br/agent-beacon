// Package config handles the agent-side data directory: config.toml, the PSK
// token file, and resolution of settings from flags, environment, and file.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the persisted agent configuration (config.toml).
type Config struct {
	URL        string `toml:"url"`         // dashboard base URL
	TokenPath  string `toml:"token_path"`  // path to the PSK token file
	BinaryPath string `toml:"binary_path"` // absolute path of this binary
	DeviceName string `toml:"device_name"` // renameable device name
	ProjectsDir string `toml:"projects_dir"` // path-list of spawnable roots

	// Orchestrator settings (optional; all overridable by flags/env).
	AnthropicKey  string `toml:"anthropic_key"`  // Anthropic API key for the verifier
	PlannerModel  string `toml:"planner_model"`  // default planner model id
	WorkerModel   string `toml:"worker_model"`   // default coding-agent model id
	VerifierModel string `toml:"verifier_model"` // default cross-model verifier id
}

// DataDir returns the agent data directory, honoring AGENT_BEACON_DATA_DIR,
// else ~/.config/agent-beacon.
func DataDir() (string, error) {
	if d := os.Getenv("AGENT_BEACON_DATA_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "agent-beacon"), nil
}

// Path returns the config.toml path within the data dir.
func Path(dataDir string) string { return filepath.Join(dataDir, "config.toml") }

// Load reads config.toml from dataDir. A missing file returns a zero Config
// and no error, so callers can layer env/flags on top.
func Load(dataDir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(Path(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, err
	}
	if err := toml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, nil
}

// Save writes config.toml, creating the data dir if needed.
func Save(dataDir string, c Config) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(Path(dataDir), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(c)
}

// WriteToken writes the PSK to <dataDir>/token with 0600 perms and returns its path.
func WriteToken(dataDir, token string) (string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dataDir, "token")
	if err := os.WriteFile(p, []byte(token), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// ReadToken reads a token file, trimming whitespace.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ResolveDeviceName applies the precedence chain, first non-empty wins:
// flag -> env(AGENT_BEACON_DEVICE_NAME) -> config.toml -> hostname -> "unknown".
// pinned is true when the value came from the flag or env (immune to remote rename).
func ResolveDeviceName(flagVal string, cfg Config) (name string, pinned bool) {
	if flagVal != "" {
		return flagVal, true
	}
	if env := os.Getenv("AGENT_BEACON_DEVICE_NAME"); env != "" {
		return env, true
	}
	if cfg.DeviceName != "" {
		return cfg.DeviceName, false
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h, false
	}
	return "unknown", false
}

// ProjectRoots splits a projects-dir path list using the OS-appropriate
// separator (':' on Unix, ';' on Windows), dropping empties.
func ProjectRoots(list string) []string {
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	var out []string
	for _, p := range strings.Split(list, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
