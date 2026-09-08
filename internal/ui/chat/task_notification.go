package chat

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/message"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/styles"
)

type taskNotificationContent struct {
	XMLName      xml.Name           `xml:"task-notification"`
	TaskID       string             `xml:"task-id"`
	TaskType     managedtask.Type   `xml:"task-type"`
	Status       managedtask.Status `xml:"status"`
	Summary      string             `xml:"summary"`
	Result       string             `xml:"result"`
	ErrorMessage string             `xml:"error-message"`
	LostReason   string             `xml:"lost-reason"`
	ExitCode     *int               `xml:"exit-code"`
}

type taskNotificationMessageItem struct {
	*baseToolMessageItem
}

type taskNotificationRenderContext struct {
	notification taskNotificationContent
}

func parseTaskNotificationMessage(msg *message.Message) (taskNotificationContent, bool) {
	if msg == nil {
		return taskNotificationContent{}, false
	}
	content := strings.TrimSpace(msg.Content().Text)
	if !strings.HasPrefix(content, "<task-notification>") {
		return taskNotificationContent{}, false
	}
	var notification taskNotificationContent
	if xml.Unmarshal([]byte(content), &notification) != nil || notification.XMLName.Local != "task-notification" || notification.TaskID == "" {
		return taskNotificationContent{}, false
	}
	return notification, true
}

func IsTaskNotificationMessage(msg *message.Message) bool {
	_, ok := parseTaskNotificationMessage(msg)
	return ok
}

func TaskNotificationTitle(msg *message.Message) (string, bool) {
	notification, ok := parseTaskNotificationMessage(msg)
	if !ok {
		return "", false
	}
	name := taskNotificationName(notification.TaskType)
	if notification.Status == "" {
		return name, true
	}
	return name + " · " + string(notification.Status), true
}

func newTaskNotificationMessageItem(sty *styles.Styles, msg *message.Message) (MessageItem, bool) {
	notification, ok := parseTaskNotificationMessage(msg)
	if !ok {
		return nil, false
	}
	result := strings.TrimSpace(notification.Result)
	if notification.TaskType == managedtask.TypeImage {
		if formatted, ok := FormatImagegenResult(result); ok {
			result = formatted
		}
	}
	diagnostics := taskResultDiagnostics(notification.ErrorMessage, notification.LostReason, notification.ExitCode)
	if diagnostics != "" {
		result = strings.TrimSpace(diagnostics + "\n" + result)
	}
	if result == "" {
		result = strings.TrimSpace(notification.Summary)
	}
	base := newBaseToolMessageItem(sty, message.ToolCall{
		ID: msg.ID + ":task-notification", Name: taskNotificationName(notification.TaskType), Finished: true,
	}, &message.ToolResult{ToolCallID: msg.ID, Content: result, IsError: notification.Status == managedtask.StatusFailed || notification.Status == managedtask.StatusLost}, &taskNotificationRenderContext{notification: notification}, notification.Status == managedtask.StatusKilled)
	base.SetMessageID(msg.ID)
	return &taskNotificationMessageItem{baseToolMessageItem: base}, true
}

func (t *taskNotificationRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	status := ToolStatusSuccess
	switch t.notification.Status {
	case managedtask.StatusFailed, managedtask.StatusLost:
		status = ToolStatusError
	case managedtask.StatusKilled:
		status = ToolStatusCanceled
	}
	name := taskNotificationName(t.notification.TaskType)
	header := toolHeader(sty, status, name, width, opts, t.notification.TaskID, "status", string(t.notification.Status))
	if opts.Compact || opts.Result.Content == "" {
		return header
	}
	body := toolOutputPlainContent(sty, opts.Result.Content, toolBodyWidth(sty, width), opts.ExpandedContent)
	return joinToolParts(header, sty.Tool.Body.Render(body))
}

func taskResultDiagnostics(errorMessage, lostReason string, exitCode *int) string {
	var lines []string
	if errorMessage = strings.TrimSpace(errorMessage); errorMessage != "" {
		lines = append(lines, errorMessage)
	}
	if lostReason = strings.TrimSpace(lostReason); lostReason != "" && lostReason != errorMessage {
		lines = append(lines, lostReason)
	}
	if exitCode != nil {
		lines = append(lines, fmt.Sprintf("Exit code: %d", *exitCode))
	}
	return strings.Join(lines, "\n")
}

func taskNotificationName(taskType managedtask.Type) string {
	switch taskType {
	case managedtask.TypeAgent:
		return "Background Agent"
	case managedtask.TypeShell:
		return "Background Shell"
	case managedtask.TypeImage:
		return "Background Image"
	default:
		return "Background Task"
	}
}
