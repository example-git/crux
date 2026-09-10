package model

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/stretchr/testify/require"
)

func sidebarSessionPreview(t *testing.T, data map[string]any, rows int) *UI {
	t.Helper()
	p, err := NewPreview()
	require.NoError(t, err)
	encoded, err := json.Marshal(data)
	require.NoError(t, err)
	_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "idle", Cols: 120, Rows: rows, Data: encoded})
	require.NoError(t, err)
	return p.ui
}

func TestSidebarSessionLayout(t *testing.T) {
	for _, address := range []string{"", "192.168.1.117:14995"} {
		for _, rows := range []int{100, 30} {
			m := sidebarSessionPreview(t, map[string]any{"directory": "/mnt/project", "remoteAddress": address, "session": map[string]any{"Title": "Session title"}, "authority": map[string]any{"mode": "client", "revision": 2}}, rows)
			frame := sidebarFrameText(m)
			require.NotContains(t, frame, "Workspace Authority")
			require.Less(t, strings.Index(frame, "Model / Context"), strings.Index(frame, "Session"))
			require.Less(t, strings.Index(frame, "Session"), strings.Index(frame, "Modified Files"))
			require.Contains(t, frame, "ID: "+session.ShortID(m.session.ID))
			require.NotEmpty(t, m.sidebarSession.idRect)
			lines := strings.Split(ansi.Strip(m.sidebarContent), "\n")
			header := m.sidebarSectionHeaders["session"]
			body := strings.Split(trimSidebarBlankLines(strings.Join(lines[header+1:m.sidebarFilesHeaderLine], "\n")), "\n")
			require.Equal(t, "/mnt/project", strings.TrimSpace(body[0]))
			if address == "" {
				require.NotContains(t, frame, "Remote:")
			} else {
				require.Equal(t, "Remote: "+address, strings.TrimSpace(body[1]))
			}
			if rows == 100 {
				view := m.View()
				screen := uv.NewScreenBuffer(m.width, m.height)
				uv.NewStyledString(view.Content).Draw(screen, screen.Bounds())
				for y := m.layout.sidebar.Min.Y; y < m.layout.sidebar.Max.Y; y++ {
					line := ansi.Strip(ansi.Cut(strings.Split(view.Content, "\n")[y], m.layout.sidebar.Min.X+2, m.layout.sidebar.Max.X-2))
					for _, part := range []struct {
						text string
						bold bool
					}{{"/mnt/project", true}, {"Session title", true}, {"Remote:", true}, {address, false}} {
						if part.text == "" {
							continue
						}
						if x := strings.Index(line, part.text); x >= 0 {
							for i := range len(part.text) {
								require.Equal(t, part.bold, screen.CellAt(m.layout.sidebar.Min.X+2+x+i, y).Style.Attrs&uv.AttrBold != 0, part.text)
							}
						}
					}
				}
			}
		}
	}
}

func TestSidebarSessionDebugAndUnassignedID(t *testing.T) {
	m := sidebarSessionPreview(t, map[string]any{"settings": map[string]any{"debug": true}, "authority": map[string]any{"mode": "client", "revision": 2}}, 100)
	require.Contains(t, sidebarFrameText(m), "Workspace Authority")
	require.Greater(t, m.sidebarSectionHeaders["authority"], m.sidebarSectionHeaders["session"])
	for _, id := range []string{"", "   "} {
		m.session.ID = id
		m.updateSidebarScrollState()
		require.NotContains(t, ansi.Strip(m.sidebarContent), "ID:")
		require.Empty(t, m.sidebarSession.idRect)
		require.Empty(t, m.sidebarSession.id)
	}
	m.session = nil
	m.updateSidebarScrollState()
	require.Empty(t, m.sidebarSession.idRect)
}

