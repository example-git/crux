package dialog

import (
	"encoding/json"
	"errors"
	"image"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func newDeclaredControlDialog(t *testing.T, kind string, effective json.RawMessage) (*Instructions, *instructionsTestWorkspace) {
	t.Helper()
	d, ws := newInstructionsTestDialog(t, "synthetic", config.ToolingInstructionsCrux)
	owner := providerregistry.RegistrationOwner{
		ProviderID: "synthetic", Construction: providerregistry.ConstructionGenericJSON,
		HasManifest: true, ManifestID: "plugin.synthetic", ManifestVersion: "1.0.0",
	}
	control := manifest.RuntimeControl{ID: "vendor.mode", Label: "Vendor mode", Type: kind, Scope: "model", RequestPath: "/vendor/mode"}
	binding := providerregistry.RuntimeControlBinding{Kind: providerregistry.RuntimeControlModelOption}
	digest := providerregistry.RuntimeControlDescriptorDigest(control, binding)
	ws.surfaces = []providerregistry.Surface{{
		ID: owner.ProviderID, Owner: &owner, Available: true,
		RuntimeControls: []providerregistry.RuntimeControlSurface{{RuntimeControl: control, Available: true, AvailableModels: []string{"test-model"}, Binding: &binding, DescriptorDigest: digest}},
	}}
	require.NoError(t, ws.cfg.BindProviderSurfaceOwners(ws.surfaces))
	target := config.RuntimeControlTarget{
		Owner: owner, ControlID: control.ID, DescriptorDigest: digest,
		Selection: config.RuntimeControlSelection{ModelType: config.SelectedModelTypeLarge, ModelID: "test-model"},
	}
	ws.controlStates = map[string]config.RuntimeControlState{control.ID: {
		Scope: config.ScopeGlobal, Target: target, Binding: binding, ScopedKnown: true,
		Effective: config.RuntimeControlValue{Present: true, Value: effective},
		Source:    config.RuntimeControlSource{Kind: "provider", Key: control.ID}, Models: ws.cfg.AgentModelState(),
	}}
	d = NewInstructions(d.com)
	d.cursor = instructionItemIndex(d.items, instrMetadataValue, control.ID)
	return d, ws
}

func TestInstructionsRuntimeControlsResolveWithoutOptimisticModelMutation(t *testing.T) {
	d, ws := newDeclaredControlDialog(t, "boolean", json.RawMessage(`true`))
	require.Zero(t, ws.controlResolveCalls, "constructing a dialog must not perform I/O")
	require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), "Unresolved")
	action := d.toggle().(ActionCmd)
	require.IsType(t, util.InfoMsg{}, action.Cmd())
	require.False(t, d.editingMetadata)
	command := d.LoadRuntimeControls()
	require.Zero(t, ws.controlResolveCalls)
	require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), "Loading")
	message := command().(ActionInstructionControlsLoaded)
	require.Equal(t, 1, ws.controlResolveCalls)
	require.Nil(t, d.items[d.cursor].controlState, "command must not mutate dialog")
	require.NoError(t, d.CompleteRuntimeControls(message, true))
	require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), "true · provider · inherited")
	require.Nil(t, d.toggle())
	require.Empty(t, d.metadataInput.Value(), "known absent saved value must not become an explicit inherited value")
	require.Contains(t, d.metadataInput.Placeholder, "Inherited: true")
}

func TestInstructionsRuntimeControlsUnknownScopeUsesEffectiveEditorValue(t *testing.T) {
	d, ws := newDeclaredControlDialog(t, "string", json.RawMessage(`"accepted"`))
	state := ws.controlStates["vendor.mode"]
	state.ScopedKnown = false
	ws.controlStates["vendor.mode"] = state
	loadInstructionsControls(t, d)
	require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), "saved scope unknown")
	require.NotContains(t, runtimeControlRowValue(d.items[d.cursor]), "inherited")
	require.Nil(t, d.toggle())
	require.Equal(t, "accepted", d.metadataInput.Value())
	require.Contains(t, d.metadataInput.Placeholder, "scope unknown")
}

