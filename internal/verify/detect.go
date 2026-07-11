// Package verify provides the deterministic gate layer for the orchestrator:
// language detection from marker files and execution of build/test/e2e
// commands. These gates must pass before a cross-model verifier is consulted,
// so the verifier judges goal-completion rather than compilation.
package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Language describes a detected language/toolchain and its default gate command
// set. Commands are argv slices (no shell) run with the repo as the working dir.
type Language struct {
	Name  string     // "go", "node", "python", "rust"
	Build []string   // build command argv (may be nil)
	Test  []string   // unit-test command argv (may be nil)
	Extra [][]string // additional deterministic checks (e.g. go vet)
}

// Detect scans repoDir for well-known marker files and returns the matching
// languages with their default gate commands. An empty result means no marker
// was recognized; callers should fall back to explicit --build-cmd/--test-cmd.
func Detect(repoDir string) []Language {
	var langs []Language

	if exists(repoDir, "go.mod") {
		langs = append(langs, Language{
			Name:  "go",
			Build: []string{"go", "build", "./..."},
			Test:  []string{"go", "test", "./..."},
			Extra: [][]string{{"go", "vet", "./..."}},
		})
	}
	if exists(repoDir, "package.json") {
		l := Language{
			Name: "node",
			Test: []string{"npm", "test"},
		}
		// Only add a build gate when the package defines a build script.
		if hasNpmScript(filepath.Join(repoDir, "package.json"), "build") {
			l.Build = []string{"npm", "run", "build"}
		}
		langs = append(langs, l)
	}
	if exists(repoDir, "pyproject.toml") || exists(repoDir, "requirements.txt") {
		langs = append(langs, Language{
			Name: "python",
			Test: []string{"pytest"},
		})
	}
	if exists(repoDir, "Cargo.toml") {
		langs = append(langs, Language{
			Name:  "rust",
			Build: []string{"cargo", "build"},
			Test:  []string{"cargo", "test"},
		})
	}
	return langs
}

// exists reports whether name is present directly under dir.
func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// hasNpmScript reads package.json and reports whether scripts[name] is defined.
// Any read/parse error is treated as "no script" — the caller then omits the
// build gate rather than failing detection.
func hasNpmScript(pkgPath, name string) bool {
	b, err := os.ReadFile(pkgPath)
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return false
	}
	_, ok := pkg.Scripts[name]
	return ok
}
