package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/example-git/crux/internal/proto"
)

func (c *controllerV1) handleGetWorkspaceTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := c.backend.ListTasks(r.Context(), r.PathValue("id"))
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, tasks)
}

func (c *controllerV1) handlePostWorkspaceTaskOutput(w http.ResponseWriter, r *http.Request) {
	var request proto.TaskOutputRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	if request.Timeout < 0 || request.Timeout > 10*time.Minute {
		jsonError(w, http.StatusBadRequest, "timeout must be between 0 and 10 minutes")
		return
	}
	result, err := c.backend.TaskOutput(r.Context(), r.PathValue("id"), r.PathValue("tid"), request.Wait, request.Timeout)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, result)
}

func (c *controllerV1) handlePostWorkspaceTaskStop(w http.ResponseWriter, r *http.Request) {
	result, err := c.backend.StopTask(r.Context(), r.PathValue("id"), r.PathValue("tid"))
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, result)
}

func (c *controllerV1) handlePostWorkspaceTaskContinue(w http.ResponseWriter, r *http.Request) {
	var request proto.TaskContinueRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	if request.ParentSessionID == "" || request.Prompt == "" {
		jsonError(w, http.StatusBadRequest, "parent_session_id and prompt are required")
		return
	}
	result, err := c.backend.ContinueTask(r.Context(), r.PathValue("id"), r.PathValue("tid"), request.ParentSessionID, request.Prompt)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, result)
}

func (c *controllerV1) handleGetWorkspaceTaskNotifications(w http.ResponseWriter, r *http.Request) {
	unreadOnly := false
	if value := r.URL.Query().Get("unread_only"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "unread_only must be a boolean")
			return
		}
		unreadOnly = parsed
	}
	notifications, err := c.backend.ListTaskNotifications(r.Context(), r.PathValue("id"), r.URL.Query().Get("parent_session_id"), unreadOnly)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, notifications)
}

func (c *controllerV1) handlePostWorkspaceTaskNotificationRead(w http.ResponseWriter, r *http.Request) {
	notification, err := c.backend.MarkTaskNotificationRead(r.Context(), r.PathValue("id"), r.PathValue("nid"))
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, notification)
}
