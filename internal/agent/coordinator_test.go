package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openaicompat"
	"github.com/example-git/crux/internal/automemory"
	"github.com/example-git/crux/internal/codebaseindex"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/codex"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/oauth/gemini/antigravity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSessionAgent is a minimal mock for the SessionAgent interface.
type mockSessionAgent struct {
	model     Model
	runFunc   func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	cancelled []string
}

func (m *mockSessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	return m.runFunc(ctx, call)
}

func (m *mockSessionAgent) BeginAccepted(sessionID string) *AcceptedRun {
	return &AcceptedRun{sessionID: sessionID}
}

func (m *mockSessionAgent) Model() Model                                      { return m.model }
func (m *mockSessionAgent) SetModels(large, small Model)                      {}
func (m *mockSessionAgent) SetTools(tools, planModeTools []fantasy.AgentTool) {}
func (m *mockSessionAgent) SetSystemPrompt(systemPrompt string)               {}
func (m *mockSessionAgent) Cancel(sessionID string) {
	m.cancelled = append(m.cancelled, sessionID)
}
func (m *mockSessionAgent) CancelAll()                                        {}
func (m *mockSessionAgent) IsSessionBusy(sessionID string) bool               { return false }
func (m *mockSessionAgent) IsBusy() bool                                      { return false }
func (m *mockSessionAgent) QueuedPrompts(sessionID string) int                { return 0 }
func (m *mockSessionAgent) QueuedPromptsList(sessionID string) []QueuedPrompt { return nil }
func (m *mockSessionAgent) ClearQueue(sessionID string)                       {}
func (m *mockSessionAgent) Summarize(context.Context, string, fantasy.ProviderOptions, func(context.Context, *fantasy.ProviderError) error) error {
	return nil
}
func (m *mockSessionAgent) GenerateTitle(context.Context, string, string) {}
func (m *mockSessionAgent) GenerateMemory(context.Context, string, string, int64) (string, error) {
	return `{"memories":[]}`, nil
}

func (m *mockSessionAgent) SuggestPrompt(context.Context, string) (string, error) { return "", nil }

// newTestCoordinator creates a minimal coordinator for unit testing runSubAgent.
func newTestCoordinator(t *testing.T, env fakeEnv, providerID string, providerCfg config.ProviderConfig) *coordinator {
	cfg := initTestConfig(t, env.workingDir)
	cfg.Config().Providers.Set(providerID, providerCfg)
	return &coordinator{
		cfg:      cfg,
		sessions: env.sessions,
		messages: env.messages,
	}
}

// newMockAgent creates a mockSessionAgent with the given provider and run function.
func newMockAgent(providerID string, maxTokens int64, runFunc func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)) *mockSessionAgent {
	return &mockSessionAgent{
		model: Model{
			CatwalkCfg: catwalk.Model{
				DefaultMaxTokens: maxTokens,
			},
			ModelCfg: config.SelectedModel{
				Provider: providerID,
			},
		},
		runFunc: runFunc,
	}
}

// agentResultWithText creates a minimal AgentResult with the given text response.
func agentResultWithText(text string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: text},
			},
		},
	}
}

func TestScheduleCodebaseIndexReconcileIsAsyncAndCoalesced(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	coord := &coordinator{
		reconcileCodebaseIndexFn: func(context.Context) (codebaseindex.StoreStatus, error) {
			started <- struct{}{}
			<-release
			return codebaseindex.StoreStatus{}, nil
		},
	}

	returned := make(chan struct{})
	go func() {
		coord.scheduleCodebaseIndexReconcile(t.Context())
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("scheduler waited for index reconciliation")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("index reconciliation did not start")
	}

	coord.scheduleCodebaseIndexReconcile(t.Context())
	select {
	case <-started:
		t.Fatal("concurrent index reconciliation was not coalesced")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.Eventually(t, func() bool {
		coord.codebaseIndexReconcileMu.Lock()
		defer coord.codebaseIndexReconcileMu.Unlock()
		return !coord.codebaseIndexReconciling
	}, time.Second, 10*time.Millisecond)

	coord.scheduleCodebaseIndexReconcile(t.Context())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("index reconciliation could not be scheduled after completion")
	}
}

