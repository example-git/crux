package proto

import (
	"time"

	managedtask "github.com/example-git/crux/internal/task"
)

type Task = managedtask.View

type TaskOutput = managedtask.OutputResult

type TaskNotification = managedtask.Notification

type TaskOutputRequest struct {
	Wait    bool          `json:"wait,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

type TaskContinueRequest struct {
	ParentSessionID string `json:"parent_session_id"`
	Prompt          string `json:"prompt"`
}
