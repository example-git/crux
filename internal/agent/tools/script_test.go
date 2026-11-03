package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestScriptArgumentsUsesOnlyConfiguredVariables(t *testing.T) {
	preset := "fixed"
	defaultValue := "json"
	script := config.AgentScript{Variables: map[string]config.AgentScriptVariable{
		"preset": {Flag: "--preset", Value: &preset},
		"format": {Flag: "--format", Default: &defaultValue, Values: []string{"json", "text"}},
		"input":  {Flag: "--input", Required: true},
	}}

	args, err := scriptArguments(script, map[string]string{"input": "sample.txt", "format": "text"})
	require.NoError(t, err)
	require.Equal(t, []string{"--format", "text", "--input", "sample.txt", "--preset", "fixed"}, args)

	_, err = scriptArguments(script, map[string]string{})
	require.ErrorContains(t, err, `script variable "input" is required`)
	_, err = scriptArguments(script, map[string]string{"input": "x", "unknown": "y"})
	require.ErrorContains(t, err, `unknown script variable "unknown"`)
	_, err = scriptArguments(script, map[string]string{"input": "x", "preset": "override"})
	require.ErrorContains(t, err, "preset and cannot be overridden")
	_, err = scriptArguments(script, map[string]string{"input": "x", "format": "yaml"})
	require.ErrorContains(t, err, "must be one of: json, text")
}

func TestScriptToolExecutesFixedPythonScript(t *testing.T) {
	if _, err := pythonInterpreter(); err != nil {
		t.Skip(err)
	}
	workingDir := t.TempDir()
	scriptPath := filepath.Join(t.TempDir(), "script.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte("import json, os, sys\nprint(json.dumps({'args': sys.argv[1:], 'cwd': os.getcwd(), 'env': os.environ.get('CRUX_T4_4_4_SCRIPT_ENV')}))\n"), 0o600))
	preset := "fixed"
	script := config.AgentScript{
		Path:    scriptPath,
		Timeout: 30 * time.Second,
		Variables: map[string]config.AgentScriptVariable{
			"input":  {Flag: "--input", Required: true},
			"preset": {Flag: "--preset", Value: &preset},
		},
	}
	permissions := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: true}
	tool := NewScriptTool(permissions, workingDir, script, []string{"CRUX_T4_4_4_SCRIPT_ENV=workspace"})
	response := runScriptTool(t, tool, ScriptParams{Variables: map[string]string{"input": "sample.txt"}})

	require.False(t, response.IsError)
	require.Equal(t, 1, permissions.requestCount)
	var output struct {
		Args []string `json:"args"`
		CWD  string   `json:"cwd"`
		Env  string   `json:"env"`
	}
	require.NoError(t, json.Unmarshal([]byte(response.Content), &output))
	require.Equal(t, []string{"--input", "sample.txt", "--preset", "fixed"}, output.Args)
	require.Equal(t, "workspace", output.Env)
	expectedDirectory, err := os.Stat(workingDir)
	require.NoError(t, err)
	actualDirectory, err := os.Stat(output.CWD)
	require.NoError(t, err)
	require.True(t, os.SameFile(expectedDirectory, actualDirectory))
}

func TestScriptToolDoesNotExecuteWhenPermissionDenied(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "script.py")
	outputPath := filepath.Join(t.TempDir(), "ran")
	require.NoError(t, os.WriteFile(scriptPath, []byte("from pathlib import Path\nPath("+strconvQuote(outputPath)+").write_text('ran')\n"), 0o600))
	permissions := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: false}
	tool := NewScriptTool(permissions, t.TempDir(), config.AgentScript{Path: scriptPath, Timeout: time.Second})
	response := runScriptTool(t, tool, ScriptParams{})

	require.True(t, response.IsError)
	require.True(t, response.StopTurn)
	require.NoFileExists(t, outputPath)
}

