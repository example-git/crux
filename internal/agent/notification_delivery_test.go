package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
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
	env := testEnv(t)
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	parentAgent := testSessionAgent(env, fastModel{}, fastModel{}, "system").(*sessionAgent)
	parentAgent.messages = notificationFailingMessageService{Service: env.messages}
	coordinator := &coordinator{currentAgent: parentAgent}
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
	env := testEnv(t)
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	model := &notificationFailingModel{entered: make(chan struct{}, 1), release: make(chan struct{})}
	parentAgent := testSessionAgent(env, model, fastModel{}, "system").(*sessionAgent)
	coordinator := &coordinator{currentAgent: parentAgent}
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

type notificationRecordingModel struct {
	fastModel
	calls chan fantasy.Call
}

func (m *notificationRecordingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls <- call
	return m.fastModel.Stream(ctx, call)
}

func TestImageNotificationReachesOriginatingModel(t *testing.T) {
	for _, mode := range []string{"idle", "queued"} {
		t.Run(mode, func(t *testing.T) {
			env := testEnv(t)
			parent, err := env.sessions.Create(t.Context(), "parent")
			require.NoError(t, err)
			unrelated, err := env.sessions.Create(t.Context(), "unrelated")
			require.NoError(t, err)
			model := &notificationRecordingModel{calls: make(chan fantasy.Call, 4)}
			parentAgent := testSessionAgent(env, model, fastModel{}, "system").(*sessionAgent)
			coordinator := &coordinator{currentAgent: parentAgent}
			if mode == "queued" {
				parentAgent.activeRequests.Set(parent.ID, &activeCancel{cancel: func() {}})
			}
			var persisted, discarded atomic.Int32
			err = coordinator.DeliverTaskNotification(t.Context(), managedtask.Notification{
				ID: "image-completed", TaskID: "i12345678", TaskType: managedtask.TypeImage,
				ParentSessionID: parent.ID, ToolUseID: "image-tool-call", Status: managedtask.StatusCompleted,
				FinalOutput: "Generated image: result.png",
			}, func() { persisted.Add(1) }, func() { discarded.Add(1) })
			require.NoError(t, err)
			if mode == "queued" {
				require.EqualValues(t, 0, persisted.Load())
				parentAgent.activeRequests.Del(parent.ID)
				_, err = parentAgent.Run(t.Context(), SessionAgentCall{SessionID: parent.ID, Prompt: "continue", NonInteractive: true})
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, persisted.Load())
			require.EqualValues(t, 0, discarded.Load())
			select {
			case call := <-model.calls:
				var text strings.Builder
				for _, promptMessage := range call.Prompt {
					for _, part := range promptMessage.Content {
						if content, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
							text.WriteString(content.Text)
						}
					}
				}
				require.Contains(t, text.String(), "<task-notification>")
				require.Contains(t, text.String(), "<task-id>i12345678</task-id>")
				require.Contains(t, text.String(), "<tool-use-id>image-tool-call</tool-use-id>")
				require.Contains(t, text.String(), "<result>Generated image: result.png</result>")
			default:
				t.Fatal("notification did not reach the model")
			}
			messages, err := env.messages.List(t.Context(), unrelated.ID)
			require.NoError(t, err)
			require.Empty(t, messages)
		})
	}
}

func TestNotificationSessionLoadFailureReleasesDelivery(t *testing.T) {
	env := testEnv(t)
	parentAgent := testSessionAgent(env, fastModel{}, fastModel{}, "system").(*sessionAgent)
	coordinator := &coordinator{currentAgent: parentAgent}
	var persisted, discarded atomic.Int32
	err := coordinator.DeliverTaskNotification(t.Context(), managedtask.Notification{
		ID: "image-completed", TaskID: "i12345678", TaskType: managedtask.TypeImage,
		ParentSessionID: "missing-session", Status: managedtask.StatusCompleted,
	}, func() { persisted.Add(1) }, func() { discarded.Add(1) })
	require.ErrorContains(t, err, "failed to get session")
	require.EqualValues(t, 0, persisted.Load())
	require.EqualValues(t, 1, discarded.Load())
}