func TestInstructionsRuntimeDependentFallbackIsQualifiedUntilExplicitSet(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known scope", false: "unknown scope"}[known], func(t *testing.T) {
			d, ws := newDeclaredControlDialog(t, "string", json.RawMessage(`"low"`))
			control := &ws.surfaces[0].RuntimeControls[0]
			control.Default = "low"
			control.Binding.FallbackMode = "before-request-transform"
			control.DescriptorDigest = providerregistry.RuntimeControlDescriptorDigest(control.RuntimeControl, *control.Binding)
			fallback := ws.controlStates["vendor.mode"]
			fallback.Target.DescriptorDigest = control.DescriptorDigest
			fallback.Binding = *control.Binding
			fallback.ScopedKnown = known
			fallback.RuntimeDependent = true
			fallback.Source.Kind = "manifest"
			ws.controlStates["vendor.mode"] = fallback
			d = NewInstructions(d.com)
			d.cursor = instructionItemIndex(d.items, instrMetadataValue, "vendor.mode")
			loadInstructionsControls(t, d)
			draw := func() string {
				screen := uv.NewScreenBuffer(110, 55)
				d.Draw(screen, image.Rect(0, 0, 110, 55))
				return ansi.Strip(screen.String())
			}
			require.Contains(t, draw(), "At request time · configured fallback: low · manifest")
			require.Nil(t, d.toggle())
			require.Contains(t, d.metadataInput.Placeholder, "Configured fallback")
			require.NotContains(t, d.metadataInput.Placeholder, "Effective")
			if known {
				require.Empty(t, d.metadataInput.Value())
			} else {
				require.Equal(t, "low", d.metadataInput.Value())
				require.Contains(t, d.metadataInput.Placeholder, "scope unknown")
			}
			d.metadataInput.SetValue("high")
			command := d.saveMetadataValue().(ActionCmd).Cmd
			message := command().(ActionInstructionMutationCompleted)
			require.Contains(t, draw(), "At request time", "command cannot remove fallback qualification before acknowledgement")
			require.NoError(t, d.CompleteOperation(message))
			require.Contains(t, draw(), "high · model")
			require.NotContains(t, draw(), "At request time")
			fallback.ScopedKnown = true
			ws.controlAcknowledgement = &fallback
			completeInstructionAction(t, d, d.removeMetadataValue())
			require.Contains(t, draw(), "At request time · configured fallback: low · manifest")
		})
	}
}

func TestInstructionsRuntimeDependentWithoutConfiguredFallbackRow(t *testing.T) {
	d, ws := newDeclaredControlDialog(t, "string", nil)
	state := ws.controlStates["vendor.mode"]
	state.RuntimeDependent = true
	state.Effective = config.RuntimeControlValue{}
	state.Source = config.RuntimeControlSource{Kind: "absent"}
	d.items[d.cursor].controlState = &state
	screen := uv.NewScreenBuffer(110, 55)
	d.Draw(screen, image.Rect(0, 0, 110, 55))
	output := ansi.Strip(screen.String())
	require.Contains(t, output, "At request time · no configured fallback · absent")
	require.NotContains(t, output, "configured fallback: unset")
}

func TestInstructionsRuntimeControlsPreserveFalseZeroEmptyAndLiteralID(t *testing.T) {
	for _, tc := range []struct{ name, kind, input, expected string }{
		{"false", "boolean", "false", `false`},
		{"zero", "integer", "0", `0`},
		{"precise-number", "number", "9007199254740993", `9007199254740993`},
		{"empty", "string", "", `""`},
		{"spaces", "string", "  preserve  ", `"  preserve  "`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ws := newDeclaredControlDialog(t, tc.kind, json.RawMessage(tc.expected))
			loadInstructionsControls(t, d)
			before := runtimeControlRowValue(d.items[d.cursor])
			require.Nil(t, d.toggle())
			d.metadataInput.SetValue(tc.input)
			command := d.saveMetadataValue().(ActionCmd).Cmd
			require.Zero(t, ws.controlMutationCalls)
			require.Equal(t, before, runtimeControlRowValue(d.items[d.cursor]))
			message := command().(ActionInstructionMutationCompleted)
			require.NoError(t, message.Err)
			require.Equal(t, before, runtimeControlRowValue(d.items[d.cursor]), "command cannot edit displayed state")
			require.Equal(t, "vendor.mode", ws.controlTarget.ControlID)
			require.Equal(t, tc.expected, string(ws.controlValue))
			require.False(t, ws.controlRemove)
			require.NoError(t, d.CompleteOperation(message))
			require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), " · model")
			require.NotContains(t, runtimeControlRowValue(d.items[d.cursor]), "inherited")
			if tc.name == "empty" {
				require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), `""`)
			}
		})
	}
}