func TestCoordinatorLoadsBoundedMemoryInputsFromPersistedState(t *testing.T) {
	env := testEnv(t)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	_, err = env.sessions.CreateTaskSession(t.Context(), "tool-call", parent.ID, "Child")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), parent.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "remember focused tests"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), parent.ID, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.TextContent{Text: "tool noise"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), parent.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "acknowledged"}},
	})
	require.NoError(t, err)
	coord := &coordinator{sessions: env.sessions, messages: env.messages}

	turns, err := coord.loadMemoryTranscript(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Equal(t, []automemory.Turn{
		{Role: "user", Text: "remember focused tests"},
		{Role: "assistant", Text: "acknowledged"},
	}, turns)
	sessions, err := coord.loadMemorySessions(t.Context())
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, parent.ID, sessions[0].ID)
}

func TestRunSubAgent(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("happy path", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			assert.Equal(t, "do something", call.Prompt)
			assert.Equal(t, int64(4096), call.MaxOutputTokens)
			return agentResultWithText("done"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "do something",
			SessionTitle:   "Test Session",
		})
		require.NoError(t, err)
		assert.Equal(t, "done", resp.Content)
		assert.False(t, resp.IsError)
	})

	t.Run("cost update failure preserves output", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("output before cost failure"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      "missing-parent-session",
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "output before cost failure", resp.Content)
	})

	t.Run("response with text returns it", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("the answer"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "the answer", resp.Content)
	})

	t.Run("nil result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("empty result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("ModelCfg.MaxTokens overrides default", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := &mockSessionAgent{
			model: Model{
				CatwalkCfg: catwalk.Model{
					DefaultMaxTokens: 4096,
				},
				ModelCfg: config.SelectedModel{
					Provider:  providerID,
					MaxTokens: 8192,
				},
			},
			runFunc: func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
				assert.Equal(t, int64(8192), call.MaxOutputTokens)
				return agentResultWithText("ok"), nil
			},
		}

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Content)
	})

	t.Run("session creation failure with canceled context", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, nil)

		// Use a canceled context to trigger CreateTaskSession failure.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = coord.runSubAgent(ctx, subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
	})

	t.Run("provider not configured", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		// Agent references a provider that doesn't exist in config.
		agent := newMockAgent("unknown-provider", 4096, nil)

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "model provider not configured")
	})

	t.Run("agent run error returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, errors.New("provider request failed")
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		// runSubAgent returns (errorResponse, nil) when agent.Run fails — not a Go error.
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Failed to generate response: provider request failed", resp.Content)
	})

	t.Run("session setup callback is invoked", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var setupCalledWith string
		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
			SessionSetup: func(sessionID string) {
				setupCalledWith = sessionID
			},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, setupCalledWith, "SessionSetup should have been called")
	})

	t.Run("cost propagation to parent session", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			// Simulate the agent incurring cost by updating the child session.
			childSession, err := env.sessions.Get(ctx, call.SessionID)
			if err != nil {
				return nil, err
			}
			childSession.Cost = 0.05
			_, err = env.sessions.Save(ctx, childSession)
			if err != nil {
				return nil, err
			}
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.05, updated.Cost, 1e-9)
	})
}

func TestUpdateParentSessionCost(t *testing.T) {
	t.Run("accumulates cost correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		// Set child cost.
		child.Cost = 0.10
		_, err = env.sessions.Save(t.Context(), child)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID, 0)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.10, updated.Cost, 1e-9)
	})

	t.Run("accumulates multiple child costs", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child1, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child1")
		require.NoError(t, err)
		child1.Cost = 0.05
		_, err = env.sessions.Save(t.Context(), child1)
		require.NoError(t, err)

		child2, err := env.sessions.CreateTaskSession(t.Context(), "tool-2", parent.ID, "Child2")
		require.NoError(t, err)
		child2.Cost = 0.03
		_, err = env.sessions.Save(t.Context(), child2)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child1.ID, parent.ID, 0)
		require.NoError(t, err)
		err = coord.updateParentSessionCost(t.Context(), child2.ID, parent.ID, 0)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.08, updated.Cost, 1e-9)
	})

	t.Run("child session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), "non-existent", parent.ID, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get child session")
	})

	t.Run("parent session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, "non-existent", 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get parent session")
	})

	t.Run("adds only cost incurred by a continued child turn", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)
		child.Cost = 0.16
		_, err = env.sessions.Save(t.Context(), child)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID, 0.1)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.06, updated.Cost, 1e-9)
	})

	t.Run("zero cost handled correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg := initTestConfig(t, env.workingDir)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID, 0)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.0, updated.Cost, 1e-9)
	})
}

