package dialog

import (
	"context"
	"image"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
)

type instructionsTestWorkspace struct {
	workspace.Workspace
	cfg         *config.Config
	workingDir  string
	fields      map[string]any
	surfaces    []providerregistry.Surface
	snapshot    agent.InstructionSnapshot
	snapshotErr error
}

func (w *instructionsTestWorkspace) Config() *config.Config {
	return w.cfg
}

func (w *instructionsTestWorkspace) ProviderSurfaces() []providerregistry.Surface {
	if w.surfaces != nil {
		return w.surfaces
	}
	return config.ProviderSurfaces(w.cfg)
}

func (w *instructionsTestWorkspace) WorkingDir() string {
	return w.workingDir
}

func (w *instructionsTestWorkspace) SetConfigField(_ config.Scope, key string, value any) error {
	w.fields[key] = value
	return nil
}

func (w *instructionsTestWorkspace) RemoveConfigField(_ config.Scope, key string) error {
	w.fields[key] = nil
	return nil
}

func (w *instructionsTestWorkspace) AgentInstructionSnapshot(context.Context) (agent.InstructionSnapshot, error) {
	return w.snapshot, w.snapshotErr
}

func newInstructionsTestDialog(t *testing.T, providerID, profile string) (*Instructions, *instructionsTestWorkspace) {
	t.Helper()
	return newInstructionsTestDialogForModel(t, providerID, "test-model", profile)
}

func newInstructionsTestDialogForModel(t *testing.T, providerID, modelID, profile string) (*Instructions, *instructionsTestWorkspace) {
	t.Helper()
	ws := &instructionsTestWorkspace{
		cfg: &config.Config{
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: providerID, Model: modelID},
			},
			Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
				providerID: {ID: providerID, ToolingInstructions: profile},
			}),
			Options: &config.Options{},
		},
		workingDir: t.TempDir(),
		fields:     make(map[string]any),
	}
	if providerID == codex.ID {
		registry, err := providerregistry.New(providerregistry.Integrated()...)
		if err != nil {
			t.Fatal(err)
		}
		ws.surfaces = registry.Surfaces(
			[]catalog.Provider{codex.CatalogProvider()},
			map[string]string{providerID: modelID},
		)
	}
	theme := styles.ThemeForProvider(providerID)
	return NewInstructions(&common.Common{Workspace: ws, Styles: &theme}), ws
}

func instructionItemIndex(items []instrItem, kind instrItemKind, id string) int {
	for index, item := range items {
		if item.kind == kind && item.id == id {
			return index
		}
	}
	return -1
}

func TestInstructionsProviderContextHasNoOverrideToggle(t *testing.T) {
	dialog, _ := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsCrux)
	index := instructionItemIndex(dialog.items, instrProviderContextEdit, "edit-provider-context")
	if index < 0 {
		t.Fatal("provider context edit action missing")
	}
	if dialog.providerContextPath == "" || !strings.HasSuffix(dialog.providerContextPath, filepath.Join("instructions", codex.ID+".txt")) {
		t.Fatalf("provider context path = %q", dialog.providerContextPath)
	}
	for _, item := range dialog.items {
		if item.id == "system-prompt-override" {
			t.Fatal("obsolete provider override toggle is still present")
		}
	}
}

func TestInstructionsMetadataControlsOnlyAvailableForGPT56(t *testing.T) {
	for _, tc := range []struct {
		model     string
		available bool
	}{
		{model: "gpt-5.6", available: true},
		{model: "gpt-5.6-sol", available: true},
		{model: "gpt-6-astra", available: true},
		{model: "gpt-6-other", available: false},
		{model: "gpt-5.5-codex", available: false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			dialog, _ := newInstructionsTestDialogForModel(t, codex.ID, tc.model, config.ToolingInstructionsCrux)
			for _, control := range []string{"options.response_verbosity", "options.analysis_effort"} {
				index := instructionItemIndex(dialog.items, instrMetadataValue, control)
				if index < 0 {
					t.Fatalf("%s control missing", control)
				}
				if got := !dialog.items[index].unavailable; got != tc.available {
					t.Fatalf("%s control availability = %t, want %t", control, got, tc.available)
				}
			}
		})
	}
}

