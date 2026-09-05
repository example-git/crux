package model

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/example-git/crux/internal/home"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/util"
)

// markProjectInitializedCmd marks the current project as initialized in the config.
func (m *UI) markProjectInitializedCmd() tea.Cmd {
	return func() tea.Msg {
		if err := m.com.Workspace.MarkProjectInitialized(); err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("Failed to mark project as initialized: %v", err),
				TTL:  15 * time.Second,
			}
		}
		return nil
	}
}

// updateInitializeView handles keyboard input for the project initialization prompt.
func (m *UI) updateInitializeView(msg tea.KeyPressMsg) (cmds []tea.Cmd) {
	switch {
	case key.Matches(msg, m.keyMap.Initialize.Enter):
		if m.onboarding.yesInitializeSelected {
			cmds = append(cmds, m.initializeProject())
		} else {
			cmds = append(cmds, m.skipInitializeProject())
		}
	case key.Matches(msg, m.keyMap.Initialize.Switch):
		m.onboarding.yesInitializeSelected = !m.onboarding.yesInitializeSelected
	case key.Matches(msg, m.keyMap.Initialize.Yes):
		cmds = append(cmds, m.initializeProject())
	case key.Matches(msg, m.keyMap.Initialize.No):
		cmds = append(cmds, m.skipInitializeProject())
	}
	return cmds
}

// initializeProject starts project initialization and transitions to normal
// chat. During startup it deliberately keeps the pending prompt's session:
// the initialization message is processed first and the pending prompt is then
// submitted to that same session by tea.Sequence.
func (m *UI) initializeProject() tea.Cmd {
	startup := m.state == uiInitialize
	var cmds []tea.Cmd
	if !startup {
		if cmd := m.newSession(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	m.setState(uiLanding, uiFocusEditor)
	if startup {
		if cmd := m.loadInitialSession(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	initialize := func() tea.Msg {
		initPrompt, err := m.com.Workspace.InitializePrompt()
		if err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("Failed to initialize project: %v", err),
			}
		}
		return sendMessageMsg{Content: initPrompt}
	}
	cmds = append(cmds, m.markProjectInitializedCmd(), initialize)
	if cmd := m.sendInitialPrompt(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Sequence(cmds...)
}

// skipInitializeProject skips project initialization and transitions to the landing view.
func (m *UI) skipInitializeProject() tea.Cmd {
	startup := m.state == uiInitialize
	m.setState(uiLanding, uiFocusEditor)
	var cmds []tea.Cmd
	if startup {
		if cmd := m.loadInitialSession(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	cmds = append(cmds, m.markProjectInitializedCmd())
	if cmd := m.sendInitialPrompt(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Sequence(cmds...)
}

// initializeView renders the project initialization prompt with Yes/No buttons.
func (m *UI) initializeView() string {
	s := m.com.Styles.Initialize
	cwd := home.Short(m.com.Workspace.WorkingDir())
	initFile := m.com.Config().Options.InitializeAs

	header := s.Header.Render("Would you like to initialize this project?")
	path := s.Accent.PaddingLeft(2).Render(cwd)
	desc := s.Content.Render(fmt.Sprintf("When I initialize your codebase I examine the project and put the result into an %s file which serves as general context.", initFile))
	hint := s.Content.Render("You can also initialize anytime via ") + s.Accent.Render("ctrl+p") + s.Content.Render(".")
	prompt := s.Content.Render("Would you like to initialize now?")

	buttonOpts := []common.ButtonOpts{
		{Text: "Yep!", Selected: m.onboarding.yesInitializeSelected, Hovered: m.onboarding.hoveredInitializeButton == 1},
		{Text: "Nope", Selected: !m.onboarding.yesInitializeSelected, Hovered: m.onboarding.hoveredInitializeButton == 2},
	}
	buttons := common.ButtonGroup(m.com.Styles, buttonOpts, " ")

	// max width 60 so the text is compact
	width := min(m.layout.main.Dx(), 60)

	view := lipgloss.NewStyle().
		Width(width).
		Height(m.layout.main.Dy()).
		PaddingBottom(1).
		AlignVertical(lipgloss.Bottom).
		Render(strings.Join(
			[]string{
				header,
				path,
				desc,
				hint,
				prompt,
				buttons,
			},
			"\n\n",
		))
	m.onboarding.buttonCompositor = common.ButtonHitCompositorForView(m.com.Styles, buttonOpts, view, m.layout.main.Min.X, m.layout.main.Min.Y)
	return view
}

func (m *UI) handleInitializeHover(msg tea.MouseMotionMsg) bool {
	if m.state != uiInitialize {
		return false
	}
	m.onboarding.hoveredInitializeButton = common.HitButtonIndex(m.onboarding.buttonCompositor, msg.X, msg.Y) + 1
	return true
}

func (m *UI) handleInitializeClick(msg tea.MouseClickMsg) (tea.Cmd, bool) {
	if m.state != uiInitialize || msg.Button != uv.MouseLeft {
		return nil, false
	}
	index := common.HitButtonIndex(m.onboarding.buttonCompositor, msg.X, msg.Y)
	if index < 0 || index > 1 {
		return nil, false
	}
	m.onboarding.yesInitializeSelected = index == 0
	if m.onboarding.yesInitializeSelected {
		return m.initializeProject(), true
	}
	return m.skipInitializeProject(), true
}
