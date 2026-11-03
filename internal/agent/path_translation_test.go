package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/imageutil"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/stretchr/testify/require"
)

type translationSequenceModel struct {
	finishStreamModel
	calls []fantasy.Call
	label string
}

func (m *translationSequenceModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls = append(m.calls, call)
	encoded, err := json.Marshal(call.Prompt)
	if err != nil {
		return nil, err
	}
	aliases := regexp.MustCompile(`pt_[a-f0-9]{64}`).FindAllString(string(encoded), -1)
	if len(aliases) == 0 && m.label == "" {
		return nil, fmt.Errorf("provider received no translated paths")
	}
	if len(m.calls) > 2 {
		return m.finishStreamModel.Stream(ctx, call)
	}
	toolName, field := "ls", "path"
	if len(m.calls) == 2 {
		toolName, field = "view", "file_path"
	}
	path := m.label
	if len(m.calls) != 1 || path == "" {
		path = aliases[len(aliases)-1]
	}
	input, err := json.Marshal(map[string]string{field: path})
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("translation-%d", len(m.calls))
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: toolName, ToolCallInput: string(input)}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	}, nil
}

func TestPathTranslationSessionListsAndReadsActualFile(t *testing.T) {
	for _, label := range []string{"", "My library 日本語"} {
		t.Run("label="+label, func(t *testing.T) {
			env := testEnv(t)
			workingDir, err := filepath.EvalSymlinks(env.workingDir)
			require.NoError(t, err)
			protected := filepath.Join(workingDir, "private-library")
			require.NoError(t, os.Mkdir(protected, 0o700))
			filePath := filepath.Join(protected, "confidential name.txt")
			require.NoError(t, os.WriteFile(filePath, []byte("contents from the actual original file"), 0o600))
			stateDir := t.TempDir()
			conn, err := db.Connect(t.Context(), stateDir)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
			store := pathtranslate.NewStore(conn)
			current, err := env.sessions.Create(t.Context(), "translation")
			require.NoError(t, err)
			policy, err := store.Save(t.Context(), pathtranslate.Policy{
				Scope:    pathtranslate.Scope{Kind: "session", SessionID: current.ID},
				Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected, Label: label, Recursive: true}}},
			})
			require.NoError(t, err)
			resolve := func(ctx context.Context) (*pathtranslate.Translator, error) {
				names, err := store.Names(ctx)
				if err != nil || names == nil {
					return nil, err
				}
				return pathtranslate.New(names, policy.Settings, workingDir)
			}
			provider := &translationSequenceModel{label: label, finishStreamModel: finishStreamModel{text: "read completed"}}
			model := translatedLanguageModel{LanguageModel: provider, resolve: resolve}
			listing := tools.NewLsTool(env.permissions, workingDir, config.ToolLs{})
			view := tools.NewViewTool(nil, env.permissions, *env.filetracker, nil, workingDir)
			agent := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "Read the requested files.", listing, view)
			runtime := agent.Runtime()
			runtime.LargeModel.PathTranslation = resolve
			agent.SetRuntime(runtime)
			_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "List " + protected + " and read the file."})
			require.NoError(t, err)
			require.Len(t, provider.calls, 3)
			for _, call := range provider.calls {
				payload, err := json.Marshal(call.Prompt)
				require.NoError(t, err)
				require.NotContains(t, string(payload), "private-library")
				require.NotContains(t, string(payload), "confidential name")
			}
			last, err := json.Marshal(provider.calls[2].Prompt)
			require.NoError(t, err)
			require.Contains(t, string(last), "contents from the actual original file")
			require.NoError(t, env.messages.FlushAll(t.Context()))
			stored, err := env.messages.List(t.Context(), current.ID)
			require.NoError(t, err)
			var readOriginal bool
			for _, msg := range stored {
				if msg.Role == message.Assistant {
					for _, call := range msg.ToolCalls() {
						if call.Name == "view" {
							readOriginal = strings.Contains(call.Input, filePath)
						}
					}
				}
			}
			require.True(t, readOriginal)
			original, err := os.ReadFile(filePath)
			require.NoError(t, err)
			require.Equal(t, "contents from the actual original file", string(original))
		})
	}
}