func TestScriptToolTimeoutAndOutputLimit(t *testing.T) {
	if _, err := pythonInterpreter(); err != nil {
		t.Skip(err)
	}
	permissions := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: true}
	t.Run("timeout", func(t *testing.T) {
		scriptPath := filepath.Join(t.TempDir(), "script.py")
		require.NoError(t, os.WriteFile(scriptPath, []byte("import time\ntime.sleep(5)\n"), 0o600))
		tool := NewScriptTool(permissions, t.TempDir(), config.AgentScript{Path: scriptPath, Timeout: 20 * time.Millisecond})
		response := runScriptTool(t, tool, ScriptParams{})
		require.True(t, response.IsError)
		require.Contains(t, response.Content, "timed out")
	})

	t.Run("output", func(t *testing.T) {
		scriptPath := filepath.Join(t.TempDir(), "script.py")
		require.NoError(t, os.WriteFile(scriptPath, []byte("print('x' * 100000)\n"), 0o600))
		tool := NewScriptTool(permissions, t.TempDir(), config.AgentScript{Path: scriptPath, Timeout: time.Second})
		response := runScriptTool(t, tool, ScriptParams{})
		require.False(t, response.IsError)
		require.Less(t, len(response.Content), scriptOutputLimit+100)
		require.Contains(t, response.Content, "output truncated")
	})
}

func TestLimitedScriptBufferBoundsCapture(t *testing.T) {
	buffer := newLimitedScriptBuffer(4)
	written, err := buffer.Write([]byte("abcdefgh"))
	require.NoError(t, err)
	require.Equal(t, 8, written)
	require.Equal(t, "abcd\n... output truncated", buffer.String())
}

func TestScriptToolRejectsIdentityStateBeforeLaunch(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	_, err = store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "session"}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, pathtranslate.Settings{}, root)
	require.NoError(t, err)
	scriptPath := filepath.Join(root, "reader.py")
	marker := filepath.Join(root, "executed")
	require.NoError(t, os.WriteFile(scriptPath, []byte("from pathlib import Path\nPath("+strconvQuote(marker)+").write_text('ran')\n"), 0o600))
	permissions := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: true}
	script := config.AgentScript{Path: scriptPath, Timeout: time.Second, Variables: map[string]config.AgentScriptVariable{"input": {Flag: "--input", Required: true}}}
	tool := NewScriptTool(permissions, root, script)
	input, err := json.Marshal(ScriptParams{Variables: map[string]string{"input": filepath.Join(state, "crux.db")}})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session")
	ctx = pathtranslate.WithTranslator(ctx, translator)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "script", Name: ScriptToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, pathtranslate.ErrInternalStateAccess.Error(), response.Content)
	require.Zero(t, permissions.requestCount)
	require.NoFileExists(t, marker)
}

func runScriptTool(t *testing.T, tool fantasy.AgentTool, params ScriptParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session")
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: ScriptToolName, Input: string(input)})
	require.NoError(t, err)
	return response
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return strings.ReplaceAll(string(encoded), "\\/", "/")
}

func TestScriptToolCarriesProtectedOutputPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	protected := filepath.Join(root, "private")
	require.NoError(t, os.Mkdir(protected, 0o700))
	file := filepath.Join(protected, "confidential name.txt")
	require.NoError(t, os.WriteFile(file, []byte("original"), 0o600))
	state := t.TempDir()
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "session"}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected, Recursive: true}}}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, policy.Settings, root)
	require.NoError(t, err)
	scriptPath := filepath.Join(root, "listing.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte("import os, sys\nprint(os.listdir(sys.argv[2])[0])\n"), 0o600))
	permissions := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: true}
	tool := NewScriptTool(permissions, root, config.AgentScript{Path: scriptPath, Timeout: time.Second, Variables: map[string]config.AgentScriptVariable{"input": {Flag: "--input", Required: true}}})
	input, err := json.Marshal(ScriptParams{Variables: map[string]string{"input": protected}})
	require.NoError(t, err)
	ctx := pathtranslate.WithTranslator(context.WithValue(t.Context(), SessionIDContextKey, "session"), translator)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "script", Name: ScriptToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, response.IsError, response.Content)
	require.Contains(t, response.Content, "confidential name.txt")
	translated, err := translator.ReferencedText(ctx, response.Content, response.Metadata)
	require.NoError(t, err)
	require.NotContains(t, translated, "confidential")
	resolved, err := translator.ResolvePath(ctx, strings.TrimSpace(translated))
	require.NoError(t, err)
	require.Equal(t, file, resolved)
}