func TestInstructionsMetadataValuePersistsAndCanBeUnset(t *testing.T) {
	dialog, ws := newInstructionsTestDialogForModel(t, codex.ID, "gpt-5.6-sol", config.ToolingInstructionsCrux)
	index := instructionItemIndex(dialog.items, instrMetadataValue, "options.analysis_effort")
	dialog.cursor = index
	dialog.metadataInput.SetValue("max")
	dialog.editingMetadata = true
	if _, ok := dialog.saveMetadataValue().(ActionInstructionsChanged); !ok {
		t.Fatal("metadata save did not report instruction change")
	}
	if got := ws.fields["options.analysis_effort"]; got != "max" {
		t.Fatalf("persisted analysis effort = %#v, want max", got)
	}

	dialog.metadataInput.SetValue("")
	dialog.editingMetadata = true
	if _, ok := dialog.saveMetadataValue().(ActionInstructionsChanged); !ok {
		t.Fatal("metadata removal did not report instruction change")
	}
	if got, ok := ws.fields["options.analysis_effort"]; !ok || got != nil {
		t.Fatalf("removed analysis effort = %#v, present = %t", got, ok)
	}
}

func TestInstructionsRejectsInvalidMetadataValue(t *testing.T) {
	dialog, ws := newInstructionsTestDialogForModel(t, codex.ID, "gpt-5.6-sol", config.ToolingInstructionsCrux)
	dialog.cursor = instructionItemIndex(dialog.items, instrMetadataValue, "options.analysis_effort")
	dialog.metadataInput.SetValue("unlimited")
	dialog.editingMetadata = true
	if _, ok := dialog.saveMetadataValue().(ActionCmd); !ok {
		t.Fatal("invalid metadata value did not return an error command")
	}
	if len(ws.fields) != 0 {
		t.Fatalf("invalid metadata value persisted fields: %#v", ws.fields)
	}
	if !dialog.editingMetadata {
		t.Fatal("invalid metadata value closed the editor")
	}
}

func TestInstructionsNativeToggleMarksReplacedSections(t *testing.T) {
	dialog, _ := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsNative)
	nativeIndex := instructionItemIndex(dialog.items, instrNativeToggle, config.ToolingInstructionsNative)
	if nativeIndex < 0 {
		t.Fatal("native toggle missing")
	}
	if dialog.items[nativeIndex].disabled {
		t.Fatal("native toggle is not selected")
	}
	if dialog.items[nativeIndex].label != "Use "+codex.Name+" native tooling instructions" {
		t.Fatalf("native toggle label = %q", dialog.items[nativeIndex].label)
	}
	for _, item := range dialog.items {
		if item.kind == instrSection && !item.replaced {
			t.Fatalf("section %q is not marked as replaced", item.id)
		}
	}
}

