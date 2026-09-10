package model

import (
	"image"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/workspace"
)

const sidebarDirectoryPause = 1200 * time.Millisecond
const sidebarDirectoryStep = 160 * time.Millisecond

type sidebarDirectoryTickMsg struct{ generation uint64 }
type copySidebarSessionIDMsg struct{ generation uint64 }
type sidebarSessionIDCopiedMsg struct{ generation uint64 }

type sidebarSessionState struct {
	path, id                  string
	width, offset             int
	ticking, directoryVisible bool
	directoryRect             image.Rectangle
	directoryHovered          bool
	directoryOverflow         bool
	tickGeneration            uint64
	idRect                    image.Rectangle
	anchor, end               int
	pressed                   bool
	lastClick                 time.Time
	lastClickX                int
	selectionGeneration       uint64
}

func sidebarSingleLine(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(value))
}

func (m *UI) sidebarSessionInfo(width int) (body, id string) {
	if m.session == nil {
		return "", ""
	}
	style := m.com.Styles.Sidebar.SessionTitle
	padding := min(1, width/2)
	textWidth := max(0, width-2*padding)
	path := sidebarSingleLine(m.com.Workspace.WorkingDir())
	offset := 0
	if m.sidebarSession.path == path && m.sidebarSession.width == textWidth {
		offset = m.sidebarSession.offset
	}
	directory := path
	if ansi.StringWidth(path) > textWidth {
		directory = ansi.Truncate(path, textWidth, "…")
		if m.sidebarSession.ticking && m.sidebarSession.directoryHovered {
			directory = ansi.Truncate(ansi.Cut(path+"   "+path, offset, offset+textWidth), textWidth, "")
		}
	}
	lines := []string{m.com.Styles.Sidebar.Directory.Padding(0, padding).Width(width).MaxHeight(1).Render(directory)}
	if remote, ok := m.com.Workspace.(workspace.RemoteAddressProvider); ok {
		if address := sidebarSingleLine(remote.RemoteAddress()); strings.TrimSpace(address) != "" {
			lines = append(lines, ansi.Truncate(style.Bold(true).Render("Remote:")+" "+style.Bold(false).Render(address), width, "…"))
		}
	}
	if title := strings.TrimSpace(sidebarSingleLine(m.session.Title)); title != "" {
		lines = append(lines, style.Bold(true).Width(width).Render(title))
	}
	id = session.ShortID(m.session.ID)
	if id != "" {
		lines = append(lines, style.Bold(false).Render("ID: "+id))
	}
	return strings.Join(lines, "\n"), id
}

func (m *UI) sidebarDirectoryCanHover() bool {
	return m.state == uiChat && m.hasSession() && !m.isCompact && !m.sidebarCollapsed["session"] &&
		(m.dialog == nil || !m.dialog.HasDialogs()) && !m.sidebarSession.directoryRect.Empty()
}

// Observe events before panel/dialog routing so leaving the strip always stops
// its ticker. No mouse event means no hover and no animation command.
func (m *UI) trackSidebarDirectoryHover(msg tea.Msg) {
	s := &m.sidebarSession
	switch msg := msg.(type) {
	case tea.MouseMsg:
		mouse := msg.Mouse()
		s.directoryHovered = m.sidebarDirectoryCanHover() && image.Pt(mouse.X, mouse.Y).In(s.directoryRect)
	case tea.BlurMsg, tea.WindowSizeMsg:
		s.directoryHovered = false
	}
}

func (s *sidebarSessionState) stopTicker() {
	if s.ticking {
		s.tickGeneration++
	}
	s.ticking, s.offset = false, 0
}

// The idle fast path does not even compose the sidebar. Only a hovered,
// overflowing directory may schedule a tick, including on mouseless clients.
func (m *UI) syncSidebarDirectoryTicker() tea.Cmd {
	s := &m.sidebarSession
	if !m.sidebarDirectoryCanHover() {
		s.setGeometry("", image.Rectangle{}, image.Rectangle{}, false)
	}
	if !s.directoryHovered {
		s.stopTicker()
		return nil
	}
	m.updateSidebarScrollState()
	path, width := sidebarSingleLine(m.com.Workspace.WorkingDir()), max(0, m.layout.sidebar.Dx()-6)
	if !s.directoryHovered || !s.directoryOverflow || width <= 0 {
		s.stopTicker()
		return nil
	}
	if s.ticking && s.path == path && s.width == width {
		return nil
	}
	s.path, s.width, s.offset, s.ticking = path, width, 0, true
	s.tickGeneration++
	return sidebarDirectoryTick(s.tickGeneration, sidebarDirectoryPause)
}

