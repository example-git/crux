package backend

import (
	"context"
	"time"

	managedtask "github.com/example-git/crux/internal/task"
)

func (b *Backend) ListTasks(ctx context.Context, workspaceID string) ([]managedtask.View, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	return ws.ListTasks(ctx)
}

func (b *Backend) TaskOutput(ctx context.Context, workspaceID, taskID string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return managedtask.OutputResult{}, err
	}
	return ws.TaskOutput(ctx, taskID, wait, timeout)
}

func (b *Backend) StopTask(ctx context.Context, workspaceID, taskID string) (managedtask.View, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return managedtask.View{}, err
	}
	return ws.StopTask(ctx, taskID)
}

func (b *Backend) ContinueTask(ctx context.Context, workspaceID, taskID, parentSessionID, prompt string) (managedtask.View, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return managedtask.View{}, err
	}
	return ws.ContinueTask(ctx, taskID, parentSessionID, prompt)
}

func (b *Backend) ListTaskNotifications(ctx context.Context, workspaceID, parentSessionID string, unreadOnly bool) ([]managedtask.Notification, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	return ws.ListTaskNotifications(ctx, parentSessionID, unreadOnly)
}

func (b *Backend) MarkTaskNotificationRead(ctx context.Context, workspaceID, notificationID string) (managedtask.Notification, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return managedtask.Notification{}, err
	}
	return ws.MarkTaskNotificationRead(ctx, notificationID)
}
