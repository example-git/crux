package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

type notificationFailingMessageService struct {
	message.Service
}

func (s notificationFailingMessageService) Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	for _, part := range params.Parts {
		if text, ok := part.(message.TextContent); ok && strings.Contains(text.Text, "<task-notification>") {
			return message.Message{}, errors.New("notification persistence failed")
		}
	}
	return s.Service.Create(ctx, sessionID, params)
}

func TestQueuedNotificationPersistenceFailureReleasesRemainingNotifications(t *testing.T) {
	coordinator, env := sessionRuntimeTestCoordinator(t)
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	saveNotificationModels(t, coordinator, parent.ID, coordinator.cfg.RuntimeSnapshot().AgentModelState())
	parentAgent := testSessionAgent(env, fastModel{}, fastModel{}, "system").(*sessionAgent)
	parentAgent.messages = notificationFailingMessageService{Service: env.messages}
	coordinator.currentAgent = parentAgent
	parentAgent.activeRequests.Set(parent.ID, &activeCancel{cancel: func() {}})
	var discarded, persisted atomic.Int32
	for _, id := range []string{"i12345678", "i87654321"} {
		err := coordinator.DeliverTaskNotification(t.Context(), managedtask.Notification{
			ID: id, TaskID: id, TaskType: managedtask.TypeImage,
			ParentSessionID: parent.ID, Status: managedtask.StatusCompleted,
		}, func() { persisted.Add(1) }, func() { discarded.Add(1) })
		require.NoError(t, err)
	}
	parentAgent.activeRequests.Del(parent.ID)
	_, err = parentAgent.Run(t.Context(), SessionAgentCall{SessionID: parent.ID, Prompt: "continue", NonInteractive: true})
	require.ErrorContains(t, err, "notification persistence failed")
	require.EqualValues(t, 0, persisted.Load())
	require.EqualValues(t, 2, discarded.Load(), "every drained notification must be released for retry")
}

type notificationFailingModel struct {
	fastModel
	entered chan struct{}
	release chan struct{}
}

func (m *notificationFailingModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	select {
	case m.entered <- struct{}{}:
	default:
	}
	select {
	case <-m.release:
		return nil, errors.New("notification test provider failure")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestQueuedNotificationProviderFailureReleasesNotifications(t *testing.T) {
	coordinator, env := sessionRuntimeTestCoordinator(t)
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	saveNotificationModels(t, coordinator, parent.ID, coordinator.cfg.RuntimeSnapshot().AgentModelState())
	model := &notificationFailingModel{entered: make(chan struct{}, 1), release: make(chan struct{})}
	parentAgent := testSessionAgent(env, model, fastModel{}, "system").(*sessionAgent)
	coordinator.currentAgent = parentAgent
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("parent run did not stop")
		}
	})
	go func() {
		_, runErr := parentAgent.Run(ctx, SessionAgentCall{SessionID: parent.ID, Prompt: "continue", NonInteractive: true})
		done <- runErr
		close(done)
	}()
	select {
	case <-model.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("parent model did not start")
	}
	var discarded, persisted atomic.Int32
	err = coordinator.DeliverTaskNotification(ctx, managedtask.Notification{
		ID: "i12345678", TaskID: "i12345678", TaskType: managedtask.TypeImage,
		ParentSessionID: parent.ID, Status: managedtask.StatusCompleted,
	}, func() { persisted.Add(1) }, func() { discarded.Add(1) })
	require.NoError(t, err)
	_, err = parentAgent.Run(ctx, SessionAgentCall{SessionID: parent.ID, Prompt: "ordinary queued prompt", NonInteractive: true})
	require.NoError(t, err)
	close(model.release)
	select {
	case runErr := <-done:
		require.ErrorContains(t, runErr, "notification test provider failure")
	case <-time.After(3 * time.Second):
		t.Fatal("parent failure did not finish")
	}
	require.EqualValues(t, 0, persisted.Load())
	require.EqualValues(t, 1, discarded.Load())
	queued, _ := parentAgent.messageQueue.Get(parent.ID)
	require.Len(t, queued, 1)
	require.Equal(t, "ordinary queued prompt", queued[0].Prompt)
}

func TestNotificationSessionLoadFailureReleasesDelivery(t *testing.T) {
	coordinator, env := sessionRuntimeTestCoordinator(t)
	parentAgent := testSessionAgent(env, fastModel{}, fastModel{}, "system").(*sessionAgent)
	coordinator.currentAgent = parentAgent
	var persisted, discarded atomic.Int32
	err := coordinator.DeliverTaskNotification(t.Context(), managedtask.Notification{
		ID: "image-completed", TaskID: "i12345678", TaskType: managedtask.TypeImage,
		ParentSessionID: "missing-session", Status: managedtask.StatusCompleted,
	}, func() { persisted.Add(1) }, func() { discarded.Add(1) })
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.EqualValues(t, 0, persisted.Load())
	require.EqualValues(t, 1, discarded.Load())
}

