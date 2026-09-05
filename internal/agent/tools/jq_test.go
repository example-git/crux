package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/stretchr/testify/require"
)

func TestJQToolSupportsShellModes(t *testing.T) {
	tool := NewJQTool(nil, t.TempDir())
	require.Equal(t, JQToolName, tool.Info().Name)
	require.NotEmpty(t, tool.Info().Description)

	tests := []struct {
		name   string
		params JQParams
		want   string
	}{
		{
			name:   "default filter",
			params: JQParams{Input: `{"name":"crux"}`},
			want:   "{\n  \"name\": \"crux\"\n}\n",
		},
		{
			name:   "raw output",
			params: JQParams{Filter: ".name", Input: `{"name":"crux"}`, RawOutput: true},
			want:   "crux\n",
		},
		{
			name:   "joined output",
			params: JQParams{Filter: ".[]", Input: `["a","b"]`, JoinOutput: true},
			want:   "ab",
		},
		{
			name:   "compact output",
			params: JQParams{Input: `{"a":1}`, CompactOutput: true},
			want:   "{\"a\":1}\n",
		},
		{
			name:   "slurped stream",
			params: JQParams{Filter: "map(.a)", Input: `{"a":1}{"a":2}`, Slurp: true, CompactOutput: true},
			want:   "[1,2]\n",
		},
		{
			name: "null input with variables",
			params: JQParams{
				Filter:        "{host: $host, port: $port}",
				NullInput:     true,
				CompactOutput: true,
				Args:          map[string]string{"host": "localhost"},
				JSONArgs:      map[string]any{"port": float64(8080)},
			},
			want: "{\"host\":\"localhost\",\"port\":8080}\n",
		},
		{
			name:   "raw input",
			params: JQParams{Filter: "ascii_upcase", Input: "one\ntwo", RawInput: true, RawOutput: true},
			want:   "ONE\nTWO\n",
		},
		{
			name:   "successful exit status",
			params: JQParams{Input: "true", ExitStatus: true, CompactOutput: true},
			want:   "true\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runJQTool(t, tool, t.Context(), test.params)
			require.False(t, response.IsError)
			require.Equal(t, test.want, response.Content)
		})
	}
}

func TestJQToolUsesExplicitEnvironment(t *testing.T) {
	const key = "CRUX_JQ_WORKSPACE_ENV"
	t.Setenv(key, "process")
	tool := NewJQTool(nil, t.TempDir(), []string{key + "=workspace"})

	response := runJQTool(t, tool, t.Context(), JQParams{
		Filter:    "env." + key,
		NullInput: true,
		RawOutput: true,
	})
	require.False(t, response.IsError)
	require.Equal(t, "workspace\n", response.Content)
	require.Equal(t, "process", os.Getenv(key))
}

func TestJQToolReadsWorkspaceFiles(t *testing.T) {
	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "input.json")
	require.NoError(t, os.WriteFile(filePath, []byte(`{"name":"crux"}`), 0o644))

	response := runJQTool(t, NewJQTool(nil, workingDir), t.Context(), JQParams{
		Filter:    ".name",
		Files:     []string{"input.json"},
		RawOutput: true,
	})
	require.False(t, response.IsError)
	require.Equal(t, "crux\n", response.Content)
}

func TestJQToolAuthorizesExternalFiles(t *testing.T) {
	workingDir := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(workingDir, 0o755))
	externalFile := filepath.Join(t.TempDir(), "input.json")
	require.NoError(t, os.WriteFile(externalFile, []byte(`{"name":"crux"}`), 0o644))

	canonicalExternalFile, err := canonicalToolPath(workingDir, externalFile)
	require.NoError(t, err)
	permissions := &recordingPermissionService{allow: true}
	tool := NewJQTool(permissions, workingDir)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session-1")
	response := runJQTool(t, tool, ctx, JQParams{Filter: ".name", Files: []string{externalFile}, RawOutput: true})

	require.False(t, response.IsError)
	require.Equal(t, "crux\n", response.Content)
	require.Equal(t, 1, permissions.requestCount)
	require.Equal(t, "session-1", permissions.lastRequest.SessionID)
	require.Equal(t, "jq-call", permissions.lastRequest.ToolCallID)
	require.Equal(t, JQToolName, permissions.lastRequest.ToolName)
	require.Equal(t, "read", permissions.lastRequest.Action)
	require.Equal(t, canonicalExternalFile, permissions.lastRequest.Path)
	require.Equal(t, JQPermissionsParams{FilePath: canonicalExternalFile, Filter: ".name"}, permissions.lastRequest.Params)
}

