package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestForegroundNativeKeyDetachesBash(t *testing.T) {
	preview, err := NewPreview()
	require.NoError(t, err)
	_, err = preview.Render(PreviewOptions{Cols: 65, Rows: 25, Model: "dummy-coder", Scenario: "working", Example: "tool-bash"})
	require.NoError(t, err)
	ui := preview.ui
	application := app.NewForTest(t.Context())
	defer application.ShutdownForTest()
	application.Permissions.SetSkipRequests(true)
	ui.com.Workspace = workspace.NewAppWorkspace(application, nil)
	tool := tools.NewBashTool(application.BackgroundShells, application.Permissions, t.TempDir())
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := context.WithValue(parent, tools.SessionIDContextKey, ui.session.ID)
	type result struct {
		response fantasy.ToolResponse
		err      error
	}
	results := make(chan result, 1)
	go func() {
		response, err := tool.Run(ctx, fantasy.ToolCall{ID: "native-detach", Name: tools.BashToolName, Input: `{"command":"sleep 2; echo native-detach-done","description":"native detach","timeout":1}`})
		results <- result{response, err}
	}()
	require.Eventually(t, func() bool { return application.BackgroundShells.ForegroundWaits.Count(ui.session.ID) == 1 }, time.Second, time.Millisecond)
	count, err := ui.com.Workspace.(*workspace.AppWorkspace).ForegroundTaskControl(t.Context(), ui.session.ID, false)
	require.NoError(t, err)
	ui.applyTaskStatus(taskStatusMsg{sessionID: ui.session.ID, foregroundCount: count})
	require.True(t, ui.canDetachForeground())
	ui.foregroundWaitCount = 0
	ui.agentBusyCache.set(false)
	cmd := ui.handleKeyPressMsg(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	info, ok := cmd().(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeSuccess, info.Type)
	require.False(t, ui.canDetachForeground())
	var response result
	select {
	case response = <-results:
	case <-time.After(time.Second):
		t.Fatal("Ctrl+B did not return Bash response")
	}
	require.NoError(t, response.err)
	var metadata tools.BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.response.Metadata), &metadata))
	require.True(t, metadata.Background)
	backgroundShell, ok := application.BackgroundShells.Get(metadata.ShellID)
	require.True(t, ok)
	cancel()
	require.Eventually(t, func() bool { return backgroundShell.State().Status.Terminal() }, 4*time.Second, 10*time.Millisecond)
	stdout, _, _, runErr := backgroundShell.GetOutput()
	require.NoError(t, runErr)
	require.Contains(t, stdout, "native-detach-done")
}

func TestForegroundPreviewFooter(t *testing.T) {
	for _, cols := range []int{45, 65, 160} {
		for _, available := range []bool{false, true} {
			preview, err := NewPreview()
			require.NoError(t, err)
			count := 0
			if available {
				count = 1
			}
			data, err := json.Marshal(map[string]int{"foregroundWaitCount": count})
			require.NoError(t, err)
			frame, err := preview.Render(PreviewOptions{Cols: cols, Rows: 25, Model: "dummy-coder", Scenario: "working", Example: "tool-bash", Data: data})
			require.NoError(t, err)
			if available {
				require.Contains(t, ansi.Strip(frame.Content), "ctrl+b background")
			} else {
				require.NotContains(t, ansi.Strip(frame.Content), "ctrl+b background")
			}
		}
	}
}