func TestApplyCodexRuntimeOptions(t *testing.T) {
	configured := &config.Options{ResponseVerbosity: "high", AnalysisEffort: "max"}

	options := applyRegisteredRuntimeOptions(codex.ID, "gpt-5.6-sol", configured, fantasy.ProviderOptions{})
	parsed, ok := options[codexresponses.Name].(*codexresponses.ProviderOptions)
	require.True(t, ok)
	assert.Equal(t, "high", parsed.ResponseVerbosity)
	assert.Equal(t, "max", parsed.ReasoningEffort)
	assert.False(t, parsed.DisableReasoning)
}

func TestApplyCodexRuntimeOptionsIsRestrictedToCodexGPT56(t *testing.T) {
	configured := &config.Options{ResponseVerbosity: "high", AnalysisEffort: "max"}

	for _, tc := range []struct {
		name       string
		providerID string
		modelID    string
	}{
		{name: "other provider", providerID: "openai", modelID: "gpt-5.6-sol"},
		{name: "other model", providerID: codex.ID, modelID: "gpt-5.5-codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := applyRegisteredRuntimeOptions(tc.providerID, tc.modelID, configured, fantasy.ProviderOptions{})
			assert.Empty(t, options)
		})
	}
}

func TestGetProviderOptionsReasoningEffort(t *testing.T) {
	model := Model{
		CatwalkCfg: catwalk.Model{
			ID:              "deepseek-chat",
			CanReason:       true,
			ReasoningLevels: []string{"high"},
		},
		ModelCfg: config.SelectedModel{Provider: "deepseek", ReasoningEffort: "high"},
	}

	opts := getProviderOptions(model, config.ProviderConfig{ID: "deepseek", Type: catwalk.TypeOpenAICompat})
	raw, ok := opts[openaicompat.Name]
	require.True(t, ok)
	parsed, ok := raw.(*openaicompat.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, parsed.ReasoningEffort)
	assert.Equal(t, "high", string(*parsed.ReasoningEffort))
}

func TestGeminiProviderDoesNotConfigureThinkingForGPTOSS(t *testing.T) {
	model := Model{
		CatwalkCfg: catwalk.Model{
			ID: "gpt-oss-120b-medium",
		},
		ModelCfg: config.SelectedModel{
			Provider: gemini.ID,
		},
	}

	opts := getProviderOptions(model, config.ProviderConfig{ID: gemini.ID})
	parsed, ok := opts[antigravity.Name].(*antigravity.ProviderOptions)
	require.True(t, ok)
	assert.Nil(t, parsed.ThinkingConfig)
}

func TestIsUnauthorized(t *testing.T) {
	t.Run("nil error", func(t *testing.T) {
		assert.False(t, isUnauthorized(nil))
	})

	t.Run("non-provider error", func(t *testing.T) {
		assert.False(t, isUnauthorized(errors.New("something broke")))
	})

	t.Run("provider error with 401", func(t *testing.T) {
		err := &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "unauthorized"}
		assert.True(t, isUnauthorized(err))
	})

	t.Run("provider error with non-401", func(t *testing.T) {
		err := &fantasy.ProviderError{StatusCode: http.StatusForbidden, Message: "forbidden"}
		assert.False(t, isUnauthorized(err))
	})

	t.Run("wrapped provider error with 401", func(t *testing.T) {
		inner := &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "expired"}
		err := fmt.Errorf("request failed: %w", inner)
		assert.True(t, isUnauthorized(err))
	})
}

func TestGetProviderOptionsReasoningEffortFallback(t *testing.T) {
	model := Model{
		CatwalkCfg: catwalk.Model{
			ID:              "glm-5.2",
			CanReason:       true,
			ReasoningLevels: []string{"high", "max"},
		},
		ModelCfg: config.SelectedModel{
			Provider: "zai",
		},
	}
	providerCfg := config.ProviderConfig{
		ID:   string(catwalk.InferenceProviderZAI),
		Type: openaicompat.Name,
	}

	opts := getProviderOptions(model, providerCfg)

	raw, ok := opts[openaicompat.Name]
	require.True(t, ok)
	parsed, ok := raw.(*openaicompat.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, parsed.ReasoningEffort)
	assert.Equal(t, "high", string(*parsed.ReasoningEffort))

	thinking, ok := parsed.ExtraBody["thinking"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "enabled", thinking["type"])
}

