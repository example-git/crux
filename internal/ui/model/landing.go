package model

import (
	"encoding/json"
	"image"

	"github.com/example-git/crux/internal/config"

	"charm.land/lipgloss/v2"
	"github.com/example-git/crux/foundation/ultraviolet/layout"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/workspace"
)

// selectedLargeModel returns the currently selected large language model as
// memoized by the off-thread busy/agent probe (see workspace_cache.go), or
// nil when the agent isn't ready. It must never probe the workspace: it is
// called on every frame and AgentIsReady/AgentModel are synchronous HTTP
// round-trips in client/server mode.
func (m *UI) selectedLargeModel() *workspace.AgentModel {
	if m.session != nil && len(m.session.Models) > 0 {
		data := string(m.session.Models)
		if data != m.sessionModelData || m.sessionModelWorkspace != m.com.Workspace {
			m.sessionModelData, m.sessionLargeModel = data, nil
			m.sessionModelWorkspace = m.com.Workspace
			var models config.AgentModelState
			if json.Unmarshal(m.session.Models, &models) == nil && models.Large != nil {
				selected := models.Large.Model
				m.sessionLargeModel = &workspace.AgentModel{ModelCfg: selected}
				if cfg := m.com.Config(); cfg != nil {
					if model := cfg.GetModel(selected.Provider, selected.Model); model != nil {
						m.sessionLargeModel.CatalogModel = *model
					} else {
						m.sessionLargeModel.CatalogModel.Name = selected.Model
					}
				}
			}
		}
		return m.sessionLargeModel
	}
	if m.agentReady {
		model := m.agentModel
		return &model
	}
	return nil
}

// landingView renders the landing page view showing the current working
// directory, model information, and LSP/MCP status in a two-column layout.
func (m *UI) landingView() string {
	t := m.com.Styles
	width := m.layout.main.Dx()
	cwd := common.PrettyPath(t, m.com.Workspace.WorkingDir(), width)

	parts := []string{
		cwd,
	}

	parts = append(parts, "", m.modelInfo(width))
	infoSection := lipgloss.JoinVertical(lipgloss.Left, parts...)

	var remainingHeightArea image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(infoSection)+1),
		layout.Fill(1),
	).Split(m.layout.main).Assign(new(image.Rectangle), &remainingHeightArea)

	mcpLspSectionWidth := min(30, (width-2)/3)

	lspSection := m.lspInfo(mcpLspSectionWidth, max(1, remainingHeightArea.Dy()), false)
	mcpSection := m.mcpInfo(mcpLspSectionWidth, max(1, remainingHeightArea.Dy()), false)
	skillsSection := m.skillsInfo(mcpLspSectionWidth, max(1, remainingHeightArea.Dy()), false)

	content := lipgloss.JoinHorizontal(lipgloss.Left, lspSection, " ", mcpSection, " ", skillsSection)

	return lipgloss.NewStyle().
		Width(width).
		Height(m.layout.main.Dy() - 1).
		PaddingTop(1).
		Render(
			lipgloss.JoinVertical(lipgloss.Left, infoSection, "", content),
		)
}
