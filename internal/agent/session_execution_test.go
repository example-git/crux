package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorSessionModelsApplyToManualAndAutomaticTurns(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	env := testEnv(t)
	var mu sync.Mutex
	var requests []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if r.Header.Get("x-request-purpose") != "title" {
			mu.Lock()
			requests = append(requests, body.Model)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"session response\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body.Model, body.Model)
	}))
	t.Cleanup(host.Close)
	configuration := fmt.Sprintf(`{"options":{"disable_default_providers":true,"disable_auto_summarize":true},"providers":{"first":{"type":"openai-compat","base_url":%q,"api_key":"synthetic-first","models":[{"id":"first-model","name":"First","context_window":200000,"default_max_tokens":128}]},"second":{"type":"openai-compat","base_url":%q,"api_key":"synthetic-second","models":[{"id":"second-model","name":"Second","context_window":200000,"default_max_tokens":128}]}},"models":{"large":{"provider":"first","model":"first-model"},"small":{"provider":"first","model":"first-model"}}}`, host.URL+"/v1", host.URL+"/v1")
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crux.json"), []byte(configuration), 0o600))
	store := initTestConfig(t, env.workingDir)
	store.SetupAgents()
	saved := store.RuntimeSnapshot().AgentModelState()
	encoded, err := json.Marshal(saved)
	require.NoError(t, err)
	var sessions []session.Session
	for _, title := range []string{"translation", "categorization"} {
		current, err := env.sessions.Create(t.Context(), title)
		require.NoError(t, err)
		current, err = env.sessions.SetModels(t.Context(), current.ID, 0, encoded)
		require.NoError(t, err)
		sessions = append(sessions, current)
	}
	owner, ok := store.RuntimeSnapshot().ProviderOwner("second")
	require.True(t, ok)
	selected := &config.OwnedSelectedModel{Owner: owner, Model: config.SelectedModel{Provider: "second", Model: "second-model"}}
	changed := config.AgentModelState{Large: selected, Small: selected}
	_, err = store.OverrideModelsForOwners(changed)
	require.NoError(t, err)
	built, err := NewCoordinator(t.Context(), CoordinatorOptions{Config: store, Sessions: env.sessions, Messages: env.messages, Permissions: env.permissions, History: env.history, FileTracker: *env.filetracker})
	require.NoError(t, err)
	coord := built.(*coordinator)
	t.Cleanup(coord.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, current := range sessions {
		_, err = coord.Run(ctx, current.ID, "manual session turn")
		require.NoError(t, err)
		_, err = coord.run(ctx, nil, true, current.ID, goalContinuationPrompt)
		require.NoError(t, err)
	}
	manual, err := env.sessions.Create(t.Context(), "third session")
	require.NoError(t, err)
	_, err = coord.Run(ctx, manual.ID, "third session turn")
	require.NoError(t, err)
	changedJSON, err := json.Marshal(changed)
	require.NoError(t, err)
	_, err = env.sessions.SetModels(ctx, sessions[0].ID, 1, changedJSON)
	require.NoError(t, err)
	_, err = coord.run(ctx, nil, true, sessions[0].ID, goalContinuationPrompt)
	require.NoError(t, err)
	_, err = coord.Run(ctx, sessions[1].ID, "unchanged session")
	require.NoError(t, err)
	require.NoError(t, env.messages.FlushAll(ctx))
	mu.Lock()
	actual := append([]string(nil), requests...)
	mu.Unlock()
	require.Equal(t, []string{"first-model", "first-model", "first-model", "first-model", "second-model", "second-model", "first-model"}, actual)
	for _, current := range sessions {
		stored, err := env.messages.List(ctx, current.ID)
		require.NoError(t, err)
		for _, msg := range stored {
			if msg.Role == message.Assistant {
				require.Equal(t, "session response", msg.Content().Text)
			}
		}
	}
}

func TestSessionModelChangesApplyAtNextStepInBothModes(t *testing.T) {
	for _, internal := range []bool{false, true} {
		for _, sameSession := range []bool{false, true} {
			t.Run(fmt.Sprintf("internal=%t/same_session=%t", internal, sameSession), func(t *testing.T) {
				env := testEnv(t)
				first := &goalHandoffModel{finishStreamModel: finishStreamModel{text: "first response"}}
				second := &summaryCaptureModel{finishStreamModel: finishStreamModel{text: "second response"}}
				var a SessionAgent
				var bound InstalledRuntime
				tool := fantasy.NewAgentTool[struct{}]("work", "Perform work", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					next := a.Runtime()
					next.LargeModel.Model = second
					next.LargeModel.ModelCfg = config.SelectedModel{Provider: "second", Model: "second-model"}
					next.ModelRevision = bound.ModelRevision + 1
					if sameSession {
						bound = next
					} else {
						a.SetRuntime(next)
					}
					return fantasy.NewTextResponse("work completed"), nil
				})
				a = testSessionAgent(env, first, &finishStreamModel{text: "title"}, "system", tool)
				bound = a.Runtime()
				bound.ModelRevision = 1
				bound.LargeModel.ModelCfg = config.SelectedModel{Provider: "first", Model: "first-model"}
				a.SetRuntime(bound)
				current, err := env.sessions.Create(t.Context(), "session")
				require.NoError(t, err)
				_, err = a.Run(t.Context(), SessionAgentCall{
					SessionID: current.ID, Prompt: "work", Internal: internal, runtime: &bound,
					RuntimeProvider: func(context.Context) (InstalledRuntime, error) { return bound, nil },
					PrepareRuntime: func(context.Context, InstalledRuntime) (fantasy.StepModelSettings, error) {
						return fantasy.StepModelSettings{}, nil
					},
				})
				require.NoError(t, err)
				calls, _ := second.snapshot()
				if sameSession {
					require.Equal(t, 1, first.calls)
					require.Len(t, calls, 1)
				} else {
					require.Equal(t, 2, first.calls)
					require.Empty(t, calls)
				}
			})
		}
	}
}
