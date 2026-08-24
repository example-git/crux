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

func newTaskNotificationMessageItem(sty *styles.Styles, msg *message.Message) (MessageItem, bool) {
	content := strings.TrimSpace(msg.Content().Text)
	if !strings.HasPrefix(content, "<task-notification>") {
		return nil, false
	}
	var notification taskNotificationContent
	if xml.Unmarshal([]byte(content), &notification) != nil || notification.XMLName.Local != "task-notification" || notification.TaskID == "" {
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
	name := "Background Task"
	switch t.notification.TaskType {
	case managedtask.TypeAgent:
		name = "Background Agent"
	case managedtask.TypeShell:
		name = "Background Shell"
	}
	contentWidth := cappedMessageWidth(width)
	header := toolHeader(t.sty, status, name, contentWidth, &ToolRenderOpts{Status: status}, t.notification.TaskID, "status", string(t.notification.Status))
	result := strings.TrimSpace(t.notification.Result)
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
