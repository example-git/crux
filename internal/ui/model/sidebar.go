package model

import (
	"cmp"
	"fmt"
	"image"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/charmtone"
	mcp "github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/logo"
)

// modelInfo renders the current model information including reasoning
// settings and context usage/cost for the sidebar.
func (m *UI) modelInfo(width int) string {
	model := m.selectedLargeModel()
	reasoningInfo := ""
	providerID := ""
	providerName := ""

	if model != nil {
		// Get provider name first
		providerID = model.ModelCfg.Provider
		providerConfig, ok := m.com.Config().Providers.Get(providerID)
		if ok {
			providerName = providerConfig.Name

			// Only check reasoning if model can reason
			if model.CatalogModel.CanReason {
				if len(model.CatalogModel.ReasoningLevels) == 0 {
					if model.ModelCfg.Think {
						reasoningInfo = "Thinking On"
					} else {
						reasoningInfo = "Thinking Off"
					}
				} else {
					reasoningEffort := cmp.Or(model.ModelCfg.ReasoningEffort, model.CatalogModel.DefaultReasoningEffort)
					reasoningInfo = fmt.Sprintf("Reasoning %s", common.FormatReasoningEffort(reasoningEffort))
				}
			}
		}
	}

	var modelContext *common.ModelContextInfo
	if model != nil && m.session != nil {
		modelContext = &common.ModelContextInfo{
			ContextUsed:    m.session.CompletionTokens + m.session.PromptTokens,
			Cost:           m.session.Cost,
			ModelContext:   model.CatalogModel.ContextWindow,
			EstimatedUsage: m.session.EstimatedUsage,
		}
	}
	var modelName string
	if model != nil {
		modelName = model.CatalogModel.Name
	}
	return common.ModelInfo(m.com.Styles, modelName, providerID, providerName, reasoningInfo, modelContext, width)
}

func (m *UI) sidebarBrand() *providerBrand {
	if m.sidebarShowCruxLogo {
		return nil
	}
	return m.brand
}

func (m *UI) handleSidebarLogoClick(msg tea.MouseClickMsg) bool {
	if m.state != uiChat || msg.Button != uv.MouseLeft || m.brand == nil || m.sidebarBrandLogoHeight <= 0 {
		return false
	}
	if point := image.Pt(msg.X, msg.Y); !point.In(m.layout.sidebar) || msg.Y >= m.layout.sidebar.Min.Y+m.sidebarBrandLogoHeight {
		return false
	}
	m.sidebarShowCruxLogo = !m.sidebarShowCruxLogo
	m.cacheSidebarLogo(m.layout.sidebar.Dx())
	return true
}

func (m *UI) handleSidebarFilesClick(msg tea.MouseClickMsg) bool {
	if m.state != uiChat || m.isCompact || msg.Button != uv.MouseLeft || m.sidebarContent == "" || m.sidebarFilesHeaderLine < 0 {
		return false
	}
	if !image.Pt(msg.X, msg.Y).In(m.layout.sidebar) {
		return false
	}
	contentTop := m.layout.sidebar.Min.Y + lipgloss.Height(m.sidebarDrawLogo)
	contentLine := msg.Y - contentTop + m.sidebarOffset
	if contentLine != m.sidebarFilesHeaderLine {
		return false
	}
	m.sidebarFilesCollapsed = !m.sidebarFilesCollapsed
	return true
}

// updateSidebarScrollState renders the sidebar content and computes scroll
// state (scrollability, max offset, clamp) before drawing. This keeps all
// state mutation in the update path rather than in the draw function.
func (m *UI) updateSidebarScrollState() {
	if m.session == nil || m.isCompact {
		return
	}

	const logoHeightBreakpoint = 30

	t := m.com.Styles
	width := m.layout.sidebar.Dx()
	height := m.layout.sidebar.Dy()

	contentWidth := max(width-2, 1)

	title := t.Sidebar.SessionTitle.Width(contentWidth).MaxHeight(2).Render(m.session.Title)
	cwd := common.PrettyPath(t, m.com.Workspace.WorkingDir(), contentWidth)
	sidebarLogo := m.sidebarLogo
	if height < logoHeightBreakpoint {
		smallOpts := logo.Opts{}
		if brand := m.sidebarBrand(); brand != nil {
			smallOpts.Title = brand.Title
			smallOpts.TitleColorA = brand.GradA
			smallOpts.TitleColorB = brand.GradB
		}
		sidebarLogo = lipgloss.JoinVertical(lipgloss.Left, logo.SmallRender(m.com.Styles, contentWidth, smallOpts), "")
	}
	m.sidebarBrandLogoHeight = lipgloss.Height(sidebarLogo)
	// Pin provider quota usage right below the logo so it stays visible
	// while the rest of the sidebar scrolls.
	if usageBars := m.usageBars(contentWidth, false); usageBars != "" {
		sidebarLogo = lipgloss.JoinVertical(lipgloss.Left, sidebarLogo, usageBars, "")
	}

	var logoRect, contentRect image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(sidebarLogo)),
		layout.Fill(1),
	).Split(m.layout.sidebar).Assign(&logoRect, &contentRect)

	contentHeight := contentRect.Dy()

	// Render all items without truncation; virtual scrolling handles overflow.
	lspSection := m.lspInfo(contentWidth, len(m.lspStates), true)
	mcpSection := m.mcpInfo(contentWidth, mcpCount(m.com.Config().MCP.Sorted(), m.mcpStates), true)
	skillsSection := m.skillsInfo(contentWidth, len(m.skillStatusItems()), true)
	filesSection := m.filesInfo(m.com.Workspace.WorkingDir(), contentWidth, fileChangeCount(m.sessionFiles), true)

	// Build the scrollable content.
	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		"",
		cwd,
		"",
		m.modelInfo(contentWidth),
		"",
		filesSection,
		"",
		lspSection,
		"",
		mcpSection,
		"",
		skillsSection,
	)

	m.sidebarFilesHeaderLine = -1
	for i, line := range strings.Split(content, "\n") {
		if strings.Contains(ansi.Strip(line), "Modified Files") {
			m.sidebarFilesHeaderLine = i
			break
		}
	}

	totalLines := strings.Count(content, "\n") + 1
	m.sidebarContent = content
	m.sidebarTotalLines = totalLines
	m.sidebarContentWidth = contentWidth
	m.sidebarContentHeight = contentHeight
	m.sidebarDrawLogo = sidebarLogo
	m.sidebarScrollable = totalLines > contentHeight
	m.sidebarMaxOffsetVal = max(0, totalLines-contentHeight)

	// If the sidebar is focused but no longer scrollable (e.g. after a
	// resize), return focus to the chat.
	if m.focus == uiFocusSidebar && !m.sidebarScrollable {
		m.focus = uiFocusMain
		m.chat.Focus()
	}

	// Clamp sidebarOffset.
	if m.sidebarOffset > m.sidebarMaxOffsetVal {
		m.sidebarOffset = m.sidebarMaxOffsetVal
	}
}