func TestPathTranslationRemoteCompactionUsesProtectedPrompt(t *testing.T) {
	env := testEnv(t)
	stateDir := t.TempDir()
	conn, err := db.Connect(t.Context(), stateDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
	store := pathtranslate.NewStore(conn)
	current, err := env.sessions.Create(t.Context(), "compaction")
	require.NoError(t, err)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{
		Scope: pathtranslate.Scope{Kind: "session", SessionID: current.ID},
		Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{
			{Path: "/private/compaction-library", Recursive: true},
		}},
	})
	require.NoError(t, err)
	compactor := &remoteCompactionModel{}
	agent := newSummaryTestAgent(env, compactor)
	configured := configureRemoteCompaction(agent, compactor)
	configured.PathTranslation = func(ctx context.Context) (*pathtranslate.Translator, error) {
		require.Equal(t, current.ID, tools.GetSessionFromContext(ctx))
		names, err := store.Names(ctx)
		if err != nil {
			return nil, err
		}
		return pathtranslate.New(names, policy.Settings, "/private")
	}
	agent.largeModel.Set(configured)
	_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Read /private/compaction-library/confidential.txt"}},
	})
	require.NoError(t, err)
	require.NoError(t, agent.Summarize(t.Context(), current.ID, nil, nil))
	require.EqualValues(t, 1, compactor.compactCalls.Load())
	_, call := compactor.snapshot()
	encoded, err := json.Marshal(call.Prompt)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "compaction-library")
	require.NotContains(t, string(encoded), "confidential.txt")
	require.Contains(t, string(encoded), pathtranslate.AliasPrefix)
}

func TestPathTranslationToolWrapperRejectsIdentityStateAndAllowsOrdinaryRead(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "state guard")
	require.NoError(t, err)
	stateDir := filepath.Join(env.workingDir, "state")
	conn, err := db.Connect(t.Context(), stateDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
	store := pathtranslate.NewStore(conn)
	_, err = store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: current.ID}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, pathtranslate.Settings{}, env.workingDir)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, current.ID)
	ctx = pathtranslate.WithTranslator(ctx, translator)
	view := translateTools([]fantasy.AgentTool{tools.NewViewTool(nil, env.permissions, *env.filetracker, nil, env.workingDir)})[0]
	response, err := view.Run(ctx, fantasy.ToolCall{ID: "private", Name: tools.ViewToolName, Input: `{"file_path":"state/crux.db"}`})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, pathtranslate.ErrInternalStateAccess.Error(), response.Content)
	ordinary := filepath.Join(stateDir, "ordinary.txt")
	require.NoError(t, os.WriteFile(ordinary, []byte("ordinary contents"), 0o600))
	response, err = view.Run(ctx, fantasy.ToolCall{ID: "ordinary", Name: tools.ViewToolName, Input: `{"file_path":"state/ordinary.txt"}`})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Contains(t, response.Content, "ordinary contents")
}