func TestInstructionsUsesProviderDeclaredNativeDefault(t *testing.T) {
	const providerID = "synthetic-native"
	ws := &instructionsTestWorkspace{
		cfg: &config.Config{
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: providerID, Model: "test-model"},
			},
			Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
				providerID: {ID: providerID},
			}),
			Options: &config.Options{},
		},
		workingDir: t.TempDir(),
		fields:     make(map[string]any),
		surfaces: []providerregistry.Surface{{
			ID: providerID, Name: "Synthetic", Available: true,
			Instructions: &providerregistry.InstructionSurface{
				Default:          "stock",
				SelectionDefault: config.ToolingInstructionsNative,
				Profiles:         map[string]string{"stock": "Exact stock instructions"},
			},
		}},
	}
	theme := styles.ThemeForProvider(providerID)
	dialog := NewInstructions(&common.Common{Workspace: ws, Styles: &theme})
	nativeIndex := instructionItemIndex(dialog.items, instrNativeToggle, config.ToolingInstructionsNative)
	if nativeIndex < 0 || dialog.items[nativeIndex].disabled {
		t.Fatalf("declared native default selection incorrect: %#v", dialog.items)
	}
	if dialog.items[nativeIndex].label != "Use Synthetic native tooling instructions (default)" {
		t.Fatalf("native default label = %q", dialog.items[nativeIndex].label)
	}
	ws.cfg.Providers.Set(providerID, config.ProviderConfig{ID: providerID, ToolingInstructions: config.ToolingInstructionsCrux})
	dialog = NewInstructions(&common.Common{Workspace: ws, Styles: &theme})
	nativeIndex = instructionItemIndex(dialog.items, instrNativeToggle, config.ToolingInstructionsNative)
	if nativeIndex < 0 || !dialog.items[nativeIndex].disabled {
		t.Fatalf("explicit Crux selection did not override provider default: %#v", dialog.items)
	}
	for _, item := range dialog.items {
		if item.kind == instrSection && item.replaced {
			t.Fatalf("section %q remained replaced with native disabled", item.id)
		}
	}
}

func TestInstructionsNativeTogglePersistsProviderSetting(t *testing.T) {
	dialog, ws := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsCrux)
	dialog.cursor = instructionItemIndex(dialog.items, instrNativeToggle, config.ToolingInstructionsNative)
	if _, ok := dialog.toggle().(ActionInstructionsChanged); !ok {
		t.Fatal("native toggle did not report instruction change")
	}
	key := "providers." + codex.ID + ".tooling_instructions"
	if got := ws.fields[key]; got != config.ToolingInstructionsNative {
		t.Fatalf("persisted profile = %#v, want %q", got, config.ToolingInstructionsNative)
	}
	if dialog.items[dialog.cursor].disabled {
		t.Fatal("native toggle was not selected")
	}
	for _, item := range dialog.items {
		if item.kind == instrSection && !item.replaced {
			t.Fatalf("section %q was not grayed after enabling native", item.id)
		}
	}
}

func TestInstructionsNativeToggleHiddenWithoutNonblankProfile(t *testing.T) {
	dialog, _ := newInstructionsTestDialog(t, "openai", config.ToolingInstructionsCrux)
	if index := instructionItemIndex(dialog.items, instrNativeToggle, config.ToolingInstructionsNative); index >= 0 {
		t.Fatalf("native toggle shown for unsupported provider: %#v", dialog.items[index])
	}

	const providerID = "blank-native"
	ws := &instructionsTestWorkspace{
		cfg: &config.Config{
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: providerID, Model: "test-model"},
			},
			Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: {ID: providerID}}),
			Options:   &config.Options{},
		},
		workingDir: t.TempDir(),
		fields:     make(map[string]any),
		surfaces: []providerregistry.Surface{{
			ID: providerID, Name: "Blank", Available: true,
			Instructions: &providerregistry.InstructionSurface{
				Default:  "stock",
				Profiles: map[string]string{"stock": "  \n"},
			},
		}},
	}
	theme := styles.ThemeForProvider(providerID)
	dialog = NewInstructions(&common.Common{Workspace: ws, Styles: &theme})
	if index := instructionItemIndex(dialog.items, instrNativeToggle, config.ToolingInstructionsNative); index >= 0 {
		t.Fatalf("native toggle shown for blank profile: %#v", dialog.items[index])
	}
}

