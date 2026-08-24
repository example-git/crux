package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/example-git/crux/internal/agent"
	managedtask "github.com/example-git/crux/internal/task"
)

func (app *App) taskCoordinator() (agent.TaskCoordinator, error) {
	coordinator, ok := app.AgentCoordinator.(agent.TaskCoordinator)
	if !ok {
		return nil, fmt.Errorf("managed task service is unavailable")
	}
	return coordinator, nil
}

func (app *App) ListTasks(context.Context) ([]managedtask.View, error) {
	coordinator, err := app.taskCoordinator()
	if err != nil {
		return nil, err
	}
	return coordinator.ListTasks(), nil
}

func (app *App) TaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	coordinator, err := app.taskCoordinator()
	if err != nil {
		return managedtask.OutputResult{}, err
	}
	return coordinator.TaskOutput(ctx, id, wait, timeout)
}

func (app *App) StopTask(ctx context.Context, id string) (managedtask.View, error) {
	coordinator, err := app.taskCoordinator()
	if err != nil {
		return managedtask.View{}, err
	}
	return coordinator.StopTask(ctx, id)
}

func (app *App) ContinueTask(ctx context.Context, id, parentSessionID, prompt string) (managedtask.View, error) {
	coordinator, err := app.taskCoordinator()
	if err != nil {
		return managedtask.View{}, err
	}
	return coordinator.ContinueTask(ctx, id, parentSessionID, prompt, "")
}

func (app *App) ListTaskNotifications(_ context.Context, parentSessionID string, unreadOnly bool) ([]managedtask.Notification, error) {
	return app.TaskStore.ListNotifications(app.config.WorkingDir(), parentSessionID, unreadOnly, false)
}

func (app *App) MarkTaskNotificationRead(_ context.Context, notificationID string) (managedtask.Notification, error) {
	return app.TaskStore.MarkNotificationRead(notificationID)
}

func (app *App) startTaskNotificationDelivery() {
	app.taskNotificationsOnce.Do(func() {
		ctx := app.eventsCtx
		shellNotifications := app.BackgroundShells.SubscribeNotifications(ctx)
		agentNotifications := app.BackgroundAgents.SubscribeNotifications(ctx)
		app.serviceEventsWG.Go(func() {
			pending, err := app.TaskStore.ListNotifications(app.config.WorkingDir(), "", false, true)
			if err != nil {
				slog.Error("Failed to load pending task notifications", "error", err)
			}
			for _, notification := range pending {
				app.deliverTaskNotification(ctx, notification)
			}
			for {
				select {
				case event, ok := <-shellNotifications:
					if !ok {
						shellNotifications = nil
						continue
					}
					app.deliverTaskNotification(ctx, event.Payload)
				case event, ok := <-agentNotifications:
					if !ok {
						agentNotifications = nil
						continue
					}
					app.deliverTaskNotification(ctx, event.Payload)
				case <-ctx.Done():
					return
				}
			}
		})
	})
}

func (app *App) beginTaskNotificationDelivery(notificationID string) bool {
	app.taskNotificationDeliveryMu.Lock()
	defer app.taskNotificationDeliveryMu.Unlock()
	if _, ok := app.taskNotificationDeliveries[notificationID]; ok {
		return false
	}
	if app.taskNotificationDeliveries == nil {
		app.taskNotificationDeliveries = make(map[string]struct{})
	}
	app.taskNotificationDeliveries[notificationID] = struct{}{}
	return true
}

func (app *App) finishTaskNotificationDelivery(notificationID string) {
	app.taskNotificationDeliveryMu.Lock()
	delete(app.taskNotificationDeliveries, notificationID)
	app.taskNotificationDeliveryMu.Unlock()
}

func (app *App) retryTaskNotification(ctx context.Context, notification managedtask.Notification) {
	app.finishTaskNotificationDelivery(notification.ID)
	time.AfterFunc(time.Second, func() {
		if ctx.Err() == nil {
			app.deliverTaskNotification(ctx, notification)
		}
	})
}

func (app *App) deliverTaskNotification(ctx context.Context, notification managedtask.Notification) {
	record, err := app.TaskStore.Get(notification.TaskID)
	if err != nil || record.Notification == nil || !record.Notification.ModelDeliveredAt.IsZero() {
		return
	}
	notification = *record.Notification
	if !app.beginTaskNotificationDelivery(notification.ID) {
		return
	}
	coordinator, err := app.taskCoordinator()
	if err != nil {
		app.retryTaskNotification(ctx, notification)
		return
	}
	err = coordinator.DeliverTaskNotification(ctx, notification, func() {
		if _, markErr := app.TaskStore.MarkNotificationDelivered(notification.ID); markErr != nil {
			slog.Error("Failed to mark task notification delivered", "notification_id", notification.ID, "error", markErr)
			app.retryTaskNotification(ctx, notification)
			return
		}
		app.finishTaskNotificationDelivery(notification.ID)
	}, func() {
		app.retryTaskNotification(ctx, notification)
	})
	if err != nil {
		slog.Error("Failed to deliver task notification", "notification_id", notification.ID, "error", err)
		app.retryTaskNotification(ctx, notification)
	}
}
