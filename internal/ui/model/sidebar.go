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
			ContextUsed:    m.session.ContextTokens(),
			Cost:           m.session.Cost,
			ModelContext:   model.CatalogModel.ContextWindow,
			EstimatedUsage: m.session.ContextEstimated(),
		}
	}
	var modelName string
	if model != nil {
		modelName = model.CatalogModel.Name
	}
	return common.ModelInfo(m.com.Styles, modelName, providerID, providerName, reasoningInfo, modelContext, width)
}

func sidebarSection(content string, width, height int) string {
	if width <= 0 || height <= 0 || content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) > height {
		lines = lines[:height]
		if height == 1 {
			lines[0] = ansi.Truncate(lines[0], max(0, width-2), "") + " …"
		} else {
			lines[height-1] = "… more"
		}
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return strings.Join(lines, "\n")
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
	if point := image.Pt(msg.X, msg.Y); !point.In(m.layout.sidebar.Inset(1)) || msg.Y >= m.layout.sidebar.Min.Y+1+m.sidebarBrandLogoHeight {
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
	if !image.Pt(msg.X, msg.Y).In(m.layout.sidebar.Inset(1)) {
		return false
	}
	logoHeight := 0
	if m.sidebarDrawLogo != "" {
		logoHeight = lipgloss.Height(m.sidebarDrawLogo)
	}
	contentTop := m.layout.sidebar.Min.Y + 1 + logoHeight
	contentLine := msg.Y - contentTop + m.sidebarOffset
	if contentLine != m.sidebarFilesHeaderLine {
		return false
	}
	m.sidebarFilesCollapsed = !m.sidebarFilesCollapsed
	return true
}

func (m *UI) handleSidebarSectionClick(msg tea.MouseClickMsg) bool {
	if m.state != uiChat || m.isCompact || msg.Button != uv.MouseLeft || m.sidebarContent == "" {
		return false
	}
	if !image.Pt(msg.X, msg.Y).In(m.layout.sidebar.Inset(1)) {
		return false
	}
	logoHeight := 0
	if m.sidebarDrawLogo != "" {
		logoHeight = lipgloss.Height(m.sidebarDrawLogo)
	}
	contentLine := msg.Y - m.layout.sidebar.Min.Y - 1 - logoHeight + m.sidebarOffset
	if contentLine < m.sidebarOffset || contentLine >= m.sidebarOffset+m.sidebarContentHeight {
		return false
	}
	for id, line := range m.sidebarSectionHeaders {
		if line == contentLine {
			if m.sidebarCollapsed == nil {
				m.sidebarCollapsed = make(map[string]bool)
			}
			m.sidebarCollapsed[id] = !m.sidebarCollapsed[id]
			return true
		}
	}
	return false
}

// updateSidebarScrollState renders the sidebar content and computes scroll
// state (scrollability, max offset, clamp) before drawing. This keeps all
// state mutation in the update path rather than in the draw function.
func (m *UI) updateSidebarScrollState() {
	if m.session == nil || m.isCompact {
		return
	}
	t := m.com.Styles
	contentWidth := max(m.layout.sidebar.Dx()-4, 1)
	height := max(0, m.layout.sidebar.Dy()-2)
	type section struct {
		id    string
		lines []string
		rows  int
	}
	var sections []section
	add := func(id, title, body string) {
		marker := "▾ "
		if m.sidebarCollapsed[id] {
			marker = "▸ "
		}
		content := common.Section(t, t.Resource.Heading.Render(marker+title), contentWidth)
		if !m.sidebarCollapsed[id] && body != "" {
			content += "\n\n" + body
		}
		lines := strings.Split(content, "\n")
		sections = append(sections, section{id: id, lines: lines, rows: len(lines)})
	}
	body := func(content string) string {
		_, rest, _ := strings.Cut(content, "\n")
		return strings.TrimLeft(rest, "\n")
	}
	add("model", "Model / Context", m.modelInfo(contentWidth))
	if authority := m.workspaceAuthorityInfo(contentWidth); authority != "" {
		add("authority", "Workspace Authority", authority)
	}
	if count := fileChangeCount(m.sessionFiles); count > 0 {
		lines := strings.Split(m.filesInfo(m.com.Workspace.WorkingDir(), contentWidth, count, true), "\n")
		sections = append(sections, section{id: "files", lines: lines, rows: len(lines)})
	}
	add("session", "Session", t.Sidebar.SessionTitle.Width(contentWidth).Render(m.session.Title)+"\n"+common.PrettyPath(t, m.com.Workspace.WorkingDir(), contentWidth))
	if count := len(m.lspStates); count > 0 {
		add("lsp", "LSPs", body(m.lspInfo(contentWidth, count, false)))
	}
	if count := mcpCount(m.com.Config().MCP.Sorted(), m.mcpStates); count > 0 {
		add("mcp", "MCPs", body(m.mcpInfo(contentWidth, count, false)))
	}
	if count := len(m.skillStatusItems()); count > 0 {
		add("skills", "Skills", body(m.skillsInfo(contentWidth, count, false)))
	}
	if usage := m.usageBars(contentWidth, false); usage != "" {
		add("usage", "Provider Usage", usage)
	}
	gap := 1
	total := func() int {
		rows, visible := 0, 0
		for _, section := range sections {
			if section.rows > 0 {
				rows += section.rows
				visible++
			}
		}
		return rows + max(0, visible-1)*gap
	}
	sidebarLogo := m.sidebarLogo
	logoHeight := func() int {
		if sidebarLogo == "" {
			return 0
		}
		return lipgloss.Height(sidebarLogo)
	}
	if total()+logoHeight() > height {
		gap = 0
		for i := range sections {
			var lines []string
			for _, line := range sections[i].lines {
				if strings.TrimSpace(ansi.Strip(line)) != "" {
					lines = append(lines, line)
				}
			}
			sections[i].lines = lines
			sections[i].rows = len(lines)
		}
	}
	if total()+logoHeight() > height {
		opts := logo.Opts{Sidebar: true}
		if brand := m.sidebarBrand(); brand != nil {
			opts.Title, opts.TitleColorA, opts.TitleColorB = brand.Title, brand.GradA, brand.GradB
		}
		sidebarLogo = logo.SmallRender(t, contentWidth, opts)
	}
	if total()+logoHeight() > height {
		sidebarLogo = ""
	}
	for _, id := range []string{"skills", "mcp", "lsp", "session", "usage", "files", "model"} {
		for i := range sections {
			if sections[i].id != id {
				continue
			}
			floor := 0
			if id == "files" {
				floor = min(2, sections[i].rows)
			}
			if id == "model" {
				floor = min(3, sections[i].rows)
			}
			sections[i].rows -= min(max(0, total()+logoHeight()-height), sections[i].rows-floor)
		}
	}
	for total() > height {
		for i := len(sections) - 1; i >= 0 && total() > height; i-- {
			if sections[i].rows > 0 {
				sections[i].rows--
			}
		}
	}
	m.sidebarSectionHeaders = make(map[string]int)
	m.sidebarFilesHeaderLine = -1
	var lines []string
	for _, section := range sections {
		if section.rows == 0 {
			continue
		}
		if len(lines) > 0 && gap > 0 {
			lines = append(lines, "")
		}
		if section.id == "files" {
			m.sidebarFilesHeaderLine = len(lines)
		} else {
			m.sidebarSectionHeaders[section.id] = len(lines)
		}
		visible := append([]string(nil), section.lines[:section.rows]...)
		if section.rows < len(section.lines) {
			if section.id == "model" && section.rows >= 3 {
				visible[section.rows-1] = section.lines[len(section.lines)-1]
			} else {
				last := section.rows - 1
				visible[last] = ansi.Truncate(visible[last], max(0, contentWidth-2), "") + " …"
			}
		}
		for _, line := range visible {
			lines = append(lines, ansi.Truncate(line, contentWidth, "…"))
		}
	}
	m.sidebarContent = strings.Join(lines, "\n")
	m.sidebarTotalLines = len(lines)
	m.sidebarContentWidth = contentWidth
	m.sidebarContentHeight = max(0, height-logoHeight())
	m.sidebarDrawLogo = sidebarLogo
	m.sidebarBrandLogoHeight = logoHeight()
	m.sidebarScrollable = false
	m.sidebarMaxOffsetVal = 0
	m.sidebarOffset = 0
	if m.focus == uiFocusSidebar {
		m.focus = uiFocusMain
		m.chat.Focus()
	}
}

// drawSidebar renders the chat sidebar with a fixed logo and a
// virtual-scrolling content area with an auto-hiding scrollbar. While the
// sidebar is focused, the scrollbar stays visible.
func (m *UI) drawSidebar(scr uv.Screen, area uv.Rectangle) {
	if m.session == nil || area.Dx() < 3 || area.Dy() < 3 {
		return
	}

	frameArea := area
	area = area.Inset(1)
	sidebarLogo := m.sidebarDrawLogo
	contentWidth := m.sidebarContentWidth
	contentHeight := m.sidebarContentHeight
	totalLines := m.sidebarTotalLines

	var logoRect, contentRect image.Rectangle
	layout.Vertical(
		layout.Len(max(0, area.Dy()-contentHeight)),
		layout.Fill(1),
	).Split(area).Assign(&logoRect, &contentRect)
	logoRect.Min.X = min(logoRect.Min.X+1, logoRect.Max.X)
	contentRect.Min.X = min(contentRect.Min.X+1, contentRect.Max.X)

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

	fillSurfaceBackground(scr, area, m.com.Styles.Sidebar.Background)
	if frameArea.Dx() >= 2 && frameArea.Dy() >= 2 {
		frame := lipgloss.NewStyle().Foreground(m.editorAccent()).Background(m.com.Styles.Sidebar.Background)
		uv.NewStyledString(frame.Render("╭"+strings.Repeat("─", frameArea.Dx()-2)+"╮")).Draw(scr, image.Rect(frameArea.Min.X, frameArea.Min.Y, frameArea.Max.X, frameArea.Min.Y+1))
		uv.NewStyledString(frame.Render("╰"+strings.Repeat("─", frameArea.Dx()-2)+"╯")).Draw(scr, image.Rect(frameArea.Min.X, frameArea.Max.Y-1, frameArea.Max.X, frameArea.Max.Y))

		controls := lipgloss.NewStyle().Foreground(lipgloss.Color("#000000")).Background(m.editorAccent())
		label := " ctrl+left show ctrl+right hide "
		uv.NewStyledString(controls.Width(frameArea.Dx()-2).Render(label)).Draw(scr, image.Rect(frameArea.Min.X+1, frameArea.Max.Y-1, frameArea.Max.X-1, frameArea.Max.Y))
		for y := frameArea.Min.Y + 1; y < frameArea.Max.Y-1; y++ {
			uv.NewStyledString(frame.Render("│")).Draw(scr, image.Rect(frameArea.Min.X, y, frameArea.Min.X+1, y+1))
			uv.NewStyledString(frame.Render("│")).Draw(scr, image.Rect(frameArea.Max.X-1, y, frameArea.Max.X, y+1))
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