func TestJQToolStopsAfterExternalFilePermissionDenial(t *testing.T) {
	workingDir := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(workingDir, 0o755))
	externalFile := filepath.Join(t.TempDir(), "input.json")
	require.NoError(t, os.WriteFile(externalFile, []byte(`{"name":"crux"}`), 0o644))

	permissions := &recordingPermissionService{allow: false}
	tool := NewJQTool(permissions, workingDir)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session-1")
	response := runJQTool(t, tool, ctx, JQParams{Files: []string{externalFile}})

	require.True(t, response.IsError)
	require.True(t, response.StopTurn)
	require.Contains(t, response.Content, "User denied permission")
	require.Equal(t, 1, permissions.requestCount)
}

func TestJQToolRejectsInvalidInputCombinations(t *testing.T) {
	tool := NewJQTool(nil, t.TempDir())
	tests := []struct {
		name   string
		params JQParams
		want   string
	}{
		{
			name:   "input and files",
			params: JQParams{Input: "null", Files: []string{"input.json"}},
			want:   "input and files are mutually exclusive",
		},
		{
			name:   "null input and input",
			params: JQParams{NullInput: true, Input: "null"},
			want:   "null_input cannot be combined",
		},
		{
			name:   "empty file",
			params: JQParams{Files: []string{" "}},
			want:   "files must not contain an empty path",
		},
		{
			name:   "duplicate variable",
			params: JQParams{NullInput: true, Args: map[string]string{"value": "text"}, JSONArgs: map[string]any{"value": 1}},
			want:   `variable "value" is present in both args and json_args`,
		},
		{
			name:   "empty variable",
			params: JQParams{NullInput: true, Args: map[string]string{" ": "text"}},
			want:   "variable names must not be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runJQTool(t, tool, t.Context(), test.params)
			require.True(t, response.IsError)
			require.Contains(t, response.Content, test.want)
		})
	}
}

func TestJQToolReportsEvaluationFailuresAndPreservesOutput(t *testing.T) {
	tool := NewJQTool(nil, t.TempDir())

	parseFailure := runJQTool(t, tool, t.Context(), JQParams{Filter: "[", Input: "null"})
	require.True(t, parseFailure.IsError)
	require.Contains(t, parseFailure.Content, "jq:")

	runtimeFailure := runJQTool(t, tool, t.Context(), JQParams{Filter: ".foo", Input: "1"})
	require.True(t, runtimeFailure.IsError)
	require.Contains(t, runtimeFailure.Content, "jq:")

	joinedFailure := runJQTool(t, tool, t.Context(), JQParams{
		Filter:     `.[] | if . == 2 then error("boom") else tostring end`,
		Input:      "[1,2]",
		JoinOutput: true,
	})
	require.True(t, joinedFailure.IsError)
	require.True(t, strings.HasPrefix(joinedFailure.Content, "1\njq:"), joinedFailure.Content)

	exitFailure := runJQTool(t, tool, t.Context(), JQParams{Input: "false", ExitStatus: true})
	require.True(t, exitFailure.IsError)
	require.Contains(t, exitFailure.Content, "false")
	require.Contains(t, exitFailure.Content, "status 1")

	noOutput := runJQTool(t, tool, t.Context(), JQParams{Filter: "empty", Input: "null"})
	require.False(t, noOutput.IsError)
	require.Equal(t, "no output", noOutput.Content)
}

func TestJQToolBoundsOutputAndStopsEvaluation(t *testing.T) {
	tool := NewJQTool(nil, t.TempDir())
	response := runJQTool(t, tool, t.Context(), JQParams{
		Filter:        "range(1000000)",
		NullInput:     true,
		CompactOutput: true,
	})

	require.False(t, response.IsError)
	require.LessOrEqual(t, len(response.Content), maxJQToolOutput)
	require.Contains(t, response.Content, jqOutputTruncatedMark)
	var metadata JQResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.True(t, metadata.Truncated)
}

func TestJQOutputWriterKeepsTruncatedOutputValidUTF8(t *testing.T) {
	writer := &jqOutputWriter{remaining: maxJQToolOutput - len(jqOutputTruncatedMark)}
	_, err := writer.Write([]byte(strings.Repeat("界", maxJQToolOutput)))
	require.ErrorIs(t, err, errJQOutputLimit)
	require.True(t, utf8.ValidString(writer.String()))
	require.LessOrEqual(t, len(writer.String()), maxJQToolOutput)
}

func TestJQToolReturnsContextCancellation(t *testing.T) {
	tool := NewJQTool(nil, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	input, err := json.Marshal(JQParams{Filter: "range(1000000)", NullInput: true})
	require.NoError(t, err)
	_, err = tool.Run(ctx, fantasy.ToolCall{ID: "jq-call", Name: JQToolName, Input: string(input)})
	require.ErrorIs(t, err, context.Canceled)
}

func runJQTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params JQParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "jq-call", Name: JQToolName, Input: string(input)})
	require.NoError(t, err)
	return response
}
