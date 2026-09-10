package model

import (
	"cmp"
	"fmt"
	"image"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/charmtone"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/foundation/ultraviolet/layout"
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

// Sidebar sections own their spacing. Resource renderers pad blank rows to
// their width, so trimming newline bytes alone leaves an extra empty row.
func trimSidebarBlankLines(content string) string {
	lines := strings.Split(content, "\n")
	blank := func(line string) bool { return strings.TrimSpace(ansi.Strip(line)) == "" }
	for len(lines) > 0 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
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
		m.sidebarSession.setGeometry("", image.Rectangle{}, image.Rectangle{}, false)
		return
	}
	t := m.com.Styles
	contentWidth := max(m.layout.sidebar.Dx()-4, 1)
	height := max(0, m.layout.sidebar.Dy()-2)
	type section struct {
		id                    string
		lines                 []string
		rows                  int
		directoryLine, idLine int
	}
	var sections []section
	// Quota belongs to the provider header, not a collapsible resource section.
	// Keep it first so shrinking the sidebar trims other sections before usage.
	if usage := m.usageBars(contentWidth, true); usage != "" {
		lines := strings.Split(usage, "\n")
		sections = append(sections, section{id: "usage", lines: lines, rows: len(lines)})
	}
	add := func(id, title, body string) {
		marker := "▾ "
		if m.sidebarCollapsed[id] {
			marker = "▸ "
		}
		content := common.Section(t, t.Resource.Heading.Render(marker+title), contentWidth)
		body = trimSidebarBlankLines(body)
		if !m.sidebarCollapsed[id] && body != "" {
			content += "\n\n" + body
		}
		lines := strings.Split(content, "\n")
		sections = append(sections, section{id: id, lines: lines, rows: len(lines)})
	}
	body := func(content string) string {
		_, rest, _ := strings.Cut(content, "\n")
		return rest
	}
	add("model", "Model / Context", m.modelInfo(contentWidth))
	sessionBody, sessionID := m.sidebarSessionInfo(contentWidth)
	add("session", "Session", sessionBody)
	sessionSection := &sections[len(sections)-1]
	sessionSection.directoryLine, sessionSection.idLine = -1, -1
	if !m.sidebarCollapsed["session"] {
		sessionSection.directoryLine = 2
		if sessionID != "" {
			sessionSection.idLine = len(sessionSection.lines) - 1
		}
	}
	if options := m.com.Config().Options; options != nil && options.Debug {
		if authority := m.workspaceAuthorityInfo(contentWidth); authority != "" {
			add("authority", "Workspace Authority", authority)
		}
	}
	if count := fileChangeCount(m.sessionFiles); count > 0 {
		lines := strings.Split(m.filesInfo(m.com.Workspace.WorkingDir(), contentWidth, count, true), "\n")
		sections = append(sections, section{id: "files", lines: lines, rows: len(lines)})
	}
	if count := len(m.lspStates); count > 0 {
		add("lsp", "LSPs", body(m.lspInfo(contentWidth, count, false)))
	}
	if count := mcpCount(m.com.Config().MCP.Sorted(), m.mcpStates); count > 0 {
		add("mcp", "MCPs", body(m.mcpInfo(contentWidth, count, false)))
	}
	if count := len(m.skillStatusItems()); count > 0 {
		add("skills", "Skills", body(m.skillsInfo(contentWidth, count, false)))
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
			for row, line := range sections[i].lines {
				if sections[i].id == "session" {
					if row == sections[i].directoryLine {
						sections[i].directoryLine = len(lines)
					}
					if row == sections[i].idLine {
						sections[i].idLine = len(lines)
					}
				}
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
	for _, id := range []string{"skills", "mcp", "lsp", "authority", "files", "session", "model"} {
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
			if id == "session" {
				floor = min(5, sections[i].rows)
			}
			sections[i].rows -= min(max(0, total()+logoHeight()-height), sections[i].rows-floor)
		}
	}
	// At extreme heights, exhaust less important sections before touching the
	// provider quota. Removing one row from every section used to corrupt quota
	// lines while leaving lower-priority content visible.
	for _, id := range []string{"skills", "mcp", "lsp", "authority", "files", "session", "model", "usage"} {
		for i := range sections {
			if sections[i].id == id {
				sections[i].rows -= min(max(0, total()-height), sections[i].rows)
			}
		}
	}
	m.sidebarSectionHeaders = make(map[string]int)
	m.sidebarFilesHeaderLine = -1
	directoryLine, idLine := -1, -1
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
		} else if section.id != "usage" {
			m.sidebarSectionHeaders[section.id] = len(lines)
		}
		visible := append([]string(nil), section.lines[:section.rows]...)
		if section.rows < len(section.lines) {
			if section.id == "model" && section.rows >= 3 {
				visible[section.rows-1] = section.lines[len(section.lines)-1]
			} else if section.id == "session" && section.rows >= 3 && section.idLine >= 0 {
				visible[section.rows-1] = section.lines[section.idLine]
				section.idLine = section.rows - 1
			} else {
				last := section.rows - 1
				visible[last] = ansi.Truncate(visible[last], max(0, contentWidth-2), "") + " …"
			}
		}
		if section.id == "session" {
			if section.directoryLine >= 0 && section.directoryLine < len(visible) && section.directoryLine != section.idLine {
				directoryLine = len(lines) + section.directoryLine
			}
			if section.idLine >= 0 && section.idLine < len(visible) && contentWidth >= 4+ansi.StringWidth(sessionID) {
				idLine = len(lines) + section.idLine
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
	idRect := image.Rectangle{}
	if idLine >= 0 {
		x, y := m.layout.sidebar.Min.X+2+4, m.layout.sidebar.Min.Y+1+logoHeight()+idLine
		idRect = image.Rect(x, y, x+ansi.StringWidth(sessionID), y+1)
	}
	directoryRect := image.Rectangle{}
	if directoryLine >= 0 {
		x, y := m.layout.sidebar.Min.X+2, m.layout.sidebar.Min.Y+1+logoHeight()+directoryLine
		directoryRect = image.Rect(x, y, x+contentWidth, y+1)
	}
	overflow := ansi.StringWidth(sidebarSingleLine(m.com.Workspace.WorkingDir())) > max(0, contentWidth-2)
	m.sidebarSession.setGeometry(sessionID, idRect, directoryRect, overflow)
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
	m.drawSidebarSessionSelection(scr)
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
	if width <= 0 || u == nil || len(u.Windows) == 0 {
		return ""
	}
	t := m.com.Styles

	nameWidth := 0
	for _, w := range u.Windows {
		nameWidth = max(nameWidth, lipgloss.Width(w.Name))
	}
	suffixWidth := 0
	if withPercent {
		suffixWidth = lipgloss.Width(" 100% left")
	}
	nameWidth = min(nameWidth, max(1, width-6-suffixWidth))

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
			// Even a narrow surface should retain the quota value.
			percent := fmt.Sprintf("%d%%", remaining)
			name := ansi.Truncate(w.Name, max(0, width-lipgloss.Width(percent)-1), "…")
			line := strings.TrimSpace(name + " " + percent)
			lines = append(lines, t.Header.Percentage.Render(ansi.Truncate(line, width, "")))
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
		name := ansi.Truncate(w.Name, nameWidth, "…")
		name += strings.Repeat(" ", max(0, nameWidth-lipgloss.Width(name)))
		line := t.Header.Percentage.Render(name) +
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
