package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestPathTranslationShellOutputAndRestart(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	protected := filepath.Join(root, "private-shell")
	require.NoError(t, os.Mkdir(protected, 0o700))
	for index := range 400 {
		name := fmt.Sprintf("confidential %03d %s.txt", index, strings.Repeat("long filename ", 8))
		require.NoError(t, os.WriteFile(filepath.Join(protected, name), []byte("real original content"), 0o600))
	}
	state := t.TempDir()
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "shell"}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected, Recursive: true}}}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, policy.Settings, root)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, "shell")
	ctx = pathtranslate.WithTranslator(ctx, translator)
	permissions := permission.NewPermissionService(root, true, nil)
	events := permissions.Subscribe(ctx)
	var deny atomic.Bool
	var requests atomic.Int32
	go func() {
		for event := range events {
			requests.Add(1)
			if deny.Load() {
				permissions.Deny(event.Payload)
			} else {
				permissions.Grant(event.Payload)
			}
		}
	}()
	outputRoot, recordRoot := filepath.Join(state, "output"), filepath.Join(state, "records")
	outputStore, err := managedtask.NewOutputStore(outputRoot, managedtask.OutputStoreOptions{})
	require.NoError(t, err)
	recordStore, err := managedtask.NewStore(recordRoot)
	require.NoError(t, err)
	manager, err := shell.NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	t.Cleanup(func() { manager.KillAll(context.Background()); _ = recordStore.Close() })
	coord := &coordinator{backgroundShells: manager}
	bash := translatedTool{AgentTool: codexBoundedTools([]fantasy.AgentTool{tools.NewBashTool(manager, permissions, root)}, Model{InstructionPolicy: fantasy.InstructionPolicyCodex, ModelCfg: config.SelectedModel{Model: "gpt-5.2"}})[0]}
	alias, err := translator.Path(ctx, protected)
	require.NoError(t, err)
	input, err := json.Marshal(tools.BashParams{Command: "ls -1 " + alias})
	require.NoError(t, err)
	call := fantasy.ToolCall{ID: "list", Name: "bash", Input: string(input)}
	response, err := bash.Run(ctx, call)
	require.NoError(t, err)
	require.False(t, response.IsError, response.Content)
	require.LessOrEqual(t, len(response.Content), 12000)
	translate := func(call fantasy.ToolCall, response fantasy.ToolResponse) string {
		t.Helper()
		prompt, err := translator.Prompt(ctx, fantasy.Prompt{
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: call.ID, ToolName: call.Name, Input: call.Input}}},
			{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: call.ID, ClientMetadata: response.Metadata, Output: fantasy.ToolResultOutputContentText{Text: response.Content}}}},
		})
		require.NoError(t, err)
		return prompt[1].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText).Text
	}
	translated := translate(call, response)
	require.NotContains(t, translated, "confidential")
	require.NotContains(t, translated, "long filename")
	aliases := regexp.MustCompile(`pt_[a-f0-9]{64}`).FindAllString(translated, -1)
	require.NotEmpty(t, aliases)
	firstFile, err := translator.ResolvePath(ctx, aliases[0])
	require.NoError(t, err)
	original, err := os.ReadFile(firstFile)
	require.NoError(t, err)
	require.Equal(t, "real original content", string(original))
	input, err = json.Marshal(tools.BashParams{Command: "ls -1 " + alias + "; sleep 2", RunInBackground: true})
	require.NoError(t, err)
	call.Input, call.ID = string(input), "background"
	response, err = bash.Run(ctx, call)
	require.NoError(t, err)
	var metadata tools.BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.NotEmpty(t, metadata.ShellID)
	background, ok := manager.Get(metadata.ShellID)
	require.True(t, ok)
	background.Wait()
	readTool := codexBoundedTools([]fantasy.AgentTool{tools.NewTaskOutputTool(coord)}, Model{InstructionPolicy: fantasy.InstructionPolicyCodex, ModelCfg: config.SelectedModel{Model: "gpt-5.2"}})[0]
	readInput, err := json.Marshal(tools.TaskOutputParams{TaskID: background.ID})
	require.NoError(t, err)
	readCall := fantasy.ToolCall{ID: "output", Name: tools.TaskOutputToolName, Input: string(readInput)}
	response, err = readTool.Run(ctx, readCall)
	require.NoError(t, err)
	translated = translate(readCall, response)
	require.NotContains(t, translated, "confidential")
	require.NotContains(t, translated, "long filename")
	require.Contains(t, translated, pathtranslate.AliasPrefix)
	before := requests.Load()
	deny.Store(true)
	restartInput, err := json.Marshal(tools.TaskRestartParams{TaskID: background.ID})
	require.NoError(t, err)
	response, err = tools.NewTaskRestartTool(coord, permissions).Run(ctx, fantasy.ToolCall{ID: "restart", Name: tools.TaskRestartToolName, Input: string(restartInput)})
	require.NoError(t, err)
	require.False(t, response.IsError, response.Content)
	restarted, ok := manager.Get(background.ID)
	require.True(t, ok)
	restarted.Wait()
	require.Greater(t, requests.Load(), before)
	require.Equal(t, managedtask.StatusFailed, restarted.State().Status)
	stdout, _, _, _ := restarted.GetOutput()
	require.NotContains(t, stdout, "confidential")
	record, err := recordStore.Get(background.ID)
	require.NoError(t, err)
	require.Contains(t, record.Shell.OutputPaths, protected)
	recovered, err := shell.NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	recoveredShell, ok := recovered.Get(background.ID)
	require.True(t, ok)
	require.Contains(t, recoveredShell.OutputPaths(), protected)
}