func sidebarDirectoryTick(generation uint64, delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg { return sidebarDirectoryTickMsg{generation} })
}

func (m *UI) advanceSidebarDirectoryTicker(msg sidebarDirectoryTickMsg) tea.Cmd {
	s := &m.sidebarSession
	if !s.ticking || !s.directoryHovered || !m.sidebarDirectoryCanHover() || msg.generation != s.tickGeneration {
		return nil
	}
	s.offset = (s.offset + 1) % (ansi.StringWidth(s.path) + 3)
	delay := sidebarDirectoryStep
	if s.offset == 0 {
		delay = sidebarDirectoryPause
	}
	return sidebarDirectoryTick(s.tickGeneration, delay)
}

func (s *sidebarSessionState) clearSelection() {
	s.anchor, s.end, s.pressed = 0, 0, false
	s.lastClick = time.Time{}
	s.selectionGeneration++
}

func (s *sidebarSessionState) setGeometry(id string, rect, directoryRect image.Rectangle, overflow bool) {
	if s.id != id || s.idRect != rect {
		s.clearSelection()
	}
	if s.directoryRect != directoryRect {
		s.directoryHovered = false
		s.stopTicker()
	}
	s.id, s.idRect, s.directoryVisible = id, rect, !directoryRect.Empty()
	s.directoryRect, s.directoryOverflow = directoryRect, overflow
}

func (s *sidebarSessionState) selectedID() string {
	return ansi.Cut(s.id, min(s.anchor, s.end), max(s.anchor, s.end))
}

// Run ahead of chat/inline/task-panel routing so dragging beyond the seven
// characters still completes the sidebar gesture and never selects the chat.
func (m *UI) routeSidebarSessionInput(msg tea.Msg) (bool, tea.Cmd) {
	s := &m.sidebarSession
	if m.dialog != nil && m.dialog.HasDialogs() || m.state != uiChat || m.isCompact || m.session == nil {
		if s.pressed || s.selectedID() != "" {
			s.clearSelection()
		}
		return false, nil
	}
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		if msg.Button != uv.MouseLeft {
			return false, nil
		}
		if s.id == "" || !image.Pt(msg.X, msg.Y).In(s.idRect) {
			s.clearSelection()
			return false, nil
		}
		double := time.Since(s.lastClick) <= doubleClickThreshold && abs(msg.X-s.lastClickX) <= clickTolerance
		s.selectionGeneration++
		s.anchor = msg.X - s.idRect.Min.X
		s.end, s.pressed = s.anchor, true
		if double {
			s.anchor, s.end = 0, ansi.StringWidth(s.id)
		}
		s.lastClick, s.lastClickX = time.Now(), msg.X
		m.chat.ClearMouse()
		return true, nil
	case tea.MouseMotionMsg:
		if s.pressed {
			s.end = max(0, min(ansi.StringWidth(s.id), msg.X-s.idRect.Min.X))
			return true, nil
		}
	case tea.MouseReleaseMsg:
		if s.pressed && msg.Button == uv.MouseLeft {
			s.pressed = false
			if s.selectedID() != "" {
				generation := s.selectionGeneration
				return true, tea.Tick(doubleClickThreshold, func(time.Time) tea.Msg { return copySidebarSessionIDMsg{generation} })
			}
			return true, nil
		}
	}
	return false, nil
}

func (m *UI) copySidebarSessionID(msg copySidebarSessionIDMsg) tea.Cmd {
	s := &m.sidebarSession
	if msg.generation != s.selectionGeneration || s.pressed || s.selectedID() == "" || m.dialog.HasDialogs() || m.state != uiChat || m.isCompact || m.session == nil || s.id != session.ShortID(m.session.ID) || s.idRect.Empty() || m.sidebarCollapsed["session"] {
		return nil
	}
	return common.CopyToClipboardWithCallback(s.selectedID(), "Selected text copied to clipboard", func() tea.Msg {
		return sidebarSessionIDCopiedMsg{msg.generation}
	})
}

func (m *UI) drawSidebarSessionSelection(scr uv.Screen) {
	s := &m.sidebarSession
	if selected := s.selectedID(); selected != "" && !s.idRect.Empty() {
		rect := s.idRect
		rect.Min.X += min(s.anchor, s.end)
		rect.Max.X = rect.Min.X + ansi.StringWidth(selected)
		uv.NewStyledString(m.com.Styles.TextSelection.Render(selected)).Draw(scr, rect)
	}
}
