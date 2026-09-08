package model

import (
	"github.com/example-git/crux/internal/ui/dialog"
)

type previewModalEntry struct {
	PreviewMenu
	Create func(*Preview) (dialog.Dialog, error)
}

var previewModalRegistry = []previewModalEntry{
	{PreviewMenu{"codebase-index", "Codebase Index"}, func(p *Preview) (dialog.Dialog, error) {
		return dialog.NewPreviewCodebaseIndex(p.ui.com, p.data.CodebaseIndex), nil
	}},
	{PreviewMenu{"instructions", "Instructions"}, func(p *Preview) (dialog.Dialog, error) { return dialog.NewInstructions(p.ui.com), nil }},
	{PreviewMenu{"instructions-preview", "Instructions Preview"}, func(p *Preview) (dialog.Dialog, error) {
		return dialog.NewPreviewInstructionsContent(p.ui.com, p.data.Instructions, p.ui.width), nil
	}},
	{PreviewMenu{"commands", "Commands"}, func(p *Preview) (dialog.Dialog, error) {
		return dialog.NewCommands(p.ui.com, p.data.Session.ID, true, true, true, nil, nil)
	}},
	{PreviewMenu{"models", "Models"}, func(p *Preview) (dialog.Dialog, error) { return dialog.NewModels(p.ui.com, false) }},
	{PreviewMenu{"providers", "Providers"}, func(p *Preview) (dialog.Dialog, error) { return dialog.NewProviders(p.ui.com), nil }},
	{PreviewMenu{"sessions", "Sessions"}, func(p *Preview) (dialog.Dialog, error) { return dialog.NewSessions(p.ui.com, "fixture-session-0") }},
	{PreviewMenu{"tasks", "Background tasks"}, func(p *Preview) (dialog.Dialog, error) {
		return dialog.NewPreviewTasks(p.ui.com, p.data.Tasks, false), nil
	}},
	{PreviewMenu{"task-detail", "Task output detail"}, func(p *Preview) (dialog.Dialog, error) {
		return dialog.NewPreviewTasks(p.ui.com, p.data.Tasks, true), nil
	}},
	{PreviewMenu{"permissions", "Tool permission"}, func(p *Preview) (dialog.Dialog, error) {
		return dialog.NewPermissions(p.ui.com, p.data.Permission), nil
	}},
	{PreviewMenu{"reasoning", "Reasoning effort"}, func(p *Preview) (dialog.Dialog, error) { return dialog.NewReasoning(p.ui.com) }},
	{PreviewMenu{"notifications", "Notification settings"}, func(p *Preview) (dialog.Dialog, error) { return dialog.NewNotifications(p.ui.com), nil }},
	{PreviewMenu{"summarization", "Summarization settings"}, func(p *Preview) (dialog.Dialog, error) { return dialog.NewSummarization(p.ui.com), nil }},
	{PreviewMenu{"quit", "Quit confirmation"}, func(p *Preview) (dialog.Dialog, error) { return dialog.NewQuit(p.ui.com), nil }},
}

func previewModalMenus() []PreviewMenu {
	menus := []PreviewMenu{{"none", "None"}}
	for _, entry := range previewModalRegistry {
		menus = append(menus, entry.PreviewMenu)
	}
	return menus
}

// PreviewRegistry is the single discovery entry point for the embedded demo.
// Fixtures use native components; changing their production rendering is visible
// here on the next binary build. New tools must have a registered fixture (the
// factory coverage test enforces this). Menus use the same registry for controls.
func PreviewRegistry() map[string]any {
	return map[string]any{"provider": "Dummy Provider (local fixture)", "models": PreviewModels, "examples": PreviewExamples(), "modals": PreviewModals, "popovers": PreviewPopovers, "dataEndpoint": "/api/fixture", "renderer": "crux/internal/ui/model.UI.View"}
}