func TestPathTranslationChildInheritsParentWithoutAffectingIndependentSession(t *testing.T) {
	env := testEnv(t)
	stateDir := t.TempDir()
	conn, err := db.Connect(t.Context(), stateDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
	store := pathtranslate.NewStore(conn)
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), "child", parent.ID, "child")
	require.NoError(t, err)
	other, err := env.sessions.Create(t.Context(), "independent")
	require.NoError(t, err)
	fork, err := env.sessions.CreateFork(t.Context(), parent.ID, "fork")
	require.NoError(t, err)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{
		Scope: pathtranslate.Scope{Kind: "session", SessionID: parent.ID},
		Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{
			{Path: "/private/library", Recursive: true},
		}},
	})
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO path_translation_scopes(scope_key, settings) VALUES (?, ?), (?, ?)`, `{"kind":"provider","provider":"provider"}`, `{"enabled":true,"targets":[{"path":"/private/library","recursive":true}]}`, `{"kind":"model","provider":"provider","model":"model"}`, `{"enabled":true,"targets":[{"path":"/private/library","recursive":true}]}`)
	require.NoError(t, err)
	coordinator := &coordinator{sessions: env.sessions, pathTranslations: store}
	coordinator.cfg = initTestConfig(t, env.workingDir)
	effective, err := coordinator.effectivePathTranslation(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, policy, effective)
	provider := &summaryCaptureModel{finishStreamModel: finishStreamModel{text: "done"}}
	bound := coordinator.bindPathTranslation(Model{Model: provider, ModelCfg: config.SelectedModel{Provider: "provider", Model: "model"}})
	for _, sessionID := range []string{parent.ID, child.ID, other.ID, fork.ID, ""} {
		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, sessionID)
		_, err := bound.Model.Stream(ctx, fantasy.Call{Prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Read /private/library/child.txt"}}}}})
		require.NoError(t, err)
	}
	calls, _ := provider.snapshot()
	require.Len(t, calls, 5)
	for index, call := range calls {
		encoded, err := json.Marshal(call.Prompt)
		require.NoError(t, err)
		if index < 2 {
			require.NotContains(t, string(encoded), "child.txt")
		} else {
			require.Contains(t, string(encoded), "/private/library/child.txt")
		}
	}
	effective, err = coordinator.effectivePathTranslation(t.Context(), other.ID)
	require.NoError(t, err)
	require.False(t, effective.Settings.Enabled)
	disabled, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: child.ID}})
	require.NoError(t, err)
	effective, err = coordinator.effectivePathTranslation(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, disabled, effective)
	disabled.Settings.Inherit = true
	_, err = store.Save(t.Context(), disabled)
	require.NoError(t, err)
	effective, err = coordinator.effectivePathTranslation(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, policy, effective)
}

func TestPathTranslationPolicyChangesResetContinuationOnlyWhenNeeded(t *testing.T) {
	env := testEnv(t)
	stateDir := t.TempDir()
	conn, err := db.Connect(t.Context(), stateDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
	store := pathtranslate.NewStore(conn)
	current, err := env.sessions.Create(t.Context(), "policy changes")
	require.NoError(t, err)
	coordinator := &coordinator{sessions: env.sessions, pathTranslations: store, cfg: initTestConfig(t, env.workingDir)}
	provider := &summaryCaptureModel{finishStreamModel: finishStreamModel{text: "done"}}
	bound := coordinator.bindPathTranslation(Model{Model: provider, ModelCfg: config.SelectedModel{Provider: "provider", Model: "model"}})
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, current.ID)
	send := func() {
		_, err := bound.Model.Stream(ctx, fantasy.Call{Prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Read /private/library/file.txt"}}}}})
		require.NoError(t, err)
	}
	send()
	send()
	_, resets := provider.snapshot()
	require.Empty(t, resets)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{
		Scope: pathtranslate.Scope{Kind: "session", SessionID: current.ID},
		Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{
			{Path: "/private/library", Recursive: true},
		}},
	})
	require.NoError(t, err)
	send()
	send()
	_, resets = provider.snapshot()
	require.Len(t, resets, 1)
	policy, err = store.Save(t.Context(), policy)
	require.NoError(t, err)
	send()
	_, resets = provider.snapshot()
	require.Len(t, resets, 1)
	policy.Settings.Enabled = false
	_, err = store.Save(t.Context(), policy)
	require.NoError(t, err)
	send()
	send()
	_, resets = provider.snapshot()
	require.Len(t, resets, 2)
}

func TestPathTranslationRebuildsOpaqueCheckpointBeforeContinuing(t *testing.T) {
	env := testEnv(t)
	stateDir := t.TempDir()
	conn, err := db.Connect(t.Context(), stateDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
	store := pathtranslate.NewStore(conn)
	current, err := env.sessions.Create(t.Context(), "checkpoint policy")
	require.NoError(t, err)
	original, err := env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Read /private/library/confidential.txt"}},
	})
	require.NoError(t, err)
	provider := &remoteCompactionModel{}
	agent := newSummaryTestAgent(env, provider)
	configured := configureRemoteCompaction(agent, provider)
	require.NoError(t, agent.Summarize(t.Context(), current.ID, nil, nil))
	current, err = env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	oldCheckpoint := current.SummaryMessageID
	policy, err := store.Save(t.Context(), pathtranslate.Policy{
		Scope: pathtranslate.Scope{Kind: "session", SessionID: current.ID},
		Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{
			{Path: "/private/library", Recursive: true},
		}},
	})
	require.NoError(t, err)
	coordinator := &coordinator{sessions: env.sessions, pathTranslations: store, cfg: initTestConfig(t, env.workingDir)}
	configured = coordinator.bindPathTranslation(configured)
	agent.largeModel.Set(configured)
	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "Continue"})
	require.NoError(t, err)
	require.EqualValues(t, 2, provider.compactCalls.Load())
	require.EqualValues(t, 1, provider.streamCalls.Load())
	current, err = env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.NotEqual(t, oldCheckpoint, current.SummaryMessageID)
	checkpoint, err := env.messages.Get(t.Context(), current.SummaryMessageID)
	require.NoError(t, err)
	require.NotEmpty(t, checkpoint.Content().PathTranslationFingerprint)
	prompts, compactCall := provider.snapshot()
	for _, prompt := range append(prompts, compactCall.Prompt) {
		payload, err := json.Marshal(prompt)
		require.NoError(t, err)
		require.NotContains(t, string(payload), "confidential.txt")
		require.NotContains(t, string(payload), "/private/library")
	}
	savedOriginal, err := env.messages.Get(t.Context(), original.ID)
	require.NoError(t, err)
	require.Equal(t, original.Content().Text, savedOriginal.Content().Text)
	policy.Settings.Enabled = false
	_, err = store.Save(t.Context(), policy)
	require.NoError(t, err)
	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "Continue without translation"})
	require.NoError(t, err)
	require.EqualValues(t, 3, provider.compactCalls.Load())
	require.EqualValues(t, 2, provider.streamCalls.Load())
	_, compactCall = provider.snapshot()
	payload, err := json.Marshal(compactCall.Prompt)
	require.NoError(t, err)
	require.Contains(t, string(payload), "/private/library/confidential.txt")
}

func TestPathTranslationSearchResultsPreserveWholeNames(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "search paths")
	require.NoError(t, err)
	workingDir, err := filepath.EvalSymlinks(env.workingDir)
	require.NoError(t, err)
	protected := filepath.Join(workingDir, "private library")
	require.NoError(t, os.Mkdir(protected, 0o700))
	original := filepath.Join(protected, "秘密 name.txt")
	require.NoError(t, os.WriteFile(original, []byte("find this content"), 0o600))
	stateDir := t.TempDir()
	conn, err := db.Connect(t.Context(), stateDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
	store := pathtranslate.NewStore(conn)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: current.ID}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected, Recursive: true}}}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, policy.Settings, workingDir)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, current.ID)
	ctx = pathtranslate.WithTranslator(ctx, translator)
	search := translateTools(codexBoundedTools([]fantasy.AgentTool{tools.NewSearchTool(env.permissions, workingDir, config.ToolSearch{})}, Model{InstructionPolicy: fantasy.InstructionPolicyCodex}))[0]
	for _, params := range []tools.SearchParams{
		{Mode: tools.SearchModeFiles, Pattern: "**/*.txt"},
		{Mode: tools.SearchModeContent, Pattern: "find this", LiteralText: true},
	} {
		input, err := json.Marshal(params)
		require.NoError(t, err)
		response, err := search.Run(ctx, fantasy.ToolCall{ID: "search", Name: tools.SearchToolName, Input: string(input)})
		require.NoError(t, err)
		require.False(t, response.IsError)
		require.Contains(t, response.Content, "秘密 name.txt")
		prompt := fantasy.Prompt{{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "search", ClientMetadata: response.Metadata, Output: fantasy.ToolResultOutputContentText{Text: response.Content}}}}}
		translated, err := translator.Prompt(ctx, prompt)
		require.NoError(t, err)
		payload, err := json.Marshal(translated)
		require.NoError(t, err)
		require.NotContains(t, string(payload), "private library")
		require.NotContains(t, string(payload), "秘密")
		require.NotContains(t, string(payload), "name.txt")
		require.NotContains(t, string(payload), "path_references")
		aliases := regexp.MustCompile(`pt_[a-f0-9]{64}`).FindAllString(string(payload), -1)
		require.NotEmpty(t, aliases)
		resolved, err := translator.ResolvePath(ctx, aliases[len(aliases)-1])
		require.NoError(t, err)
		require.Equal(t, original, resolved)
		view := translateTools([]fantasy.AgentTool{tools.NewViewTool(nil, env.permissions, *env.filetracker, nil, workingDir)})[0]
		input, err = json.Marshal(map[string]string{"file_path": aliases[len(aliases)-1]})
		require.NoError(t, err)
		read, err := view.Run(ctx, fantasy.ToolCall{ID: "read", Name: tools.ViewToolName, Input: string(input)})
		require.NoError(t, err)
		require.False(t, read.IsError)
		require.Contains(t, read.Content, "find this content")
	}
	t.Run("bounded directory listing", func(t *testing.T) {
		for index := range 300 {
			name := fmt.Sprintf("confidential item %03d with a deliberately long filename.txt", index)
			require.NoError(t, os.WriteFile(filepath.Join(protected, name), nil, 0o600))
		}
		require.NoError(t, os.WriteFile(filepath.Join(protected, "line\nbreak.txt"), nil, 0o600))
		listing := translateTools(codexBoundedTools([]fantasy.AgentTool{tools.NewLsTool(env.permissions, workingDir, config.ToolLs{})}, Model{InstructionPolicy: fantasy.InstructionPolicyCodex}))[0]
		input, err := json.Marshal(tools.LSParams{Path: protected})
		require.NoError(t, err)
		response, err := listing.Run(ctx, fantasy.ToolCall{ID: "listing", Name: tools.LSToolName, Input: string(input)})
		require.NoError(t, err)
		require.False(t, response.IsError)
		require.LessOrEqual(t, len(response.Content), 12000)
		prompt := fantasy.Prompt{
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "listing", ToolName: tools.LSToolName, Input: string(input)}}},
			{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "listing", ClientMetadata: response.Metadata, Output: fantasy.ToolResultOutputContentText{Text: response.Content}}}},
		}
		translated, err := translator.Prompt(ctx, prompt)
		require.NoError(t, err)
		payload, err := json.Marshal(translated)
		require.NoError(t, err)
		require.NotContains(t, string(payload), "confidential")
		require.NotContains(t, string(payload), "private library")
		require.NotContains(t, string(payload), "break.txt")
		aliases := regexp.MustCompile(`pt_[a-f0-9]{64}`).FindAllString(string(payload), -1)
		require.Greater(t, len(aliases), 2)
		last, err := translator.ResolvePath(ctx, aliases[len(aliases)-1])
		require.NoError(t, err)
		_, err = os.Stat(last)
		require.NoError(t, err)
	})
}

func TestPathTranslationLargeDirectoryAdmission(t *testing.T) {
	count := 1000
	if value := os.Getenv("CRUX_TEST_PATH_TRANSLATION_ENTRIES"); value != "" {
		var err error
		count, err = strconv.Atoi(value)
		require.NoError(t, err)
		require.Positive(t, count)
	}
	env := testEnv(t)
	root, err := filepath.EvalSymlinks(env.workingDir)
	require.NoError(t, err)
	protected := filepath.Join(root, "large-private-library")
	require.NoError(t, os.Mkdir(protected, 0o700))
	started := time.Now()
	for index := range count {
		directory := filepath.Join(protected, fmt.Sprintf("group-%04d", index/1000))
		if index%1000 == 0 {
			require.NoError(t, os.Mkdir(directory, 0o700))
		}
		require.NoError(t, os.WriteFile(filepath.Join(directory, fmt.Sprintf("private-item-%06d", index)), nil, 0o600))
		if (index+1)%100000 == 0 {
			t.Logf("created %d actual files in %s", index+1, time.Since(started))
		}
	}
	stateDir := t.TempDir()
	conn, err := db.Connect(t.Context(), stateDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
	store := pathtranslate.NewStore(conn)
	current, err := env.sessions.Create(t.Context(), "large-directory")
	require.NoError(t, err)
	started = time.Now()
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: current.ID}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected, Recursive: true}}}})
	require.NoError(t, err)
	resolve := func(ctx context.Context) (*pathtranslate.Translator, error) {
		names, err := store.Names(ctx)
		if err != nil {
			return nil, err
		}
		return pathtranslate.New(names, policy.Settings, root)
	}
	model := translatedLanguageModel{LanguageModel: &finishStreamModel{text: "ordinary answer"}, resolve: resolve}
	agent := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "Answer ordinary questions.")
	runtime := agent.Runtime()
	runtime.LargeModel.PathTranslation = resolve
	agent.SetRuntime(runtime)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	response, err := agent.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "What is two plus two?"})
	require.NoError(t, err)
	require.NotNil(t, response)
	t.Logf("enabled policy and completed actual SessionAgent turn with %d files present in %s", count, time.Since(started))
	var mappings int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM path_translation_names`).Scan(&mappings))
	require.Zero(t, mappings)
	translator, err := resolve(t.Context())
	require.NoError(t, err)
	alias, err := translator.Path(t.Context(), filepath.Join(protected, "group-0000", "private-item-000000"))
	require.NoError(t, err)
	original, err := translator.ResolvePath(t.Context(), alias)
	require.NoError(t, err)
	_, err = os.Stat(original)
	require.NoError(t, err)
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM path_translation_names`).Scan(&mappings))
	require.Equal(t, 3, mappings)
}

func TestPathTranslationSessionImageCopy(t *testing.T) {
	env := testEnv(t)
	root, err := filepath.EvalSymlinks(env.workingDir)
	require.NoError(t, err)
	protected := filepath.Join(root, "private-images")
	require.NoError(t, os.Mkdir(protected, 0o700))
	path := filepath.Join(protected, "private-picture.png")
	pixels := image.NewNRGBA(image.Rect(0, 0, 640, 480))
	for y := range 480 {
		for x := range 640 {
			pixels.SetNRGBA(x, y, color.NRGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x/4+y/4)%2) * 255, A: 255})
		}
	}
	var source bytes.Buffer
	require.NoError(t, png.Encode(&source, pixels))
	require.NoError(t, os.WriteFile(path, source.Bytes(), 0o600))
	stateDir := t.TempDir()
	conn, err := db.Connect(t.Context(), stateDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(stateDir)) })
	store := pathtranslate.NewStore(conn)
	current, err := env.sessions.Create(t.Context(), "image-copy")
	require.NoError(t, err)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: current.ID}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: protected, Recursive: true, CensorImages: true}}}})
	require.NoError(t, err)
	resolve := func(ctx context.Context) (*pathtranslate.Translator, error) {
		names, err := store.Names(ctx)
		if err != nil {
			return nil, err
		}
		return pathtranslate.New(names, policy.Settings, root)
	}
	provider := &translationSequenceModel{finishStreamModel: finishStreamModel{text: "image read"}}
	model := translatedLanguageModel{LanguageModel: provider, resolve: resolve}
	agent := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "Read the image.", tools.NewLsTool(env.permissions, root, config.ToolLs{}), tools.NewViewTool(nil, env.permissions, *env.filetracker, nil, root))
	runtime := agent.Runtime()
	runtime.LargeModel.CatalogModel.SupportsImages = true
	runtime.LargeModel.PathTranslation = resolve
	agent.SetRuntime(runtime)
	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "List " + protected + " and view the image."})
	require.NoError(t, err)
	require.Len(t, provider.calls, 3)
	var providerImage []byte
	for _, msg := range provider.calls[2].Prompt {
		for _, part := range msg.Content {
			if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
				require.Empty(t, providerImage)
				providerImage = file.Data
			}
		}
	}
	require.NotEmpty(t, providerImage)
	normalized, err := imageattachment.Normalize(imageattachment.ChatPolicy(nil), message.Attachment{FileName: path, MimeType: "image/png", Content: source.Bytes()})
	require.NoError(t, err)
	censored, err := imageutil.Censor(normalized.Content)
	require.NoError(t, err)
	expected, err := imageattachment.Normalize(imageattachment.ChatPolicy(nil), message.Attachment{FileName: path, MimeType: "image/png", Content: censored})
	require.NoError(t, err)
	require.True(t, bytes.Equal(expected.Content, providerImage), "provider copy must be censored and conform to image policy")
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source.Bytes(), unchanged)
	payload, err := json.Marshal(provider.calls[2].Prompt)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "private-picture")
	require.NotContains(t, string(payload), "private-images")
	if directory := os.Getenv("CRUX_TEST_PATH_TRANSLATION_ARTIFACT_DIR"); directory != "" {
		require.NoError(t, os.MkdirAll(directory, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "original.png"), source.Bytes(), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "provider-image.jpg"), providerImage, 0o600))
	}
}
