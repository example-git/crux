package app

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/imagegen"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

type notificationTestCoordinator struct {
	agent.Coordinator
	agent.TaskCoordinator
	deliver func(context.Context, managedtask.Notification, func(), func()) error
}

func (c *notificationTestCoordinator) DeliverTaskNotification(ctx context.Context, notification managedtask.Notification, persisted, discarded func()) error {
	return c.deliver(ctx, notification, persisted, discarded)
}

func notificationTestApp(t *testing.T, deliver func(context.Context, managedtask.Notification, func(), func()) error) *App {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_CACHE_DIR", t.TempDir())
	cfg, err := config.Init(root, "", false)
	require.NoError(t, err)
	store, err := managedtask.NewStore(filepath.Join(root, "records"))
	require.NoError(t, err)
	images, err := imagegen.NewJobManagerWithStore(root, store, imagegen.JobManagerOptions{
		MaxConcurrent: 1, MaxQueued: 4,
		Executor: func(context.Context, imagegen.JobRequest) (*imagegen.Response, error) {
			return &imagegen.Response{Data: []imagegen.ImageData{{B64JSON: base64.StdEncoding.EncodeToString([]byte("image"))}}}, nil
		},
	})
	require.NoError(t, err)
	shells := shell.NewBackgroundShellManager(root)
	agents := agent.NewBackgroundAgentManager(root, shells)
	ctx, cancel := context.WithCancel(t.Context())
	app := &App{
		config: cfg, TaskStore: store, BackgroundImages: images,
		BackgroundShells: shells, BackgroundAgents: agents,
		AgentCoordinator: &notificationTestCoordinator{deliver: deliver},
		eventsCtx:        ctx, serviceEventsWG: &sync.WaitGroup{},
	}
	t.Cleanup(func() {
		cancel()
		images.StopAll(context.Background())
		agents.StopAll(context.Background())
		app.serviceEventsWG.Wait()
		require.NoError(t, store.Close())
	})
	app.startTaskNotificationDelivery()
	return app
}

func enqueueNotificationTestImage(t *testing.T, app *App, name string) managedtask.View {
	t.Helper()
	view, err := app.BackgroundImages.Enqueue(imagegen.JobRequest{
		Mode: imagegen.ModeGenerate, Prompt: name, Count: 1,
		OutputPaths: []string{filepath.Join(app.config.WorkingDir(), name+".png")},
	}, name, managedtask.Ownership{ParentSessionID: "parent", OriginToolCallID: name})
	require.NoError(t, err)
	return view
}

func TestImageNotificationsContinueWhileEarlierDeliveryRuns(t *testing.T) {
	entered := make(chan managedtask.Notification, 4)
	app := notificationTestApp(t, func(ctx context.Context, notification managedtask.Notification, persisted, _ func()) error {
		persisted()
		entered <- notification
		if notification.ToolUseID == "first" {
			<-ctx.Done()
		}
		return nil
	})
	first := enqueueNotificationTestImage(t, app, "first")
	select {
	case notification := <-entered:
		require.Equal(t, first.ID, notification.TaskID)
	case <-time.After(3 * time.Second):
		t.Fatal("first image completion was not delivered")
	}
	second := enqueueNotificationTestImage(t, app, "second")
	select {
	case notification := <-entered:
		require.Equal(t, second.ID, notification.TaskID)
		require.Equal(t, "parent", notification.ParentSessionID)
		require.Equal(t, managedtask.StatusCompleted, notification.Status)
		require.Contains(t, notification.FinalOutput, "second.png")
	case <-time.After(time.Second):
		t.Fatal("second image completion was blocked by the first model run")
	}
}

func TestTaskNotificationDeliveryRecoversMissingEvent(t *testing.T) {
	entered := make(chan managedtask.Notification, 4)
	app := notificationTestApp(t, func(_ context.Context, notification managedtask.Notification, persisted, _ func()) error {
		persisted()
		entered <- notification
		return nil
	})
	first := enqueueNotificationTestImage(t, app, "first")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("initial delivery did not finish")
	}
	record, err := app.TaskStore.Get(first.ID)
	require.NoError(t, err)
	id, err := managedtask.NewID(managedtask.TypeImage)
	require.NoError(t, err)
	record.ID = id
	record.Notification.ID = "notification-" + id
	record.Notification.TaskID = id
	record.Notification.ModelDeliveredAt = time.Time{}
	record.Notification.ToolUseID = "missed-event"
	require.NoError(t, app.TaskStore.Put(record))
	select {
	case notification := <-entered:
		require.Equal(t, id, notification.TaskID)
	case <-time.After(3 * time.Second):
		t.Fatal("persisted completion without a live event was never delivered")
	}
	stored, err := app.TaskStore.Get(id)
	require.NoError(t, err)
	require.False(t, stored.Notification.ModelDeliveredAt.IsZero())
	for range 32 {
		app.deliverTaskNotification(t.Context(), *record.Notification)
	}
	select {
	case duplicate := <-entered:
		t.Fatalf("notification delivered twice: %s", duplicate.ID)
	case <-time.After(1100 * time.Millisecond):
	}
}

func TestTaskNotificationStaleErrorDoesNotReleaseRetry(t *testing.T) {
	type attempt struct {
		notification managedtask.Notification
		persisted    func()
	}
	entered := make(chan attempt, 8)
	returnError := make(chan struct{})
	var first sync.Once
	app := notificationTestApp(t, func(ctx context.Context, notification managedtask.Notification, persisted, discarded func()) error {
		isFirst := false
		first.Do(func() { isFirst = true })
		if isFirst {
			discarded()
			entered <- attempt{notification: notification, persisted: persisted}
			select {
			case <-returnError:
			case <-ctx.Done():
			}
			return errors.New("error after discard")
		}
		entered <- attempt{notification: notification, persisted: persisted}
		return nil
	})
	enqueueNotificationTestImage(t, app, "retry")
	var retry attempt
	for range 2 {
		select {
		case retry = <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("discarded notification was not retried")
		}
	}
	close(returnError)
	select {
	case <-entered:
		t.Fatal("old attempt released an already queued retry")
	case <-time.After(1100 * time.Millisecond):
	}
	retry.persisted()
	record, err := app.TaskStore.Get(retry.notification.TaskID)
	require.NoError(t, err)
	require.False(t, record.Notification.ModelDeliveredAt.IsZero())
}