func TestSidebarDirectoryTickerRequiresHover(t *testing.T) {
	path := "/mnt/projects/日本語/long-directory-for-sidebar-ticker"
	m := sidebarSessionPreview(t, map[string]any{"directory": path}, 80)
	before, idRect := sidebarFrameText(m), m.sidebarSession.idRect
	require.Equal(t, tea.MouseModeAllMotion, m.View().MouseMode, "first frame must request unpressed motion")
	// Ordinary updates and stale ticks must not animate a mouseless client.
	for range 5 {
		_, cmd := m.Update(sidebarDirectoryTickMsg{})
		require.Nil(t, cmd)
		require.False(t, m.sidebarSession.ticking)
		require.Equal(t, before, sidebarFrameText(m))
	}
	dirRect := m.sidebarSession.directoryRect
	filter := NewFilter()
	_, cmd := m.Update(filter.Filter(m, tea.MouseMotionMsg{X: dirRect.Min.X, Y: dirRect.Min.Y, Button: uv.MouseNone}))
	require.NotNil(t, cmd, "hovering the padding starts the real timer")
	require.True(t, m.sidebarSession.ticking)
	tick, ok := cmd().(sidebarDirectoryTickMsg)
	require.True(t, ok)
	_, next := m.Update(tick)
	require.NotNil(t, next)
	require.Equal(t, 1, m.sidebarSession.offset)
	require.NotEqual(t, before, sidebarFrameText(m))
	require.Equal(t, idRect, m.sidebarSession.idRect)
	width := m.sidebarContentWidth
	for range ansi.StringWidth(path) + 3 {
		m.Update(sidebarDirectoryTickMsg{m.sidebarSession.tickGeneration})
		body, _ := m.sidebarSessionInfo(width)
		require.Equal(t, width, ansi.StringWidth(strings.Split(body, "\n")[0]))
	}
	oldTick := sidebarDirectoryTickMsg{m.sidebarSession.tickGeneration}
	oldScroll := m.chat.list.Offset()
	_, cmd = m.Update(filter.Filter(m, tea.MouseMotionMsg{X: m.layout.main.Min.X + 2, Y: 0, Button: uv.MouseNone}))
	require.Nil(t, cmd)
	require.False(t, m.sidebarSession.ticking)
	require.Equal(t, oldScroll, m.chat.list.Offset(), "unpressed motion must not edge-scroll chat")
	require.Equal(t, before, sidebarFrameText(m), "leaving restores the static path")
	_, cmd = m.Update(oldTick)
	require.Nil(t, cmd, "an outstanding tick cannot restart the ticker")
	require.Zero(t, m.sidebarSession.offset)
	for _, stop := range []tea.Msg{tea.BlurMsg{}, tea.WindowSizeMsg{Width: m.width, Height: m.height}} {
		m.Update(tea.MouseMotionMsg{X: dirRect.Min.X + 1, Y: dirRect.Min.Y, Button: uv.MouseNone})
		require.True(t, m.sidebarSession.ticking)
		m.Update(stop)
		require.False(t, m.sidebarSession.ticking)
		m.View()
	}
	m.Update(tea.MouseMotionMsg{X: dirRect.Min.X + 1, Y: dirRect.Min.Y, Button: uv.MouseNone})
	m.sidebarCollapsed = map[string]bool{"session": true}
	m.Update(sidebarDirectoryTickMsg{m.sidebarSession.tickGeneration})
	require.False(t, m.sidebarSession.ticking)
	require.Empty(t, m.sidebarSession.idRect)
	require.Equal(t, tea.MouseModeCellMotion, m.View().MouseMode)
	m.sidebarCollapsed["session"] = false
	m.com.Workspace.(*previewWorkspace).taskData.Directory = "/short"
	m.View()
	dirRect = m.sidebarSession.directoryRect
	_, cmd = m.Update(tea.MouseMotionMsg{X: dirRect.Min.X + 1, Y: dirRect.Min.Y, Button: uv.MouseNone})
	require.Nil(t, cmd)
	require.False(t, m.sidebarSession.ticking)
	require.Equal(t, tea.MouseModeCellMotion, m.View().MouseMode)
}

func TestSidebarDirectoryStripPaddingAndColors(t *testing.T) {
	m := sidebarSessionPreview(t, map[string]any{"directory": "/short"}, 80)
	view := m.View()
	r := m.sidebarSession.directoryRect
	screen := uv.NewScreenBuffer(m.width, m.height)
	uv.NewStyledString(view.Content).Draw(screen, screen.Bounds())
	require.Equal(t, 1, r.Dy())
	require.Equal(t, m.sidebarContentWidth, r.Dx())
	require.Equal(t, " ", screen.CellAt(r.Min.X, r.Min.Y).Content)
	require.Equal(t, "/", screen.CellAt(r.Min.X+1, r.Min.Y).Content)
	require.Equal(t, " ", screen.CellAt(r.Max.X-1, r.Min.Y).Content)
	for x := r.Min.X; x < r.Max.X; x++ {
		cell := screen.CellAt(x, r.Min.Y)
		require.Equal(t, color.RGBAModel.Convert(m.com.Styles.Sidebar.Directory.GetBackground()), color.RGBAModel.Convert(cell.Style.Bg))
		if strings.TrimSpace(cell.Content) != "" {
			require.Equal(t, color.RGBAModel.Convert(m.com.Styles.Sidebar.Directory.GetForeground()), color.RGBAModel.Convert(cell.Style.Fg))
			require.NotZero(t, cell.Style.Attrs&uv.AttrBold)
		}
	}
}

