package verify

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectGo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/x\n\ngo 1.25\n")

	langs := Detect(dir)
	if len(langs) != 1 || langs[0].Name != "go" {
		t.Fatalf("expected one go language, got %+v", langs)
	}
	l := langs[0]
	if got := cmdString(l.Build); got != "go build ./..." {
		t.Errorf("go build cmd = %q", got)
	}
	if got := cmdString(l.Test); got != "go test ./..." {
		t.Errorf("go test cmd = %q", got)
	}
	if len(l.Extra) != 1 || cmdString(l.Extra[0]) != "go vet ./..." {
		t.Errorf("go vet extra = %+v", l.Extra)
	}
}

func TestDetectNodeBuildScriptGating(t *testing.T) {
	// package.json without a build script -> no build gate.
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"jest"}}`)
	langs := Detect(dir)
	if len(langs) != 1 || langs[0].Name != "node" {
		t.Fatalf("expected node, got %+v", langs)
	}
	if len(langs[0].Build) != 0 {
		t.Errorf("expected no build gate without build script, got %v", langs[0].Build)
	}
	if cmdString(langs[0].Test) != "npm test" {
		t.Errorf("node test cmd = %q", cmdString(langs[0].Test))
	}

	// With a build script -> build gate present.
	dir2 := t.TempDir()
	writeFile(t, dir2, "package.json", `{"name":"x","scripts":{"build":"tsc","test":"jest"}}`)
	langs2 := Detect(dir2)
	if len(langs2) != 1 || cmdString(langs2[0].Build) != "npm run build" {
		t.Errorf("expected npm run build gate, got %+v", langs2)
	}
}

func TestDetectPythonAndRust(t *testing.T) {
	py := t.TempDir()
	writeFile(t, py, "pyproject.toml", "[project]\nname='x'\n")
	if langs := Detect(py); len(langs) != 1 || langs[0].Name != "python" || cmdString(langs[0].Test) != "pytest" {
		t.Errorf("python detect = %+v", langs)
	}

	rs := t.TempDir()
	writeFile(t, rs, "Cargo.toml", "[package]\nname='x'\n")
	if langs := Detect(rs); len(langs) != 1 || langs[0].Name != "rust" || cmdString(langs[0].Build) != "cargo build" {
		t.Errorf("rust detect = %+v", langs)
	}
}

func TestDetectEmpty(t *testing.T) {
	if langs := Detect(t.TempDir()); len(langs) != 0 {
		t.Errorf("expected no languages for empty dir, got %+v", langs)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cmdString(argv []string) string {
	out := ""
	for i, a := range argv {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