func TestOAuthModelCatalogsEnableReasoningByDefault(t *testing.T) {
	tests := []struct {
		name   string
		models []catwalk.Model
		levels []string
		effort string
	}{
		{name: codex.ID, models: codex.Models(), levels: []string{"low", "medium", "high", "xhigh"}, effort: "medium"},
		{name: gemini.ID, models: gemini.Models(), levels: []string{"LOW", "MEDIUM", "HIGH"}, effort: "MEDIUM"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NotEmpty(t, tc.models)
			for _, model := range tc.models {
				if model.ID == "gpt-oss-120b-medium" {
					assert.False(t, model.CanReason, model.ID)
					assert.Empty(t, model.ReasoningLevels, model.ID)
					assert.Empty(t, model.DefaultReasoningEffort, model.ID)
					continue
				}
				assert.True(t, model.CanReason, model.ID)
				assert.Equal(t, tc.levels, model.ReasoningLevels, model.ID)
				assert.Equal(t, tc.effort, model.DefaultReasoningEffort, model.ID)
			}
		})
	}
}

func TestIsUnsupportedReasoningMessage(t *testing.T) {
	for _, message := range []string{
		"unsupported reasoning metadata",
		"thinking is not supported for this model",
		"invalid reasoning effort",
		"thinking error",
	} {
		assert.True(t, isUnsupportedReasoningMessage(message), message)
	}
	for _, message := range []string{
		"reasoning completed",
		"request failed",
		"unsupported image metadata",
	} {
		assert.False(t, isUnsupportedReasoningMessage(message), message)
	}
}

func TestBuildAgentModelsPinsRuntimeToEachOAuthProvider(t *testing.T) {
	env := testEnv(t)
	largeCatalog := codex.Models()[0]
	largeCatalog.CanReason = true
	largeCatalog.ReasoningLevels = []string{"low", "medium", "high", "xhigh"}
	largeCatalog.DefaultReasoningEffort = "medium"
	smallCatalog := gemini.Models()[0]
	smallCatalog.CanReason = true
	smallCatalog.ReasoningLevels = []string{"LOW", "MEDIUM", "HIGH"}
	smallCatalog.DefaultReasoningEffort = "MEDIUM"
	future := time.Now().Add(time.Hour).Unix()
	largeProvider := config.ProviderConfig{
		ID:                 codex.ID,
		BaseURL:            "wss://codex.example.test/responses",
		APIKey:             "codex-token",
		OAuthToken:         &oauth.Token{AccessToken: "codex-token", RefreshToken: "codex-refresh", ExpiresAt: future},
		SystemPromptPrefix: "codex-prefix",
		Models:             []catwalk.Model{largeCatalog},
	}
	smallProvider := config.ProviderConfig{
		ID:                 gemini.ID,
		BaseURL:            "https://gemini.example.test",
		APIKey:             "gemini-token",
		OAuthToken:         &oauth.Token{AccessToken: "gemini-token", RefreshToken: "gemini-refresh", ExpiresAt: future},
		SystemPromptPrefix: "gemini-prefix",
		Models:             []catwalk.Model{smallCatalog},
	}
	coord := newTestCoordinator(t, env, codex.ID, largeProvider)
	coord.cfg.Config().Providers.Set(gemini.ID, smallProvider)
	coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: codex.ID,
		Model:    largeCatalog.ID,
	}
	coord.cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
		Provider: gemini.ID,
		Model:    smallCatalog.ID,
	}

	large, small, err := coord.buildAgentModels(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false)
	require.NoError(t, err)
	require.Equal(t, codex.ID, large.ModelCfg.Provider)
	require.Equal(t, "codex-prefix", large.SystemPromptPrefix)
	require.NotNil(t, large.OnAuthRefresh)
	require.Contains(t, large.ProviderOptions, codexresponses.Name)
	require.NotContains(t, large.ProviderOptions, antigravity.Name)
	require.Equal(t, gemini.ID, small.ModelCfg.Provider)
	require.Equal(t, "gemini-prefix", small.SystemPromptPrefix)
	require.NotNil(t, small.OnAuthRefresh)
	require.Contains(t, small.ProviderOptions, antigravity.Name)
	require.NotContains(t, small.ProviderOptions, codexresponses.Name)

	customCatalog := catwalk.Model{ID: "organization/custom/model", DefaultMaxTokens: 8192}
	customProvider := config.ProviderConfig{
		ID:                 "custom-provider",
		Type:               openaicompat.Name,
		BaseURL:            "https://custom.example.test/v1",
		APIKey:             "custom-token",
		OAuthToken:         &oauth.Token{AccessToken: "custom-token", RefreshToken: "custom-refresh", ExpiresAt: future},
		SystemPromptPrefix: "custom-prefix",
		Models:             []catwalk.Model{customCatalog},
	}
	coord.cfg.Config().Providers.Set(customProvider.ID, customProvider)
	customSelection := config.SelectedModel{Provider: customProvider.ID, Model: customCatalog.ID}
	custom, customSmall, err := coord.buildAgentModels(t.Context(), config.Agent{
		PrimaryModelOverride: &customSelection,
	}, true)
	require.NoError(t, err)
	require.Equal(t, customProvider.ID, custom.ModelCfg.Provider)
	require.Equal(t, customCatalog.ID, custom.ModelCfg.Model)
	require.Equal(t, "custom-prefix", custom.SystemPromptPrefix)
	require.NotNil(t, custom.OnAuthRefresh)
	require.Contains(t, custom.ProviderOptions, openaicompat.Name)
	require.NotContains(t, custom.ProviderOptions, codexresponses.Name)
	require.Equal(t, gemini.ID, customSmall.ModelCfg.Provider)
	require.Equal(t, "gemini-prefix", customSmall.SystemPromptPrefix)
	require.NotNil(t, customSmall.OnAuthRefresh)
	require.Contains(t, customSmall.ProviderOptions, antigravity.Name)
}