func TestInstructionsPreviewUsesWorkspaceSnapshot(t *testing.T) {
	dialog, ws := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsNative)
	ws.snapshot = agent.InstructionSnapshot{
		ProviderID: codex.ID,
		ModelID:    "test-model",
		Policy:     fantasy.InstructionPolicyAnthropic,
		Sections: []agent.InstructionSnapshotSection{
			{Kind: fantasy.InstructionKindTooling, Stability: fantasy.InstructionStabilityStatic, Text: "tooling"},
			{Kind: fantasy.InstructionKindEnvironment, Stability: fantasy.InstructionStabilityStatic, Text: "environment", CacheBoundary: true},
			{Kind: fantasy.InstructionKindProviderContext, Stability: fantasy.InstructionStabilityDynamic, Text: "provider context"},
		},
	}
	dialog.cursor = instructionItemIndex(dialog.items, instrPreview, "preview")
	action, ok := dialog.toggle().(ActionCmd)
	if !ok || action.Cmd == nil {
		t.Fatalf("preview action = %#v", action)
	}
	preview, ok := action.Cmd().(ActionPreviewInstructions)
	if !ok {
		t.Fatalf("preview message = %#v", action.Cmd())
	}
	if got, want := len(preview.Sections), 3; got != want {
		t.Fatalf("preview section count = %d, want %d", got, want)
	}
	if got, want := preview.Sections[0].Label, "Cached · Tooling"; got != want {
		t.Fatalf("first label = %q, want %q", got, want)
	}
	if got, want := preview.Sections[1].Label, "Cached · Environment · boundary"; got != want {
		t.Fatalf("boundary label = %q, want %q", got, want)
	}
	if got, want := preview.Sections[2].Label, "Uncached · Provider context"; got != want {
		t.Fatalf("dynamic label = %q, want %q", got, want)
	}
	for _, section := range preview.Sections {
		if section.Toggleable {
			t.Fatalf("effective section %q is toggleable", section.Label)
		}
	}
}

func TestInstructionsCursorWrapsBetweenSelectableOptions(t *testing.T) {
	dialog, _ := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsCrux)
	var selectable []int
	for index, item := range dialog.items {
		if dialog.selectable(item) {
			selectable = append(selectable, index)
		}
	}
	if len(selectable) < 2 {
		t.Fatalf("selectable options = %#v", selectable)
	}

	dialog.cursor = selectable[0]
	dialog.moveCursor(-1)
	if dialog.cursor != selectable[len(selectable)-1] {
		t.Fatalf("cursor after wrapping up = %d, want %d", dialog.cursor, selectable[len(selectable)-1])
	}
	dialog.moveCursor(1)
	if dialog.cursor != selectable[0] {
		t.Fatalf("cursor after wrapping down = %d, want %d", dialog.cursor, selectable[0])
	}
}

func TestInstructionsMenuUsesBoundedScrollableViewport(t *testing.T) {
	dialog, _ := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsCrux)
	const width = 50
	const height = 12
	screen := uv.NewScreenBuffer(width, height)
	dialog.Draw(screen, image.Rect(0, 0, width, height))
	if dialog.viewport.Height() <= 0 || dialog.viewport.TotalLineCount() <= dialog.viewport.Height() {
		t.Fatalf("viewport height=%d total=%d", dialog.viewport.Height(), dialog.viewport.TotalLineCount())
	}

	dialog.moveCursor(-1)
	dialog.Draw(screen, image.Rect(0, 0, width, height))
	if dialog.viewport.YOffset() == 0 {
		t.Fatal("viewport did not scroll to wrapped last option")
	}
}

func TestInstructionPreviewSectionsUseStaticAndDynamicGroups(t *testing.T) {
	sections := instructionPreviewSections(agent.InstructionSnapshot{
		Policy: fantasy.InstructionPolicyGeneric,
		Sections: []agent.InstructionSnapshotSection{
			{Kind: fantasy.InstructionKindTooling, Stability: fantasy.InstructionStabilityStatic, Text: "tooling"},
			{Kind: fantasy.InstructionKindRuntime, Stability: fantasy.InstructionStabilityDynamic, Text: "runtime"},
		},
	})
	if got, want := sections[0].Label, "Static · Tooling"; got != want {
		t.Fatalf("static label = %q, want %q", got, want)
	}
	if got, want := sections[1].Label, "Dynamic · Runtime"; got != want {
		t.Fatalf("dynamic label = %q, want %q", got, want)
	}
}