func saveNotificationModels(t *testing.T, coord *coordinator, sessionID string, models config.AgentModelState) {
	t.Helper()
	current, err := coord.sessions.Get(t.Context(), sessionID)
	require.NoError(t, err)
	encoded, err := json.Marshal(models)
	require.NoError(t, err)
	_, err = coord.sessions.SetModels(t.Context(), sessionID, current.ModelRevision, encoded)
	require.NoError(t, err)
}

func TestTaskNotificationUsesParentSessionRuntime(t *testing.T) {
	for _, taskType := range []managedtask.Type{managedtask.TypeAgent, managedtask.TypeImage, managedtask.TypeShell} {
		for _, mode := range []string{"idle", "queued_step", "queued_turn", "queued_changed_parent"} {
			t.Run(string(taskType)+"/"+mode, func(t *testing.T) {
				coord, env := sessionRuntimeTestCoordinator(t)
				type providerRequest struct {
					Model         string           `json:"model"`
					Messages      []map[string]any `json:"messages"`
					MaxTokens     int64            `json:"max_tokens"`
					Temperature   float64          `json:"temperature"`
					Authorization string
				}
				requests := make(chan providerRequest, 4)
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				var calls atomic.Int32
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body providerRequest
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					if r.Header.Get("x-request-purpose") != "title" {
						body.Authorization = r.Header.Get("Authorization")
						requests <- body
						if calls.Add(1) == 1 && (mode == "queued_turn" || mode == "queued_changed_parent") {
							select {
							case <-release:
							case <-r.Context().Done():
								return
							}
						}
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"session response\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body.Model, body.Model)
				}))
				t.Cleanup(host.Close)
				coord.cfg.Config().Options.DisableAutoSummarize = true
				for _, id := range []string{"first", "second"} {
					provider, ok := coord.cfg.Config().Providers.Get(id)
					require.True(t, ok)
					provider.BaseURL = host.URL + "/v1"
					provider.Models[0].ContextWindow = 200000
					provider.Models[0].DefaultMaxTokens = 128
					coord.cfg.Config().Providers.Set(id, provider)
				}
				parent, err := env.sessions.Create(t.Context(), "parent")
				require.NoError(t, err)
				unrelated, err := env.sessions.Create(t.Context(), "unrelated")
				require.NoError(t, err)
				saved := coord.cfg.RuntimeSnapshot().AgentModelState()
				temperature := 0.17
				saved.Large.Model.MaxTokens = 321
				saved.Large.Model.Temperature = &temperature
				saveNotificationModels(t, coord, parent.ID, saved)
				owner, ok := coord.cfg.RuntimeSnapshot().ProviderOwner("second")
				require.True(t, ok)
				selected := &config.OwnedSelectedModel{Owner: owner, Model: config.SelectedModel{Provider: "second", Model: "second-model", MaxTokens: 456, Temperature: &temperature}}
				changed := config.AgentModelState{Large: selected, Small: selected}
				_, err = coord.cfg.OverrideModelsForOwners(changed)
				require.NoError(t, err)
				parentAgent := testSessionAgent(env, fastModel{}, fastModel{}, "shared default").(*sessionAgent)
				coord.currentAgent = parentAgent
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				receive := func() providerRequest {
					t.Helper()
					select {
					case request := <-requests:
						return request
					case <-ctx.Done():
						t.Fatal("expected provider request")
						return providerRequest{}
					}
				}
				runParent := func() error {
					call := SessionAgentCall{SessionID: parent.ID, Prompt: "continue", NonInteractive: true}
					runCtx, err := coord.bindSessionRuntime(ctx, &call)
					if err != nil {
						return err
					}
					_, err = parentAgent.Run(runCtx, call)
					return err
				}
				var done chan error
				if mode == "queued_step" {
					parentAgent.activeRequests.Set(parent.ID, &activeCancel{cancel: func() {}})
				} else if mode == "queued_turn" || mode == "queued_changed_parent" {
					done = make(chan error, 1)
					go func() { done <- runParent() }()
					require.Equal(t, "first-model", receive().Model)
				}
				var persisted, discarded atomic.Int32
				err = coord.DeliverTaskNotification(ctx, managedtask.Notification{
					ID: "task-completed", TaskID: "a12345678", TaskType: taskType,
					ParentSessionID: parent.ID, ToolUseID: "task-tool-call", Status: managedtask.StatusCompleted,
					FinalOutput: "Task finished",
				}, func() { persisted.Add(1) }, func() { discarded.Add(1) })
				require.NoError(t, err)
				if mode != "idle" {
					require.EqualValues(t, 0, persisted.Load())
				}
				if mode == "queued_changed_parent" {
					saveNotificationModels(t, coord, parent.ID, changed)
				}
				if mode == "queued_step" {
					parentAgent.activeRequests.Del(parent.ID)
					require.NoError(t, runParent())
				} else if done != nil {
					unblock()
					select {
					case err := <-done:
						require.NoError(t, err)
					case <-ctx.Done():
						t.Fatal("queued notification did not finish")
					}
				}
				request := receive()
				expectedProvider, expectedLimit := "first", int64(321)
				if mode == "queued_changed_parent" {
					expectedProvider, expectedLimit = "second", 456
				}
				require.Equal(t, expectedProvider+"-model", request.Model)
				require.Equal(t, "Bearer fixture-"+expectedProvider, request.Authorization)
				require.Equal(t, expectedLimit, request.MaxTokens)
				require.Equal(t, temperature, request.Temperature)
				require.Contains(t, fmt.Sprint(request.Messages), "<task-notification>")
				require.Contains(t, fmt.Sprint(request.Messages), "<task-id>a12345678</task-id>")
				require.Contains(t, fmt.Sprint(request.Messages), "<tool-use-id>task-tool-call</tool-use-id>")
				require.Contains(t, fmt.Sprint(request.Messages), "<result>Task finished</result>")
				require.EqualValues(t, 1, persisted.Load())
				require.EqualValues(t, 0, discarded.Load())
				messages, err := env.messages.List(ctx, unrelated.ID)
				require.NoError(t, err)
				require.Empty(t, messages)
				messages, err = env.messages.List(ctx, parent.ID)
				require.NoError(t, err)
				var lastAssistant message.Message
				for _, msg := range messages {
					if msg.Role == message.Assistant {
						lastAssistant = msg
					}
				}
				require.Equal(t, expectedProvider, lastAssistant.Provider)
				require.Equal(t, expectedProvider+"-model", lastAssistant.Model)
			})
		}
	}
}