func TestOAuthReasoningOptionsDisablesCanonicalAdapters(t *testing.T) {
	coord := &coordinator{}

	codexOriginal := &codexresponses.ProviderOptions{ReasoningEffort: "high"}
	coord.disableOAuthReasoning(codex.ID, "codex-model", "reasoning not supported")
	codexResult := coord.oauthReasoningOptions(codex.ID, "codex-model", fantasy.ProviderOptions{codexresponses.Name: codexOriginal})
	codexDisabled := codexResult[codexresponses.Name].(*codexresponses.ProviderOptions)
	assert.True(t, codexDisabled.DisableReasoning)
	assert.Empty(t, codexDisabled.ReasoningEffort)
	assert.False(t, codexOriginal.DisableReasoning)
	assert.Equal(t, "high", codexOriginal.ReasoningEffort)

	includeThoughts := true
	geminiOriginal := &antigravity.ProviderOptions{ThinkingConfig: &antigravity.ThinkingConfig{IncludeThoughts: &includeThoughts}}
	coord.disableOAuthReasoning(gemini.ID, "gemini-model", "thinking error")
	geminiResult := coord.oauthReasoningOptions(gemini.ID, "gemini-model", fantasy.ProviderOptions{antigravity.Name: geminiOriginal})
	geminiDisabled := geminiResult[antigravity.Name].(*antigravity.ProviderOptions)
	assert.Nil(t, geminiDisabled.ThinkingConfig)
	assert.NotNil(t, geminiOriginal.ThinkingConfig)

	otherModelOptions := fantasy.ProviderOptions{codexresponses.Name: codexOriginal}
	otherModelResult := coord.oauthReasoningOptions(codex.ID, "another-model", otherModelOptions)
	assert.Same(t, codexOriginal, otherModelResult[codexresponses.Name])

	// Learned fallback is intentionally coordinator-local. A restarted
	// coordinator retries the configured reasoning policy instead of inheriting
	// stale process state from a previous provider instance.
	restarted := &coordinator{}
	restartedResult := restarted.oauthReasoningOptions(codex.ID, "codex-model", fantasy.ProviderOptions{codexresponses.Name: codexOriginal})
	assert.Same(t, codexOriginal, restartedResult[codexresponses.Name])
}

func TestRunSubAgentDisablesReasoningAfterProviderWarning(t *testing.T) {
	env := testEnv(t)
	providerCfg := config.ProviderConfig{ID: codex.ID}
	coord := newTestCoordinator(t, env, codex.ID, providerCfg)
	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	agent := newMockAgent(codex.ID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		require.NotNil(t, call.OnProviderWarning)
		call.OnProviderWarning(fantasy.CallWarning{Message: "unsupported reasoning metadata"})
		return agentResultWithText("done"), nil
	})
	agent.model.ModelCfg.Model = "gpt-test"

	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parentSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "test",
		SessionTitle:   "Test",
	})
	require.NoError(t, err)
	assert.True(t, coord.reasoningDisabled[oauthReasoningKey(codex.ID, "gpt-test")])
}
