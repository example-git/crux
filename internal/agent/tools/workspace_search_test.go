package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSearchToolExposesUnifiedSchema(t *testing.T) {
	tool := NewSearchTool(nil, t.TempDir(), config.ToolSearch{})
	info := tool.Info()

	require.Equal(t, SearchToolName, info.Name)
	require.NotEmpty(t, info.Description)
	require.ElementsMatch(t, []string{"mode", "pattern"}, info.Required)
	encoded, err := json.Marshal(info.Parameters["mode"])
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"string","enum":["files","content"],"description":"Search mode: files matches file paths by glob pattern; content searches inside files by regex or literal text"}`, string(encoded))
}

func TestSearchToolFindsFiles(t *testing.T) {
	workingDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workingDir, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "pkg", "match.go"), []byte("package pkg"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "pkg", "skip.txt"), []byte("skip"), 0o644))

	response := runWorkspaceSearchTool(t, NewSearchTool(nil, workingDir, config.ToolSearch{}), t.Context(), SearchParams{
		Mode:    SearchModeFiles,
		Pattern: "**/*.go",
	})

	require.False(t, response.IsError)
	require.Contains(t, response.Content, filepath.ToSlash(filepath.Join("pkg", "match.go")))
	require.NotContains(t, response.Content, "skip.txt")
	var metadata SearchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.Equal(t, SearchModeFiles, metadata.Mode)
	require.Equal(t, 1, metadata.NumberOfFiles)
	require.Zero(t, metadata.NumberOfMatches)
}

func TestSearchToolFindsLiteralContentWithInclude(t *testing.T) {
	workingDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "match.go"), []byte("const value = `[one]`\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "skip.txt"), []byte("[one]\n"), 0o644))

	response := runWorkspaceSearchTool(t, NewSearchTool(nil, workingDir, config.ToolSearch{}), t.Context(), SearchParams{
		Mode:        SearchModeContent,
		Pattern:     "[one]",
		Include:     "*.go",
		LiteralText: true,
	})

	require.False(t, response.IsError)
	require.Contains(t, response.Content, "Found 1 matches")
	require.Contains(t, response.Content, "match.go")
	require.NotContains(t, response.Content, "skip.txt")
	var metadata SearchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.Equal(t, SearchModeContent, metadata.Mode)
	require.Zero(t, metadata.NumberOfFiles)
	require.Equal(t, 1, metadata.NumberOfMatches)
}

func TestSearchToolRejectsInvalidModeCombinations(t *testing.T) {
	tool := NewSearchTool(nil, t.TempDir(), config.ToolSearch{})
	tests := []struct {
		name   string
		params SearchParams
		want   string
	}{
		{name: "missing mode", params: SearchParams{Pattern: "*"}, want: `mode must be "files" or "content"`},
		{name: "invalid mode", params: SearchParams{Mode: "paths", Pattern: "*"}, want: `mode must be "files" or "content"`},
		{name: "missing pattern", params: SearchParams{Mode: SearchModeFiles}, want: "pattern is required"},
		{name: "include in files mode", params: SearchParams{Mode: SearchModeFiles, Pattern: "*", Include: "*.go"}, want: "include is only valid in content mode"},
		{name: "literal in files mode", params: SearchParams{Mode: SearchModeFiles, Pattern: "*", LiteralText: true}, want: "literal_text is only valid in content mode"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runWorkspaceSearchTool(t, tool, t.Context(), test.params)
			require.True(t, response.IsError)
			require.Equal(t, test.want, response.Content)
		})
	}
}

func TestSearchToolUsesModeSpecificExternalPermissions(t *testing.T) {
	workingDir := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(workingDir, 0o755))
	outsideDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "match.txt"), []byte("needle\n"), 0o644))
	canonicalOutsideDir, err := canonicalToolPath(workingDir, outsideDir)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session-1")

	tests := []struct {
		mode    string
		pattern string
		action  string
	}{
		{mode: SearchModeFiles, pattern: "*.txt", action: "list"},
		{mode: SearchModeContent, pattern: "needle", action: "read"},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			permissions := &recordingPermissionService{allow: true}
			response := runWorkspaceSearchTool(t, NewSearchTool(permissions, workingDir, config.ToolSearch{}), ctx, SearchParams{
				Mode:    test.mode,
				Pattern: test.pattern,
				Path:    outsideDir,
			})

			require.False(t, response.IsError)
			require.Equal(t, 1, permissions.requestCount)
			require.Equal(t, SearchToolName, permissions.lastRequest.ToolName)
			require.Equal(t, test.action, permissions.lastRequest.Action)
			require.Equal(t, canonicalOutsideDir, permissions.lastRequest.Path)
		})
	}
}

func runWorkspaceSearchTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params SearchParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "search-call", Name: SearchToolName, Input: string(input)})
	require.NoError(t, err)
	return response
}
