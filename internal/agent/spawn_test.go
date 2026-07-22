package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSpawnTargetWithinRoot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSpawnTarget([]string{root}, repo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	real, _ := filepath.EvalSymlinks(repo)
	if got != real {
		t.Fatalf("resolved %q, want %q", got, real)
	}
}

func TestResolveSpawnTargetRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a sibling temp dir NOT under root
	if _, err := resolveSpawnTarget([]string{root}, outside); err == nil {
		t.Fatal("expected rejection for path outside allowed roots")
	}
}

func TestResolveSpawnTargetRejectsMissing(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveSpawnTarget([]string{root}, filepath.Join(root, "nope")); err == nil {
		t.Fatal("expected rejection for nonexistent path")
	}
}

func TestResolveSpawnTargetNoRoots(t *testing.T) {
	dir := t.TempDir()
	if _, err := resolveSpawnTarget(nil, dir); err == nil {
		t.Fatal("expected rejection when no roots configured")
	}
}

// TestResolveTrustedCwdAccepts confirms a real dir passes with no roots (a
// clone's cwd is server-authoritative, so it need not be under any root).
func TestResolveTrustedCwdAccepts(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveTrustedCwd(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if got != real {
		t.Fatalf("resolved %q, want %q", got, real)
	}
}

// TestResolveTrustedCwdRejectsMissing confirms a vanished/bad path fails.
func TestResolveTrustedCwdRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := resolveTrustedCwd(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("expected rejection for nonexistent trusted cwd")
	}
	if _, err := resolveTrustedCwd(""); err == nil {
		t.Fatal("expected rejection for empty trusted cwd")
	}
}

func TestSanitizeBranch(t *testing.T) {
	cases := map[string]bool{
		"feature/foo":  true,
		"fix-123":      true,
		"main":         true,
		"a.b_c":        true,
		"":             false,
		"has space":    false,
		"../evil":      false,
		"-flaglike":    false,
		"semi;colon":   false,
		"back`tick":    false,
	}
	for in, wantOK := range cases {
		got := sanitizeBranch(in)
		if wantOK && got == "" {
			t.Errorf("sanitizeBranch(%q) rejected, want accepted", in)
		}
		if !wantOK && got != "" {
			t.Errorf("sanitizeBranch(%q) = %q, want rejected", in, got)
		}
	}
}
