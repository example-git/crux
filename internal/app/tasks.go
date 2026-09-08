package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/example-git/crux/internal/agent"
	managedtask "github.com/example-git/crux/internal/task"
)

func (app *App) taskCoordinator() (agent.TaskCoordinator, error) {
	coordinator, ok := app.CurrentAgentCoordinator().(agent.TaskCoordinator)
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

func (app *App) RestartTask(ctx context.Context, id string) (managedtask.View, error) {
	coordinator, err := app.taskCoordinator()
	if err != nil {
		return managedtask.View{}, err
	}
	return coordinator.RestartTask(ctx, id)
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
		imageNotifications := app.BackgroundImages.SubscribeNotifications(ctx)
		app.serviceEventsWG.Go(func() {
			reconcile := func() {
				pending, err := app.TaskStore.ListNotifications(app.config.WorkingDir(), "", false, true)
				if err != nil {
					slog.Error("Failed to load pending task notifications", "error", err)
					return
				}
				for _, notification := range pending {
					app.deliverTaskNotification(ctx, notification)
				}
			}
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			reconcile()
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
				case event, ok := <-imageNotifications:
					if !ok {
						imageNotifications = nil
						continue
					}
					app.deliverTaskNotification(ctx, event.Payload)
				case <-ticker.C:
					reconcile()
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

func (app *App) deliverTaskNotification(ctx context.Context, notification managedtask.Notification) {
	if ctx.Err() != nil || !app.beginTaskNotificationDelivery(notification.ID) {
		return
	}
	record, err := app.TaskStore.Get(notification.TaskID)
	if err != nil || record.Notification == nil || record.Notification.ID != notification.ID || !record.Notification.ModelDeliveredAt.IsZero() {
		app.finishTaskNotificationDelivery(notification.ID)
		return
	}
	notification = *record.Notification
	app.serviceEventsWG.Go(func() {
		var settled sync.Once
		discarded := func() {
			settled.Do(func() {
				app.finishTaskNotificationDelivery(notification.ID)
			})
		}
		if ctx.Err() != nil {
			discarded()
			return
		}
		coordinator, err := app.taskCoordinator()
		if err != nil {
			discarded()
			return
		}
		err = coordinator.DeliverTaskNotification(ctx, notification, func() {
			settled.Do(func() {
				if _, markErr := app.TaskStore.MarkNotificationDelivered(notification.ID); markErr != nil {
					slog.Error("Failed to mark task notification delivered", "notification_id", notification.ID, "error", markErr)
				}
				app.finishTaskNotificationDelivery(notification.ID)
			})
		}, discarded)
		if err != nil {
			slog.Error("Failed to deliver task notification", "notification_id", notification.ID, "error", err)
			discarded()
		}
	})
}
