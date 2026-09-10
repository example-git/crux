package dialog

import (
	"strings"
	"time"

	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/common"
)

// NewPreviewTasks uses the real task dialog with already-loaded fixture data.
func NewPreviewTasks(com *common.Common, tasks []managedtask.View, detail bool) *Tasks {
	d := NewTasksPanel(com)
	// Freeze elapsed time so repeated previews render the same production frame.
	d.now = func() time.Time { return time.Unix(1788690045, 0) }
	d.tasks = tasks
	d.loading = false
	if detail && len(tasks) > 0 {
		d.mode = taskDialogDetail
		d.output = managedtask.OutputResult{Task: tasks[0], Output: tasks[0].FinalOutput, RetrievalStatus: "success"}
		if tasks[0].Type == managedtask.TypeAgent && tasks[0].FinalOutput != "" {
			d.messages = []message.Message{{ID: tasks[0].ID + "-result", Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: tasks[0].FinalOutput}, message.Finish{Reason: message.FinishReasonEndTurn}}}}
		}
		d.panelLines, d.panelMaxWidth = prepareTaskPanelOutput(d.output, d.messages)
		d.terminalFocused = true
	}
	return d
}

// NewPreviewCodebaseIndex loads a status snapshot without dispatching IO commands.
func NewPreviewCodebaseIndex(com *common.Common, status proto.CodebaseIndexStatus) *CodebaseIndex {
	d, _ := NewCodebaseIndex(com)
	d.status = status
	d.enabled = status.Enabled
	d.lastError = status.Error
	d.database.SetValue(status.DatabasePath)
	d.store.SetValue(status.StoreDirectory)
	d.include.SetValue(strings.Join(status.IncludePaths, ", "))
	d.exclude.SetValue(strings.Join(status.ExcludePaths, ", "))
	return d
}

// NewPreviewInstructionsContent primes the normal renderer with fixture sections.
func NewPreviewInstructionsContent(com *common.Common, sections []InstructionPreviewSection, width int) *InstructionsPreview {
	d := NewInstructionsPreview(com, sections, width)
	d.HandleMsg(d.renderCmd(d.contentWidth(width), true)())
	return d
}
