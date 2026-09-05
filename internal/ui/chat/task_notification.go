package chat

import (
	"encoding/xml"
	"strings"

	"github.com/example-git/crux/internal/message"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/list"
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
}

type taskNotificationMessageItem struct {
	*list.Versioned
	id           string
	notification taskNotificationContent
	sty          *styles.Styles
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
	return &taskNotificationMessageItem{
		Versioned:    list.NewVersioned(),
		id:           msg.ID + ":task-notification",
		notification: notification,
		sty:          sty,
	}, true
}

func (t *taskNotificationMessageItem) ID() string {
	return t.id
}

func (t *taskNotificationMessageItem) Finished() bool {
	return true
}

func (t *taskNotificationMessageItem) RawRender(width int) string {
	status := ToolStatusSuccess
	switch t.notification.Status {
	case managedtask.StatusFailed, managedtask.StatusLost:
		status = ToolStatusError
	case managedtask.StatusKilled:
		status = ToolStatusCanceled
	}
	name := taskNotificationName(t.notification.TaskType)
	contentWidth := cappedMessageWidth(width)
	header := toolHeader(t.sty, status, name, contentWidth, &ToolRenderOpts{Status: status}, t.notification.TaskID, "status", string(t.notification.Status))
	result := strings.TrimSpace(t.notification.Result)
	if t.notification.TaskType == managedtask.TypeImage {
		if formatted, ok := FormatImagegenResult(result); ok {
			result = formatted
		}
	}
	if result == "" {
		result = strings.TrimSpace(t.notification.ErrorMessage)
	}
	if result == "" {
		result = strings.TrimSpace(t.notification.LostReason)
	}
	if result == "" {
		result = strings.TrimSpace(t.notification.Summary)
	}
	if result == "" {
		return header
	}
	body := toolOutputPlainContent(t.sty, result, max(0, contentWidth-toolBodyLeftPaddingTotal), false)
	return joinToolParts(header, body)
}

func (t *taskNotificationMessageItem) Render(width int) string {
	return t.sty.Messages.ToolCallBlurred.Render(t.RawRender(width))
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
