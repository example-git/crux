package model

import (
	"context"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type activityWorkspace struct {
	workspace.Workspace
	status proto.CodebaseIndexStatus
	err    error
}

func (w *activityWorkspace) CodebaseIndexStatus(context.Context) (proto.CodebaseIndexStatus, error) {
	return w.status, w.err
}

func TestActivityStatusLabel(t *testing.T) {
	t.Parallel()

	require.Equal(t, "indexing 12/40", activityStatusLabel(proto.CodebaseIndexStatus{
		State:          "indexing",
		FilesProcessed: 12,
		FilesTotal:     40,
	}))
	require.Equal(t, "indexing embedding  memory consolidating", activityStatusLabel(proto.CodebaseIndexStatus{
		State:          "indexing",
		Stage:          "embedding",
		MemoryActivity: "consolidating",
	}))
	require.Equal(t, "index ready  memory updating", activityStatusLabel(proto.CodebaseIndexStatus{
		State:          "ready",
		MemoryActivity: "updating",
	}))
	require.Equal(t, "index ready", activityStatusLabel(proto.CodebaseIndexStatus{State: "ready"}))
}

func TestActivityRefreshFetchesOffThreadAndPollsWhileActive(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.com.Workspace = &activityWorkspace{status: proto.CodebaseIndexStatus{
		State:          "indexing",
		FilesProcessed: 4,
		FilesTotal:     10,
		MemoryActivity: "updating",
	}}

	cmd := ui.requestActivityRefresh()
	require.NotNil(t, cmd)
	require.True(t, ui.activityFetchInFlight)
	msg, ok := cmd().(activityStatusMsg)
	require.True(t, ok)
	next := ui.applyActivityStatus(msg)
	require.False(t, ui.activityFetchInFlight)
	require.Equal(t, 4, ui.activityStatus.FilesProcessed)
	require.NotNil(t, next)
}

func TestActivityRefreshKeepsIdleBackstopAndPreservesStateOnError(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	next := ui.applyActivityStatus(activityStatusMsg{status: proto.CodebaseIndexStatus{State: "ready"}})
	require.NotNil(t, next)

	ui.activityStatus = proto.CodebaseIndexStatus{MemoryActivity: "updating"}
	next = ui.applyActivityStatus(activityStatusMsg{err: context.DeadlineExceeded})
	require.NotNil(t, next)
	require.Equal(t, "updating", ui.activityStatus.MemoryActivity)
}

func TestEditorAccentUsesProviderGradientWithThemeFallback(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	fallback := ui.com.Styles.Logo.TitleColorB
	require.Equal(t, fallback, ui.editorAccent())

	providerAccent := lipgloss.Color("#123456")
	ui.brand = &providerBrand{GradB: providerAccent}
	require.Equal(t, providerAccent, ui.editorAccent())
}

func TestEditorFrameKeepsStatusAtTopRight(t *testing.T) {
	t.Parallel()

	line := renderEditorFrameLine(30, "attachment", "memory updating", lipgloss.Color("#123456"))
	require.Equal(t, 30, ansi.StringWidth(line))
	require.Equal(t, "attachment──── memory updating", ansi.Strip(line))
	require.True(t, strings.HasSuffix(ansi.Strip(line), "memory updating"))
}

func TestEditorFrameUsesExactEditorWidthWithAndWithoutSidebar(t *testing.T) {
	t.Parallel()

	for _, compact := range []bool{false, true} {
		ui := newTestUI()
		ui.state = uiChat
		ui.isCompact = compact
		ui.activityStatus = proto.CodebaseIndexStatus{State: "ready"}
		layout := ui.generateLayout(120, 40)
		line := strings.Split(ui.renderEditorView(layout.editor.Dx()), "\n")[0]

		require.Equal(t, layout.editor.Dx(), ansi.StringWidth(line))
		require.True(t, strings.HasSuffix(ansi.Strip(line), "index ready"))
	}
}

func TestEditorBodyUsesDistinctBackgroundAndFillsWidth(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	require.NotEqual(t, ui.com.Styles.Background, ui.com.Styles.Editor.Background)

	body := paintEditorBody("one\ntwo", 12, ui.com.Styles.Editor.Background)
	lines := strings.Split(body, "\n")
	require.Len(t, lines, 2)
	require.Equal(t, 12, ansi.StringWidth(lines[0]))
	require.Equal(t, 12, ansi.StringWidth(lines[1]))
}

func TestRenderEditorViewIntegratesFrameBackgroundAndNotifier(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.activityStatus = proto.CodebaseIndexStatus{
		State:          "indexing",
		FilesProcessed: 8,
		FilesTotal:     20,
		MemoryActivity: "updating",
	}
	view := ui.renderEditorView(44)
	lines := strings.Split(view, "\n")
	require.Len(t, lines, ui.textarea.Height()+2)
	for _, line := range lines {
		require.Equal(t, 44, ansi.StringWidth(line))
	}
	require.Contains(t, ansi.Strip(lines[0]), "indexing 8/20  memory updating")
	require.Equal(t, strings.Repeat("─", 44), ansi.Strip(lines[len(lines)-1]))
}