func TestSidebarSessionIDMouseSelection(t *testing.T) {
	m := sidebarSessionPreview(t, map[string]any{"directory": "/short"}, 80)
	r := m.sidebarSession.idRect
	click := tea.MouseClickMsg{X: r.Min.X, Y: r.Min.Y, Button: uv.MouseLeft}
	m.Update(click)
	m.Update(tea.MouseMotionMsg{X: r.Max.X + 10, Y: r.Min.Y + 2, Button: uv.MouseLeft})
	require.Equal(t, session.ShortID(m.session.ID), m.sidebarSession.selectedID())
	require.False(t, m.chat.HasHighlight())
	view := m.View()
	screen := uv.NewScreenBuffer(m.width, m.height)
	uv.NewStyledString(view.Content).Draw(screen, screen.Bounds())
	for x := r.Min.X; x < r.Max.X; x++ {
		require.Equal(t, m.com.Styles.TextSelection.GetBackground(), screen.CellAt(x, r.Min.Y).Style.Bg)
	}
	_, release := m.Update(tea.MouseReleaseMsg{X: r.Max.X + 10, Y: r.Min.Y + 2, Button: uv.MouseLeft})
	require.NotNil(t, release)
	copyMsg := release()
	_, copyCmd := m.Update(copyMsg)
	require.NotNil(t, copyCmd)
	// Inspect the real clipboard command's OSC 52 message without changing the
	// test runner's native clipboard. This is the same command chat uses.
	sequence := reflect.ValueOf(copyCmd())
	clipboardMsg := sequence.Index(0).Interface().(tea.Cmd)()
	require.Equal(t, session.ShortID(m.session.ID), reflect.ValueOf(clipboardMsg).String())
	m.Update(sidebarSessionIDCopiedMsg{m.sidebarSession.selectionGeneration})
	require.Empty(t, m.sidebarSession.selectedID())
	m.Update(click)
	m.Update(tea.MouseReleaseMsg{X: r.Min.X, Y: r.Min.Y, Button: uv.MouseLeft})
	m.Update(click)
	require.Equal(t, session.ShortID(m.session.ID), m.sidebarSession.selectedID(), "double click selects the whole ID")
	m.session.ID = "different-session"
	m.View()
	require.Empty(t, m.sidebarSession.selectedID())
	_, stale := m.Update(copyMsg)
	require.Nil(t, stale)
	m.sidebarCollapsed = map[string]bool{"session": true}
	m.updateSidebarScrollState()
	require.Equal(t, image.Rectangle{}, m.sidebarSession.idRect)
	require.False(t, m.sidebarSession.directoryVisible)
}

// Exercise New's actual CLI startup arguments and the accepted loadSessionMsg
// path before inspecting UI.View. The fixture supplies only workspace data.
type sidebarResumeWorkspace struct{ *previewWorkspace }

func (w *sidebarResumeWorkspace) AuthenticationWorkspaceID() string { return "sidebar-resume" }

