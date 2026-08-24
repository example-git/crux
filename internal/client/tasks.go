package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/example-git/crux/internal/proto"
	managedtask "github.com/example-git/crux/internal/task"
)

func (c *Client) ListTasks(ctx context.Context, workspaceID string) ([]managedtask.View, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/tasks", workspaceID), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	var tasks []managedtask.View
	if err := json.NewDecoder(rsp.Body).Decode(&tasks); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to decode tasks: %w", err)
	}
	return tasks, nil
}

func (c *Client) TaskOutput(ctx context.Context, workspaceID, taskID string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	body := jsonBody(proto.TaskOutputRequest{Wait: wait, Timeout: timeout})
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/tasks/%s/output", workspaceID, url.PathEscape(taskID)), nil, body, http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return managedtask.OutputResult{}, fmt.Errorf("failed to read task output: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return managedtask.OutputResult{}, fmt.Errorf("failed to read task output: %w", err)
	}
	var result managedtask.OutputResult
	if err := json.NewDecoder(rsp.Body).Decode(&result); err != nil {
		return managedtask.OutputResult{}, fmt.Errorf("failed to decode task output: %w", err)
	}
	result.Status = result.RetrievalStatus
	return result, nil
}

func (c *Client) StopTask(ctx context.Context, workspaceID, taskID string) (managedtask.View, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/tasks/%s/stop", workspaceID, url.PathEscape(taskID)), nil, nil, nil)
	if err != nil {
		return managedtask.View{}, fmt.Errorf("failed to stop task: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return managedtask.View{}, fmt.Errorf("failed to stop task: %w", err)
	}
	var result managedtask.View
	if err := json.NewDecoder(rsp.Body).Decode(&result); err != nil {
		return managedtask.View{}, fmt.Errorf("failed to decode stopped task: %w", err)
	}
	return result, nil
}

func (c *Client) ContinueTask(ctx context.Context, workspaceID, taskID, parentSessionID, prompt string) (managedtask.View, error) {
	body := jsonBody(proto.TaskContinueRequest{ParentSessionID: parentSessionID, Prompt: prompt})
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/tasks/%s/continue", workspaceID, url.PathEscape(taskID)), nil, body, http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return managedtask.View{}, fmt.Errorf("failed to continue task: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return managedtask.View{}, fmt.Errorf("failed to continue task: %w", err)
	}
	var result managedtask.View
	if err := json.NewDecoder(rsp.Body).Decode(&result); err != nil {
		return managedtask.View{}, fmt.Errorf("failed to decode continued task: %w", err)
	}
	return result, nil
}

func (c *Client) ListTaskNotifications(ctx context.Context, workspaceID, parentSessionID string, unreadOnly bool) ([]managedtask.Notification, error) {
	query := url.Values{}
	if parentSessionID != "" {
		query.Set("parent_session_id", parentSessionID)
	}
	query.Set("unread_only", strconv.FormatBool(unreadOnly))
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/task-notifications", workspaceID), query, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list task notifications: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to list task notifications: %w", err)
	}
	var notifications []managedtask.Notification
	if err := json.NewDecoder(rsp.Body).Decode(&notifications); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to decode task notifications: %w", err)
	}
	return notifications, nil
}

func (c *Client) MarkTaskNotificationRead(ctx context.Context, workspaceID, notificationID string) (managedtask.Notification, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/task-notifications/%s/read", workspaceID, url.PathEscape(notificationID)), nil, nil, nil)
	if err != nil {
		return managedtask.Notification{}, fmt.Errorf("failed to mark task notification read: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return managedtask.Notification{}, fmt.Errorf("failed to mark task notification read: %w", err)
	}
	var notification managedtask.Notification
	if err := json.NewDecoder(rsp.Body).Decode(&notification); err != nil {
		return managedtask.Notification{}, fmt.Errorf("failed to decode task notification: %w", err)
	}
	return notification, nil
}