// drawSidebar renders the chat sidebar with a fixed logo and a
// virtual-scrolling content area with an auto-hiding scrollbar. While the
// sidebar is focused, the scrollbar stays visible.
func (m *UI) drawSidebar(scr uv.Screen, area uv.Rectangle) {
	if m.session == nil {
		return
	}

	sidebarLogo := m.sidebarDrawLogo
	contentWidth := m.sidebarContentWidth
	contentHeight := m.sidebarContentHeight
	totalLines := m.sidebarTotalLines

	var logoRect, contentRect image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(sidebarLogo)),
		layout.Fill(1),
	).Split(area).Assign(&logoRect, &contentRect)

	// Slice visible lines.
	end := min(m.sidebarOffset+contentHeight, totalLines)
	lines := strings.Split(m.sidebarContent, "\n")
	visibleLines := lines[m.sidebarOffset:end]
	visibleStr := strings.Join(visibleLines, "\n")

	// Determine scrollbar visibility: always visible when focused, otherwise
	// auto-hide.
	scrollbarVisible := totalLines > contentHeight && (m.sidebarScrollbarVisible || m.focus == uiFocusSidebar)

	// Draw the fixed logo.
	uv.NewStyledString(
		lipgloss.NewStyle().
			MaxWidth(contentWidth).
			MaxHeight(lipgloss.Height(sidebarLogo)).
			Render(sidebarLogo),
	).Draw(scr, logoRect)

	// Draw the visible content in the scrollable area.
	uv.NewStyledString(
		lipgloss.NewStyle().
			MaxWidth(contentWidth).
			MaxHeight(contentHeight).
			Render(visibleStr),
	).Draw(scr, contentRect)

	// Draw scrollbar in the reserved column.
	if scrollbarVisible {
		scrollbar := common.Scrollbar(m.com.Styles, contentHeight, totalLines, contentHeight, m.sidebarOffset)
		if scrollbar != "" {
			scrollbarArea := image.Rectangle{
				Min: image.Point{X: area.Max.X - 1, Y: contentRect.Min.Y},
				Max: image.Point{X: area.Max.X, Y: area.Max.Y},
			}
			uv.NewStyledString(scrollbar).Draw(scr, scrollbarArea)
		}
	}
}

// fileChangeCount returns the number of changed session files.
func fileChangeCount(files []SessionFile) int {
	count := 0
	for _, f := range files {
		if !f.Created && f.Additions == 0 && f.Deletions == 0 {
			continue
		}
		count++
	}
	return count
}

// usageBars renders one line per provider quota window in the form
// "NAME [██████░░░░]" where the bar is filled with the *remaining* quota.
// When withPercent is true, the remaining percentage is appended. Returns ""
// when no usage data is available.
func (m *UI) usageBars(width int, withPercent bool) string {
	u := m.providerUsage
	if u == nil || len(u.Windows) == 0 {
		return ""
	}
	t := m.com.Styles

	nameWidth := 0
	for _, w := range u.Windows {
		nameWidth = max(nameWidth, lipgloss.Width(w.Name))
	}

	var lines []string
	for _, w := range u.Windows {
		remaining := 100 - w.Percent
		suffix := ""
		if withPercent {
			suffix = fmt.Sprintf(" %d%% left", remaining)
		}
		// name + space + "[" + bar + "]" + suffix
		barWidth := min(20, width-nameWidth-3-lipgloss.Width(suffix))
		if barWidth < 3 {
			continue
		}
		filled := barWidth * remaining / 100
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		var barColor color.Color = charmtone.Guac
		if m.brand != nil {
			barColor = m.brand.Accent
		}
		barStyle := lipgloss.NewStyle().Foreground(barColor)
		switch {
		case remaining <= 10:
			barStyle = barStyle.Foreground(charmtone.Sriracha)
		case remaining <= 25:
			barStyle = barStyle.Foreground(charmtone.Mustard)
		}
		line := t.Header.Percentage.Render(fmt.Sprintf("%-*s", nameWidth, w.Name)) +
			" [" + barStyle.Render(bar) + "]" +
			t.Header.Percentage.Render(suffix)
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// mcpCount returns the number of MCP servers that have a state entry.
func mcpCount(mcpCfgs []config.MCP, states map[string]mcp.ClientInfo) int {
	count := 0
	for _, cfg := range mcpCfgs {
		if _, ok := states[cfg.Name]; ok {
			count++
		}
	}
	return count
}