func TestTaskNotificationRejectsUnavailableParentSelection(t *testing.T) {
	for _, mode := range []string{"missing", "changed_owner", "queued_changed_owner"} {
		t.Run(mode, func(t *testing.T) {
			coord, env := sessionRuntimeTestCoordinator(t)
			parent, err := env.sessions.Create(t.Context(), "parent")
			require.NoError(t, err)
			if mode == "changed_owner" {
				models := coord.cfg.RuntimeSnapshot().AgentModelState()
				models.Large.Owner.AccountNamespace = "unavailable-owner"
				saveNotificationModels(t, coord, parent.ID, models)
			}
			parentAgent := testSessionAgent(env, fastModel{}, fastModel{}, "shared default").(*sessionAgent)
			coord.currentAgent = parentAgent
			if mode == "queued_changed_owner" {
				saveNotificationModels(t, coord, parent.ID, coord.cfg.RuntimeSnapshot().AgentModelState())
				parentAgent.activeRequests.Set(parent.ID, &activeCancel{cancel: func() {}})
			}
			var persisted, discarded atomic.Int32
			err = coord.DeliverTaskNotification(t.Context(), managedtask.Notification{
				ID: "task-completed", TaskID: "a12345678", TaskType: managedtask.TypeAgent,
				ParentSessionID: parent.ID, Status: managedtask.StatusCompleted,
			}, func() { persisted.Add(1) }, func() { discarded.Add(1) })
			if mode == "queued_changed_owner" {
				require.NoError(t, err)
				models := coord.cfg.RuntimeSnapshot().AgentModelState()
				models.Large.Owner.AccountNamespace = "unavailable-owner"
				saveNotificationModels(t, coord, parent.ID, models)
				parentAgent.activeRequests.Del(parent.ID)
				queued := parentAgent.takeQueuedCallsLocked(parent.ID)
				require.Len(t, queued, 1)
				_, err = parentAgent.Run(t.Context(), queued[0])
			}
			var selectionErr *sessionModelSelectionError
			require.ErrorAs(t, err, &selectionErr)
			require.EqualValues(t, 0, persisted.Load())
			require.EqualValues(t, 1, discarded.Load())
			queued, _ := parentAgent.messageQueue.Get(parent.ID)
			require.Empty(t, queued)
			messages, err := env.messages.List(t.Context(), parent.ID)
			require.NoError(t, err)
			require.Empty(t, messages)
		})
	}
}