func TestSidebarResumeStartsAdaptive(t *testing.T) {
	for _, savedCompact := range []bool{false, true} {
		for _, mode := range []string{"session", "continue", "fresh"} {
			t.Run(fmt.Sprintf("%s/savedCompact=%t", mode, savedCompact), func(t *testing.T) {
				fixture := sidebarSessionPreview(t, map[string]any{"directory": "/short"}, 80)
				ws := &sidebarResumeWorkspace{fixture.com.Workspace.(*previewWorkspace)}
				ws.cfg.Options.TUI.CompactMode = savedCompact
				initialID := ""
				if mode == "session" {
					initialID = "resume-session"
				}
				m := New(common.DefaultCommon(ws), initialID, mode == "continue", "")
				m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
				m.Update(loadSessionMsg{source: ws, workspaceID: ws.AuthenticationWorkspaceID(), generation: m.sessionLoadGeneration, session: &session.Session{ID: "resume-session", Title: "Resumed session"}})
				wide := m.View()
				require.Equal(t, uiChat, m.state)
				require.Equal(t, savedCompact && mode == "fresh", m.isCompact)
				require.Equal(t, !savedCompact || mode != "fresh", strings.Contains(ansi.Strip(wide.Content), "▾ Session"))
				require.Equal(t, savedCompact, ws.cfg.Options.TUI.CompactMode, "startup must not rewrite the saved setting")
				if mode == "fresh" {
					return
				}
				for _, size := range []struct {
					w, h    int
					compact bool
				}{{119, 60, true}, {120, 60, false}, {160, 29, true}, {160, 30, false}, {160, 60, false}} {
					m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
					frame := m.View()
					require.Equal(t, size.compact, m.isCompact)
					require.Equal(t, !size.compact, strings.Contains(ansi.Strip(frame.Content), "▾ Session"))
				}
				m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl})
				require.True(t, m.isCompact, "manual hide still applies after startup")
				m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl})
				require.False(t, m.isCompact, "manual show still applies after startup")
			})
		}
	}
}

type sidebarFreshWorkspace struct {
	*sidebarResumeWorkspace
	created int
}

func (w *sidebarFreshWorkspace) AgentReadyErr() error { return nil }

func (w *sidebarFreshWorkspace) CreateSession(_ context.Context, title string) (session.Session, error) {
	w.created++
	return session.Session{ID: fmt.Sprintf("fresh-session-%d", w.created), Title: title}, nil
}

func TestSidebarFreshSessionOpensAfterFirstMessage(t *testing.T) {
	for _, savedCompact := range []bool{false, true} {
		for _, kind := range []string{"prompt", "shell", "initial-prompt"} {
			for _, size := range []struct{ width, height int }{{119, 60}, {120, 60}, {160, 29}, {160, 30}, {160, 60}} {
				t.Run(fmt.Sprintf("%s/savedCompact=%t/%dx%d", kind, savedCompact, size.width, size.height), func(t *testing.T) {
					fixture := sidebarSessionPreview(t, map[string]any{"directory": "/short"}, 80)
					ws := &sidebarFreshWorkspace{sidebarResumeWorkspace: &sidebarResumeWorkspace{fixture.com.Workspace.(*previewWorkspace)}}
					ws.cfg.Options.TUI.CompactMode = savedCompact
					initialPrompt := ""
					if kind == "initial-prompt" {
						initialPrompt = "first message"
					}
					m := New(common.DefaultCommon(ws), "", false, initialPrompt)
					m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
					assertSidebar := func(visible bool) {
						t.Helper()
						frame := m.View()
						require.Equal(t, visible, strings.Contains(ansi.Strip(frame.Content), "▾ Session"))
					}
					require.Equal(t, uiLanding, m.state)
					assertSidebar(false)
					m.textarea.SetValue("   ")
					m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					require.Zero(t, ws.created, "empty submissions must not start a session")
					assertSidebar(false)

					if kind == "initial-prompt" {
						m.Update(m.sendInitialPrompt()())
					} else {
						m.bangMode = kind == "shell"
						m.textarea.SetValue("first message")
						m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					}
					require.Equal(t, 1, ws.created)
					require.Equal(t, uiChat, m.state)
					room := size.width >= compactModeWidthBreakpoint && size.height >= compactModeHeightBreakpoint
					assertSidebar(room)
					require.Equal(t, savedCompact, ws.cfg.Options.TUI.CompactMode, "session defaults must not rewrite saved configuration")

					// A terminal that grows after the first message should reveal
					// the sidebar through the same adaptive layout path.
					m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
					assertSidebar(true)
					m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl})
					assertSidebar(false)
					m.Update(sendMessageMsg{Content: "second message"})
					require.Equal(t, 1, ws.created)
					assertSidebar(false)

					// Starting another session preserves the empty landing screen,
					// then drops the previous session's manual hide on submission.
					m.newSession()
					assertSidebar(false)
					m.Update(sendMessageMsg{Content: "first message in another session"})
					require.Equal(t, 2, ws.created)
					assertSidebar(true)
				})
			}
		}
	}
}
