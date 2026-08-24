package dialog

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent/prompt"
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
	cfg        *config.Config
	workingDir string
	fields     map[string]any
	surfaces   []providerregistry.Surface
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
			[]catwalk.Provider{codex.CatwalkProvider()},
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

func TestInstructionsProviderOverrideTogglePersistsSetting(t *testing.T) {
	dialog, ws := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsCrux)
	index := instructionItemIndex(dialog.items, instrOverrideToggle, "system-prompt-override")
	if index < 0 {
		t.Fatal("provider override toggle missing")
	}
	dialog.cursor = index
	if _, ok := dialog.toggle().(ActionInstructionsChanged); !ok {
		t.Fatal("provider override toggle did not report instruction change")
	}
	if got := ws.fields["options.system_prompt_override"]; got != true {
		t.Fatalf("persisted override setting = %#v, want true", got)
	}
	if dialog.items[index].disabled {
		t.Fatal("provider override was not enabled")
	}
}

func TestInstructionsMetadataControlsOnlyAvailableForGPT56(t *testing.T) {
	for _, tc := range []struct {
		model     string
		available bool
	}{
		{model: "gpt-5.6", available: true},
		{model: "gpt-5.6-sol", available: true},
		{model: "gpt-5.5-codex", available: false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			dialog, _ := newInstructionsTestDialogForModel(t, codex.ID, tc.model, config.ToolingInstructionsCrux)
			index := instructionItemIndex(dialog.items, instrMetadataValue, "options.response_verbosity")
			if index < 0 {
				t.Fatal("oververbosity control missing")
			}
			if got := !dialog.items[index].unavailable; got != tc.available {
				t.Fatalf("control availability = %t, want %t", got, tc.available)
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

func TestInstructionsToolingProfilesForSupportedProviders(t *testing.T) {
	for _, providerID := range []string{codex.ID} {
		t.Run(providerID, func(t *testing.T) {
			dialog, _ := newInstructionsTestDialog(t, providerID, config.ToolingInstructionsNative)
			cruxIndex := instructionItemIndex(dialog.items, instrToolingProfile, config.ToolingInstructionsCrux)
			nativeIndex := instructionItemIndex(dialog.items, instrToolingProfile, config.ToolingInstructionsNative)
			if cruxIndex < 0 || nativeIndex < 0 {
				t.Fatalf("tooling profile rows missing: %#v", dialog.items)
			}
			if dialog.items[cruxIndex].label != "Crux instructions" {
				t.Fatalf("Crux profile label = %q", dialog.items[cruxIndex].label)
			}
			if dialog.items[nativeIndex].unavailable {
				t.Fatalf("native profile unavailable for %q", providerID)
			}
			if dialog.items[nativeIndex].disabled || !dialog.items[cruxIndex].disabled {
				t.Fatalf("native profile selection incorrect: %#v", dialog.items)
			}
		})
	}
}

func TestInstructionsToolingProfileTogglePersistsProviderSetting(t *testing.T) {
	dialog, ws := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsCrux)
	dialog.cursor = instructionItemIndex(dialog.items, instrToolingProfile, config.ToolingInstructionsNative)
	if _, ok := dialog.toggle().(ActionInstructionsChanged); !ok {
		t.Fatalf("toggle action did not report instruction change")
	}
	key := "providers." + codex.ID + ".tooling_instructions"
	if got := ws.fields[key]; got != config.ToolingInstructionsNative {
		t.Fatalf("persisted profile = %#v, want %q", got, config.ToolingInstructionsNative)
	}
	if dialog.items[dialog.cursor].disabled {
		t.Fatal("native profile was not selected")
	}
}

func TestInstructionsNativeProfileUnavailableForOtherProviders(t *testing.T) {
	dialog, ws := newInstructionsTestDialog(t, "openai", config.ToolingInstructionsCrux)
	nativeIndex := instructionItemIndex(dialog.items, instrToolingProfile, config.ToolingInstructionsNative)
	if nativeIndex < 0 {
		t.Fatal("native profile row missing")
	}
	if !dialog.items[nativeIndex].unavailable {
		t.Fatal("native profile is selectable for unsupported provider")
	}
	if dialog.items[nativeIndex].label != "Provider native instructions" {
		t.Fatalf("native profile label = %q", dialog.items[nativeIndex].label)
	}
	dialog.cursor = nativeIndex
	if action := dialog.toggle(); action != nil {
		t.Fatalf("unsupported native toggle action = %#v", action)
	}
	if len(ws.fields) != 0 {
		t.Fatalf("unsupported native toggle persisted fields: %#v", ws.fields)
	}
}

func TestInstructionsNativeProfilePreview(t *testing.T) {
	dialog, _ := newInstructionsTestDialog(t, codex.ID, config.ToolingInstructionsNative)
	sections := dialog.activeInstructionSections()
	if len(sections) != 1 {
		t.Fatalf("native preview section count = %d, want 1", len(sections))
	}
	if sections[0].Label != codex.Name+" native tooling instructions" {
		t.Fatalf("native preview label = %q", sections[0].Label)
	}
	if sections[0].Content != codex.StandardToolingInstructions() {
		t.Fatal("native preview does not use Codex tooling instructions")
	}
	if sections[0].Toggleable {
		t.Fatal("provider-native preview section is toggleable")
	}
}

func TestActiveInstructionsRespectsModeAndDisabledSections(t *testing.T) {
	sections := prompt.AllSections()
	if len(sections) == 0 {
		t.Fatal("expected native instruction sections")
	}
	projectPath := filepath.Join(t.TempDir(), "instructions.md")
	const projectInstructions = "# Project\n\nUse project rules."
	if err := os.WriteFile(projectPath, []byte(projectInstructions), 0o644); err != nil {
		t.Fatal(err)
	}

	dialog := &Instructions{
		projectInstrPath: projectPath,
		items: []instrItem{
			{kind: instrMode, id: "all", disabled: false},
			{kind: instrMode, id: "project", disabled: true},
			{kind: instrMode, id: "native", disabled: true},
			{kind: instrSection, id: sections[0].ID, disabled: true},
		},
	}

	all := dialog.activeInstructions()
	if !strings.Contains(all, projectInstructions) {
		t.Fatalf("all instructions missing project content: %q", all)
	}
	if strings.Contains(all, sections[0].Content) {
		t.Fatalf("all instructions include disabled section %q", sections[0].ID)
	}

	dialog.items[0].disabled = true
	dialog.items[1].disabled = false
	if got := dialog.activeInstructions(); got != projectInstructions {
		t.Fatalf("project instructions = %q, want %q", got, projectInstructions)
	}

	dialog.items[1].disabled = true
	dialog.items[2].disabled = false
	if got := dialog.activeInstructions(); strings.Contains(got, projectInstructions) {
		t.Fatalf("native instructions include project content: %q", got)
	}
}

func TestPreviewSectionsUseToggleListAndProjectInstructions(t *testing.T) {
	native := prompt.AllSections()
	if len(native) < 2 {
		t.Fatal("expected at least two native instruction sections")
	}
	projectPath := filepath.Join(t.TempDir(), "instructions.md")
	const projectInstructions = "# Markdown-only heading\n\nProject rules"
	if err := os.WriteFile(projectPath, []byte(projectInstructions), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []instrItem{{kind: instrMode, id: "all"}}
	for index, section := range native {
		items = append(items, instrItem{
			kind:     instrSection,
			id:       section.ID,
			label:    "Toggle " + section.ID,
			disabled: index == 1,
		})
	}
	dialog := &Instructions{items: items, projectInstrPath: projectPath}
	sections := dialog.activeInstructionSections()

	if got, want := len(sections), len(native)+1; got != want {
		t.Fatalf("section count = %d, want %d", got, want)
	}
	if sections[0].Label != "Toggle "+native[0].ID {
		t.Fatalf("first section label = %q", sections[0].Label)
	}
	foundDisabled := false
	for _, section := range sections {
		if section.Label == "Toggle "+native[1].ID {
			foundDisabled = section.Disabled
		}
		if section.Label == "Markdown-only heading" {
			t.Fatal("markdown heading was parsed as a navigation section")
		}
	}
	if !foundDisabled {
		t.Fatalf("disabled section %q is not represented as disabled", native[1].ID)
	}
	project := sections[len(sections)-1]
	if project.Label != "Project instructions" {
		t.Fatalf("project section label = %q", project.Label)
	}
	if project.Content != projectInstructions {
		t.Fatalf("project section content = %q", project.Content)
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
