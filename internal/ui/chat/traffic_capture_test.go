package chat

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestTrafficCaptureRendererShowsStructuralMetadata(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{
		ID:       "capture",
		Name:     tools.TrafficCaptureToolName,
		Input:    `{"executable":"example-cli","arguments":["run"]}`,
		Finished: true,
	}
	metadata, err := json.Marshal(tools.TrafficCaptureResponseMetadata{
		Session:     "crux-capture-test",
		CapturePath: "/tmp/capture.mitm",
		StatusPath:  "/tmp/status.json",
		PaneLogPath: "/tmp/pane.log",
		Attach:      "tmux attach",
		ViewerURL:   "http://127.0.0.1:8081/?token=secret",
	})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: call.ID, Content: "model-facing fallback", Metadata: string(metadata)}
	item := NewToolMessageItem(&sty, "message", call, result, false, "")
	view := ansi.Strip(item.Render(100))

	require.Contains(t, view, "Traffic Capture")
	require.Contains(t, view, "example-cli")
	require.Contains(t, view, "crux-capture-test")
	require.Contains(t, view, "/tmp/capture.mitm")
	require.Contains(t, view, "/tmp/status.json")
	require.Contains(t, view, "/tmp/pane.log")
	require.Contains(t, view, "http://127.0.0.1:8081/?token=secret")
	require.NotContains(t, view, "model-facing fallback")
}