func TestInstructionsPreviewUsesBoundedScrollableViewport(t *testing.T) {
	previewStyles := styles.ThemeForProvider("")
	preview := NewInstructionsPreview(
		&common.Common{Styles: &previewStyles},
		[]InstructionPreviewSection{{Label: "Dynamic · Runtime", Content: strings.Repeat("long preview line\n", 80)}},
		50,
	)
	preview.markdown = false
	msg := preview.StartLoading()()
	preview.HandleMsg(msg)
	const width = 50
	const height = 12
	screen := uv.NewScreenBuffer(width, height)
	preview.Draw(screen, image.Rect(0, 0, width, height))
	if preview.viewport.Height() <= 0 || preview.viewport.Height() >= height {
		t.Fatalf("viewport height = %d for terminal height %d", preview.viewport.Height(), height)
	}
	if preview.viewport.TotalLineCount() <= preview.viewport.Height() {
		t.Fatalf("viewport total=%d height=%d", preview.viewport.TotalLineCount(), preview.viewport.Height())
	}
}

func TestInstructionsPreviewFrameFitsAreaAndToggleChangesFormat(t *testing.T) {
	previewStyles := styles.ThemeForProvider("")
	preview := NewInstructionsPreview(
		&common.Common{Styles: &previewStyles},
		[]InstructionPreviewSection{
			{Label: "Static · Tooling", Content: "# Tooling\n\n<critical_rules>\n1. Read relevant code and project instructions before editing.\n2. Complete every requested part.\n</critical_rules>"},
			{Label: "Dynamic · Project state", Content: "<project_context>\n<file path=\"/Users/example/.ai-cli/project-prompts/example/instructions.txt\">\nProject instructions\n</file>\n</project_context>"},
		},
		72,
	)
	preview.HandleMsg(preview.StartLoading()())
	area := image.Rect(0, 0, 72, 18)
	markdownView := preview.renderView(area)
	width, height := lipgloss.Size(markdownView)
	if got, want := width, preview.dialogWidth(area.Dx()); got != want {
		t.Fatalf("markdown preview width = %d, want %d", got, want)
	}
	if got, want := height, 14; got != want {
		t.Fatalf("markdown preview height = %d, want %d", got, want)
	}

	action, ok := preview.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}).(ActionCmd)
	if !ok || action.Cmd == nil {
		t.Fatalf("toggle action = %#v", action)
	}
	preview.HandleMsg(action.Cmd())
	textView := preview.renderView(area)
	width, height = lipgloss.Size(textView)
	if got, want := width, preview.dialogWidth(area.Dx()); got != want {
		t.Fatalf("text preview width = %d, want %d", got, want)
	}
	if got, want := height, 14; got != want {
		t.Fatalf("text preview height = %d, want %d", got, want)
	}
	if markdownView == textView {
		t.Fatal("format toggle did not change the rendered preview")
	}
	if text := ansi.Strip(textView); !strings.Contains(text, "# Tooling") || !strings.Contains(text, "  Text") {
		t.Fatalf("text preview did not preserve raw content and mode: %q", text)
	}
}

func TestInstructionsPreviewSectionPaneFitsAssignedWidth(t *testing.T) {
	previewStyles := styles.ThemeForProvider("")
	preview := NewInstructionsPreview(
		&common.Common{Styles: &previewStyles},
		[]InstructionPreviewSection{{Label: "Static · Tooling", Content: "tooling"}},
		100,
	)
	pane := preview.sectionsView(&previewStyles, instructionsPreviewSectionPaneWidth, 8)
	if got := lipgloss.Width(pane); got > instructionsPreviewSectionPaneWidth {
		t.Fatalf("section pane width = %d, want <= %d", got, instructionsPreviewSectionPaneWidth)
	}
}

