package chat

import (
	"fmt"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/ui/list"
	"github.com/example-git/crux/internal/ui/styles"
)

const ActivityHistoryLimit = 20

const activityHistoryStubID = "activity-history-stub"

type activityHistoryStubItem struct {
	*list.Versioned
	count int
	sty   *styles.Styles
}

func (a *activityHistoryStubItem) ID() string {
	return activityHistoryStubID
}

func (a *activityHistoryStubItem) Finished() bool {
	return true
}

func (a *activityHistoryStubItem) RawRender(int) string {
	return a.sty.Resource.AdditionalText.Render(fmt.Sprintf("… %d older agent/task activities hidden", a.count))
}

func (a *activityHistoryStubItem) Render(width int) string {
	return a.sty.Messages.ToolCallBlurred.Render(a.RawRender(width))
}

func CompactActivityHistory(sty *styles.Styles, items []MessageItem, limit int) []MessageItem {
	if limit < 0 {
		limit = 0
	}

	hidden := 0
	activityCount := 0
	stubIndex := -1
	for i, item := range items {
		if stub, ok := item.(*activityHistoryStubItem); ok {
			hidden += stub.count
			if stubIndex < 0 {
				stubIndex = i
			}
			continue
		}
		if IsAgentTaskActivity(item) && item.Finished() {
			activityCount++
		}
	}

	pruneCount := max(0, activityCount-limit)
	if pruneCount == 0 && hidden == 0 {
		return items
	}

	result := make([]MessageItem, 0, len(items)-pruneCount)
	remaining := pruneCount
	stubAdded := false
	for i, item := range items {
		if _, ok := item.(*activityHistoryStubItem); ok {
			if !stubAdded && i == stubIndex {
				result = append(result, &activityHistoryStubItem{Versioned: list.NewVersioned(), count: hidden + pruneCount, sty: sty})
				stubAdded = true
			}
			continue
		}
		if remaining > 0 && IsAgentTaskActivity(item) && item.Finished() {
			if !stubAdded && stubIndex < 0 {
				result = append(result, &activityHistoryStubItem{Versioned: list.NewVersioned(), count: hidden + pruneCount, sty: sty})
				stubAdded = true
			}
			remaining--
			continue
		}
		result = append(result, item)
	}
	return result
}

func IsAgentTaskActivity(item MessageItem) bool {
	if _, ok := item.(*taskNotificationMessageItem); ok {
		return true
	}
	toolItem, ok := item.(ToolMessageItem)
	if !ok {
		return false
	}
	switch toolItem.ToolCall().Name {
	case agent.AgentToolName,
		tools.TaskListToolName,
		tools.TaskOutputToolName,
		tools.TaskStopToolName,
		tools.TaskContinueToolName,
		tools.JobListToolName,
		tools.JobOutputToolName,
		tools.JobKillToolName:
		return true
	default:
		return false
	}
}