func TestInstructionsRuntimeControlResetDisplaysAcknowledgedInheritance(t *testing.T) {
	d, ws := newDeclaredControlDialog(t, "string", json.RawMessage(`"saved"`))
	state := ws.controlStates["vendor.mode"]
	state.Scoped = state.Effective
	state.Source.Kind = "model"
	ws.controlStates["vendor.mode"] = state
	loadInstructionsControls(t, d)
	state.Scoped = config.RuntimeControlValue{}
	state.Effective = config.RuntimeControlValue{Present: true, Value: json.RawMessage(`"low"`)}
	state.Source = config.RuntimeControlSource{Kind: "host-option", Key: "options.analysis_effort"}
	state.GlobalOverride = &config.RuntimeControlOverride{Option: providerregistry.HostAnalysisEffort, ConfigKey: "options.analysis_effort", Value: json.RawMessage(`"low"`)}
	ws.controlAcknowledgement = &state
	command := d.HandleMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}).(ActionCmd).Cmd
	require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), "saved")
	message := command().(ActionInstructionMutationCompleted)
	require.True(t, ws.controlRemove)
	require.NoError(t, d.CompleteOperation(message))
	require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), "low · global override options.analysis_effort · inherited")
	refresh := d.RefreshRuntimeControlsAfter(message)
	require.NotNil(t, refresh, "related global/scoped rows must refresh after acknowledgement")
	// An older read can never overwrite the just-accepted reset.
	old := ActionInstructionControlsLoaded{Dialog: d, generation: d.controlsGeneration - 1}
	require.NoError(t, d.CompleteRuntimeControls(old, true))
	require.Contains(t, runtimeControlRowValue(d.items[d.cursor]), "low · global override")
}

func TestInstructionsRuntimeControlFailureAndDescriptorFences(t *testing.T) {
	for _, mode := range []string{"read-error", "closed-read-error", "read-definition", "write-error", "write-definition-before", "write-definition-after", "wrong-ack", "wrong-value-ack"} {
		t.Run(mode, func(t *testing.T) {
			d, ws := newDeclaredControlDialog(t, "string", json.RawMessage(`"old"`))
			if mode == "read-error" || mode == "closed-read-error" {
				ws.controlResolveErr = errors.New("saved value pending runtime acknowledgement")
				message := d.LoadRuntimeControls()().(ActionInstructionControlsLoaded)
				require.ErrorContains(t, d.CompleteRuntimeControls(message, mode != "closed-read-error"), "pending runtime acknowledgement")
				require.Nil(t, d.items[d.cursor].controlState)
				return
			}
			if mode == "read-definition" {
				message := d.LoadRuntimeControls()().(ActionInstructionControlsLoaded)
				ws.surfaces[0].RuntimeControls[0].DescriptorDigest = "replacement"
				require.ErrorContains(t, d.CompleteRuntimeControls(message, true), "definition changed")
				require.Nil(t, d.items[d.cursor].controlState)
				return
			}
			loadInstructionsControls(t, d)
			require.Nil(t, d.toggle())
			d.metadataInput.SetValue("new")
			if mode == "write-error" {
				ws.mutationErr = errors.New(`vendor.mode is overridden by options.analysis_effort="low"; change or clear that setting first`)
			}
			command := d.saveMetadataValue().(ActionCmd).Cmd
			if mode == "write-definition-before" {
				ws.surfaces[0].RuntimeControls[0].DescriptorDigest = "replacement"
			}
			message := command().(ActionInstructionMutationCompleted)
			switch mode {
			case "write-definition-after":
				ws.surfaces[0].RuntimeControls[0].DescriptorDigest = "replacement"
			case "wrong-ack":
				message.ControlState.Target.DescriptorDigest = "replacement"
			case "wrong-value-ack":
				message.ControlState.Effective.Value = json.RawMessage(`"different"`)
			}
			require.Error(t, d.CompleteOperation(message))
			require.Equal(t, "old", d.items[d.cursor].value)
			require.True(t, d.editingMetadata)
			require.False(t, d.operationPending)
			if mode == "write-definition-before" {
				require.Zero(t, ws.controlMutationCalls)
			}
		})
	}
}