func TestInstructionsPreviewFitsNarrowShortArea(t *testing.T) {
	previewStyles := styles.ThemeForProvider("")
	preview := NewInstructionsPreview(
		&common.Common{Styles: &previewStyles},
		[]InstructionPreviewSection{
			{Label: "Static · Tooling", Content: strings.Repeat("tooling content ", 20)},
			{Label: "Dynamic · Runtime", Content: strings.Repeat("runtime content ", 20)},
		},
		36,
	)
	preview.HandleMsg(preview.StartLoading()())
	area := image.Rect(0, 0, 36, 6)
	width, height := lipgloss.Size(preview.renderView(area))
	if width > area.Dx() || height > area.Dy() {
		t.Fatalf("preview size = %dx%d, area = %dx%d", width, height, area.Dx(), area.Dy())
	}
}

func TestInstructionsPreviewRerendersAtResizedWidth(t *testing.T) {
	previewStyles := styles.ThemeForProvider("")
	preview := NewInstructionsPreview(
		&common.Common{Styles: &previewStyles},
		[]InstructionPreviewSection{
			{Label: "Static · Tooling", Content: "tooling"},
			{Label: "Dynamic · Runtime", Content: "runtime"},
		},
		100,
	)
	preview.HandleMsg(preview.StartLoading()())
	action, ok := preview.HandleMsg(tea.WindowSizeMsg{Width: 50, Height: 12}).(ActionCmd)
	if !ok || action.Cmd == nil {
		t.Fatalf("resize action = %#v", action)
	}
	message, ok := action.Cmd().(instructionsPreviewRenderedMsg)
	if !ok {
		t.Fatalf("resize message = %#v", action.Cmd())
	}
	if got, want := message.key.width, preview.contentWidth(50); got != want {
		t.Fatalf("render width = %d, want %d", got, want)
	}
	preview.HandleMsg(message)
	if _, ok := preview.rendered[message.key]; !ok {
		t.Fatal("resized preview was not cached")
	}
}

func TestTextPreviewDoesNotInsertSectionHeaders(t *testing.T) {
	sections := []InstructionPreviewSection{
		{Label: "First toggle", Content: "first line"},
		{Label: "Disabled toggle", Content: "hidden line", Disabled: true},
		{Label: "Project instructions", Content: "# Existing heading\n\nproject line"},
	}
	rendered := renderInstructionsPreviewSections(nil, sections, 80, false)
	const want = "first line\n\n# Existing heading\n\nproject line"
	if rendered.content != want {
		t.Fatalf("rendered content = %q, want %q", rendered.content, want)
	}
	if len(rendered.sectionLines) != 3 || rendered.sectionLines[0] != 0 || rendered.sectionLines[1] != -1 || rendered.sectionLines[2] != 2 {
		t.Fatalf("section offsets = %#v, want []int{0, -1, 2}", rendered.sectionLines)
	}
}

func TestPreviewToggleUpdatesSectionAndRequestsRender(t *testing.T) {
	previewStyles := styles.ThemeForProvider("")
	preview := NewInstructionsPreview(
		&common.Common{Styles: &previewStyles},
		[]InstructionPreviewSection{{
			ID:         "testing",
			Label:      "Testing",
			Content:    "test content",
			Toggleable: true,
		}},
		80,
	)
	action, ok := preview.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}).(ActionPreviewInstructionSectionToggled)
	if !ok {
		t.Fatalf("action = %#v", action)
	}
	if action.ID != "testing" || !action.Disabled || action.Cmd == nil {
		t.Fatalf("toggle action = %#v", action)
	}
	if !preview.sections[0].Disabled {
		t.Fatal("preview section was not disabled")
	}
}

