package main

import (
	"reflect"
	"testing"
)

func TestMCPServersFromConfig(t *testing.T) {
	claudeJSON := []byte(`{
		"mcpServers": {"sap-jira": {"command": "node"}, "github-tools": {"command": "node"}},
		"projects": {
			"/work/repo": {"mcpServers": {"repo-only": {"command": "x"}}},
			"/other": {"mcpServers": {"elsewhere": {"command": "y"}}}
		}
	}`)
	projectMCP := []byte(`{"mcpServers": {"shared-svc": {"command": "z"}, "sap-jira": {"command": "dup"}}}`)

	got := mcpServersFromConfig(claudeJSON, projectMCP, "/work/repo")
	// Global (sorted) -> project-scoped -> shared, de-duplicated. "sap-jira" in
	// .mcp.json is a duplicate of the global entry and must not repeat.
	want := []string{"github-tools", "sap-jira", "repo-only", "shared-svc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mcpServersFromConfig() = %v, want %v", got, want)
	}
}

func TestMCPServersFromConfigGlobalOnly(t *testing.T) {
	claudeJSON := []byte(`{"mcpServers": {"only": {"command": "node"}}}`)
	got := mcpServersFromConfig(claudeJSON, nil, "/no/project/here")
	want := []string{"only"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mcpServersFromConfig() = %v, want %v", got, want)
	}
}

func TestMCPServersFromConfigEmptyAndMalformed(t *testing.T) {
	if got := mcpServersFromConfig(nil, nil, "/x"); got != nil {
		t.Fatalf("empty config = %v, want nil", got)
	}
	if got := mcpServersFromConfig([]byte("not json"), []byte("also not json"), "/x"); got != nil {
		t.Fatalf("malformed config = %v, want nil", got)
	}
}
