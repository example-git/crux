package model

import (
	"context"
	"errors"
	"fmt"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

// Only Update owns this queue. A command covers persistence and coordinator
// rebuild together, so a later selection cannot publish ahead of an older one.
type modelSelectionLane struct {
	active *modelSelectionOperation
	queued []*modelSelectionOperation
}
type modelSelectionOperation struct {
	workspace  workspace.Workspace
	generation uint64
	selection  dialog.ActionSelectModel
	onboarding bool
	control    string
	notice     string
}
type modelSelectionCompletedMsg struct {
	operation                     *modelSelectionOperation
	providerID, modelName, notice string
	modelType                     config.SelectedModelType
	needsInitialization           bool
	err                           error
}

func (m *UI) enqueueModelSelection(selection dialog.ActionSelectModel, control, notice string) tea.Cmd {
	selection.Model = selection.Model.Clone()
	operation := &modelSelectionOperation{workspace: m.com.Workspace, generation: m.modelSelectionGen, selection: selection, onboarding: m.state == uiOnboarding, control: control, notice: notice}
	if m.modelSelectionLanes == nil {
		m.modelSelectionLanes = make(map[workspace.Workspace]*modelSelectionLane)
	}
	lane := m.modelSelectionLanes[operation.workspace]
	if lane == nil {
		lane = &modelSelectionLane{}
		m.modelSelectionLanes[operation.workspace] = lane
	}
	if lane.active != nil {
		lane.queued = append(lane.queued, operation)
		return nil
	}
	lane.active = operation
	return runModelSelection(operation)
}

func runModelSelection(operation *modelSelectionOperation) tea.Cmd {
	// Everything below reads the captured operation and workspace. It never reads
	// or mutates the UI, even when the user replaces the current workspace.
	return func() tea.Msg {
		result := modelSelectionCompletedMsg{operation: operation, notice: operation.notice}
		selection, ws := operation.selection, operation.workspace
		cfg := ws.Config()
		if err := selection.ValidateProviderOwner(cfg); err != nil {
			result.err = err
			return result
		}
		if operation.control != "" {
			current, ok := cfg.Models[selection.ModelType]
			if !ok || current.Provider != selection.Model.Provider || current.Model != selection.Model.Model {
				result.err = errors.New("model changed before its control update; choose the control again")
				return result
			}
			switch operation.control {
			case "thinking":
				current.Think = !current.Think
				result.notice = "Thinking mode disabled"
				if current.Think {
					result.notice = "Thinking mode enabled"
				}
			case "reasoning":
				current.ReasoningEffort = selection.Model.ReasoningEffort
			default:
				result.err = errors.New("unknown model control operation")
				return result
			}
			selection.Model = current.Clone()
		}
		result.providerID, result.modelType, result.modelName = selection.Model.Provider, selection.ModelType, selection.Model.Model
		if model := cfg.GetModel(selection.Model.Provider, selection.Model.Model); model != nil && model.Name != "" {
			result.modelName = model.Name
		}
		state, err := ws.UpdatePreferredModel(config.ScopeGlobal, selection.ModelType, selection.Model, selection.ProviderOwner)
		if err != nil {
			result.err = err
			return result
		}
		if operation.onboarding {
			if local, ok := ws.(interface{ Store() *config.ConfigStore }); ok {
				local.Store().SetupAgents()
			}
			if err = ws.InitCoderAgent(context.Background()); err == nil {
				result.needsInitialization, err = ws.ProjectNeedsInitialization()
			}
		} else {
			err = ws.UpdateAgentModel(context.Background(), state)
		}
		result.err = err
		return result
	}
}

func (m *UI) completeModelSelection(result modelSelectionCompletedMsg) tea.Cmd {
	operation := result.operation
	if operation == nil {
		return nil
	}
	lane := m.modelSelectionLanes[operation.workspace]
	if lane == nil || lane.active != operation {
		return nil
	}
	lane.active = nil
	var cmds []tea.Cmd
	// Always release the originating workspace lane, including stale and failed
	// completions. Newer work starts only after this full operation has returned.
	if len(lane.queued) > 0 {
		lane.active = lane.queued[0]
		lane.queued = lane.queued[1:]
		cmds = append(cmds, runModelSelection(lane.active))
	} else {
		delete(m.modelSelectionLanes, operation.workspace)
	}
	if operation.workspace != m.com.Workspace || operation.generation != m.modelSelectionGen {
		return tea.Batch(cmds...)
	}
	if result.err != nil {
		return tea.Batch(append(cmds, util.ReportError(result.err))...)
	}
	if result.modelType == config.SelectedModelTypeLarge {
		m.applyThemeForProvider(result.providerID)
	}
	if operation.onboarding && m.state == uiOnboarding {
		if result.needsInitialization {
			m.setState(uiInitialize, uiFocusEditor)
		} else {
			m.setState(uiLanding, uiFocusEditor)
		}
		if cmd := m.continueStartup(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	m.invalidateBusyCaches()
	if cmd := m.dispatchBusyRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if result.notice != "" {
		cmds = append(cmds, util.ReportInfo(result.notice))
	} else {
		cmds = append(cmds, util.ReportInfo(fmt.Sprintf("%s model changed to %s", result.modelType, result.modelName)))
	}
	if result.modelType == config.SelectedModelTypeLarge {
		cmds = append(cmds, m.fetchProviderUsageFor(result.providerID))
	}
	return tea.Batch(cmds...)
}

func (m *UI) queueModelControl(control, effort string) tea.Cmd {
	cfg := m.com.Config()
	if cfg == nil {
		return util.ReportError(errors.New("configuration not found"))
	}
	agent, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return util.ReportError(errors.New("agent configuration not found"))
	}
	selected := cfg.Models[agent.Model].Clone()
	owner, err := selectedModelOwner(cfg, selected)
	if err != nil {
		return util.ReportError(err)
	}
	notice := ""
	if control == "reasoning" {
		selected.ReasoningEffort = effort
		notice = "Reasoning effort set to " + effort
	}
	m.modelSelectionGen++
	if m.cancelCopilotImport != nil {
		m.cancelCopilotImport()
		m.cancelCopilotImport = nil
	}
	return m.enqueueModelSelection(dialog.ActionSelectModel{Provider: (&config.ProviderConfig{ID: selected.Provider}).ToProvider(), Model: selected, ModelType: agent.Model, ProviderOwner: owner, ProviderOwnerSet: true}, control, notice)
}