func TestPreviewViewportPaddingAllowsSectionsAtTop(t *testing.T) {
	lines := previewViewportLines("one\ntwo", 12)
	if got, want := lines[:4], []string{"", "", "one", "two"}; !slices.Equal(got, want) {
		t.Fatalf("preview lines = %#v, want %#v", got, want)
	}
	if got, want := len(lines), 15; got != want {
		t.Fatalf("line count = %d, want %d", got, want)
	}
}

func TestPreviewSectionOffsetsUseViewportPhysicalLines(t *testing.T) {
	sections := []InstructionPreviewSection{
		{Content: "1234567890123456789012345"},
		{Content: "second section"},
	}
	rendered := renderInstructionsPreviewSections(nil, sections, 10, false)
	if got, want := rendered.sectionLines, []int{0, 4}; !slices.Equal(got, want) {
		t.Fatalf("section offsets = %#v, want %#v", got, want)
	}
	if got, want := strings.Split(rendered.content, "\n")[:3], []string{"1234567890", "1234567890", "12345"}; !slices.Equal(got, want) {
		t.Fatalf("wrapped lines = %#v, want %#v", got, want)
	}

	preview := &InstructionsPreview{
		viewport:      viewport.New(),
		sections:      sections,
		sectionCursor: 1,
		viewportKey:   previewRenderKey{width: 10},
		rendered: map[previewRenderKey]instructionsPreviewRender{
			{width: 10}: rendered,
		},
	}
	preview.viewport.SetWidth(10)
	preview.viewport.SetHeight(3)
	preview.viewport.SetContentLines(previewViewportLines(rendered.content, 3))
	preview.jumpToSection()
	if got, want := preview.viewport.YOffset(), instructionsPreviewTopPadding+4; got != want {
		t.Fatalf("viewport offset = %d, want %d", got, want)
	}
}

func TestPreviewSectionOffsetsDoNotAccumulateSeparatorDrift(t *testing.T) {
	sections := make([]InstructionPreviewSection, 12)
	for index := range sections {
		sections[index].Content = "section"
	}
	rendered := renderInstructionsPreviewSections(nil, sections, 80, false)
	lines := strings.Split(rendered.content, "\n")
	for index, offset := range rendered.sectionLines {
		if offset >= len(lines) || lines[offset] != "section" {
			t.Fatalf("section %d offset = %d, line = %q", index, offset, lines[offset])
		}
	}
}

func TestMarkdownPreviewJumpUsesRenderedPhysicalLines(t *testing.T) {
	previewStyles := styles.ThemeForProvider("")
	sections := []InstructionPreviewSection{
		{Content: "This paragraph is deliberately long enough to wrap across several rendered lines at this narrow width."},
		{Content: "SECOND SECTION MARKER"},
	}
	const width = 24
	rendered := renderInstructionsPreviewSections(&previewStyles, sections, width, true)
	if rendered.sectionLines[1] <= 2 {
		t.Fatalf("second section offset = %d, expected wrapped first section", rendered.sectionLines[1])
	}

	preview := &InstructionsPreview{
		viewport:      viewport.New(),
		sections:      sections,
		sectionCursor: 1,
		viewportKey:   previewRenderKey{width: width, markdown: true},
		rendered: map[previewRenderKey]instructionsPreviewRender{
			{width: width, markdown: true}: rendered,
		},
	}
	preview.viewport.SetWidth(width)
	preview.viewport.SetHeight(4)
	preview.viewport.SetContentLines(previewViewportLines(rendered.content, 4))
	preview.jumpToSection()

	firstVisibleLine, _, _ := strings.Cut(ansi.Strip(preview.viewport.View()), "\n")
	if !strings.Contains(firstVisibleLine, "SECOND SECTION MARKER") {
		t.Fatalf("first visible line = %q, expected second section", firstVisibleLine)
	}
}
