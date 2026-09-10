package dialog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type instructionControlResult struct {
	target config.RuntimeControlTarget
	state  config.RuntimeControlState
	err    error
}

// ActionInstructionControlsLoaded carries immutable results back to the main UI.
type ActionInstructionControlsLoaded struct {
	Dialog     *Instructions
	generation uint64
	operation  InstructionOperation
	results    []instructionControlResult
}

// LoadRuntimeControls starts a read without doing I/O on the UI thread. Every
// row, including host controls, comes from the workspace's accepted state.
func (d *Instructions) LoadRuntimeControls() tea.Cmd {
	var targets []config.RuntimeControlTarget
	for index := range d.items {
		item := &d.items[index]
		if item.kind != instrMetadataValue || item.control == nil {
			continue
		}
		item.controlLoading = true
		targets = append(targets, config.RuntimeControlTarget{
			Owner: d.providerOwner, ControlID: item.id,
			DescriptorDigest: item.control.DescriptorDigest,
			Selection:        config.RuntimeControlSelection{ModelType: config.SelectedModelTypeLarge, ModelID: d.providerModel},
		})
	}
	if len(targets) == 0 {
		return nil
	}
	d.controlsGeneration++
	message := ActionInstructionControlsLoaded{Dialog: d, generation: d.controlsGeneration,
		operation: InstructionOperation{providerID: d.providerID, modelID: d.providerModel,
			owner: d.providerOwner, ownerSet: d.providerOwnerSet}}
	ws := d.com.Workspace
	return func() tea.Msg {
		for _, target := range targets {
			result := instructionControlResult{target: target}
			result.err = message.operation.validateSelection(ws)
			if result.err == nil {
				result.state, result.err = ws.RuntimeControlState(context.Background(), config.ScopeGlobal, target)
			}
			if result.err == nil {
				result.err = validateRuntimeControlAcknowledgement(target, &result.state)
			}
			message.results = append(message.results, result)
		}
		return message
	}
}

// CompleteRuntimeControls applies read results only to the still-open dialog.
// Closed-dialog failures remain visible, but a superseded read cannot overwrite
// a later mutation or refresh.
func (d *Instructions) CompleteRuntimeControls(msg ActionInstructionControlsLoaded, display bool) error {
	if msg.Dialog != d || msg.generation != d.controlsGeneration {
		return nil
	}
	selectionErr := msg.operation.validateSelection(d.com.Workspace)
	var failures []error
	for _, result := range msg.results {
		err := result.err
		if err == nil {
			err = selectionErr
		}
		if err == nil {
			err = validateRuntimeControlTarget(d.com.Workspace, result.state.Target)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", result.target.ControlID, err))
		}
		if !display {
			continue
		}
		for index := range d.items {
			item := &d.items[index]
			if item.kind != instrMetadataValue || item.id != result.target.ControlID {
				continue
			}
			item.controlLoading = false
			if err != nil {
				item.controlError = err.Error()
				continue
			}
			state := result.state
			item.controlState = &state
			item.controlError = ""
			item.value = runtimeControlDisplayValue(state.Effective)
		}
	}
	return errors.Join(failures...)
}

// RefreshRuntimeControlsAfter refreshes sibling effective values when a global
// override changed. The successful row keeps its acknowledged value while read
// work is pending.
func (d *Instructions) RefreshRuntimeControlsAfter(msg ActionInstructionMutationCompleted) tea.Cmd {
	if msg.Operation.mutation.kind != instrMetadataValue {
		return nil
	}
	return d.LoadRuntimeControls()
}

func validateRuntimeControlTarget(ws workspace.Workspace, target config.RuntimeControlTarget) error {
	if err := target.Validate(true); err != nil {
		return err
	}
	cfg := ws.Config()
	if cfg == nil {
		return fmt.Errorf("configuration not found")
	}
	selected := cfg.Models[target.Selection.ModelType]
	surface, ok := providerregistry.LookupSurface(ws.ProviderSurfaces(), selected.Provider)
	if !ok || surface.Owner == nil || *surface.Owner != target.Owner ||
		selected.Provider != target.Owner.ProviderID || selected.Model != target.Selection.ModelID {
		return fmt.Errorf("runtime control provider selection changed; reopen the instructions dialog")
	}
	for _, control := range surface.RuntimeControls {
		if control.ID != target.ControlID {
			continue
		}
		if !control.Available || control.Binding == nil {
			return fmt.Errorf("runtime control %q is unavailable: %s", control.ID, control.Diagnostic)
		}
		if control.DescriptorDigest != target.DescriptorDigest {
			return fmt.Errorf("runtime control %q definition changed; reopen the instructions dialog", control.ID)
		}
		return nil
	}
	return fmt.Errorf("runtime control %q is no longer declared; reopen the instructions dialog", target.ControlID)
}

func validateRuntimeControlAcknowledgement(target config.RuntimeControlTarget, state *config.RuntimeControlState) error {
	if state == nil {
		return fmt.Errorf("runtime control acknowledgement is missing")
	}
	return proto.ValidateRuntimeControlAcknowledgement(*state, config.ScopeGlobal, target, nil, false, false)
}

func (d *Instructions) checkControlItem(item *instrItem) error {
	if d.operationPending {
		return fmt.Errorf("an instruction change is still pending")
	}
	if item.controlLoading {
		return fmt.Errorf("runtime control %q is still loading", item.id)
	}
	if item.controlError != "" {
		return fmt.Errorf("runtime control %q: %s", item.id, item.controlError)
	}
	if item.controlState == nil || item.control == nil {
		return fmt.Errorf("runtime control %q has not been resolved; reopen the instructions dialog", item.id)
	}
	return validateRuntimeControlTarget(d.com.Workspace, item.controlState.Target)
}

func (d *Instructions) saveMetadataValue() Action {
	item := &d.items[d.cursor]
	if err := d.checkControlItem(item); err != nil {
		return ActionCmd{Cmd: util.ReportError(err)}
	}
	value := d.metadataInput.Value()
	// Preserve the established blank-to-clear behavior of the known host enums.
	// Declared string controls use an empty string as a value, and explicit Reset
	// is available for every type.
	if item.controlState.Binding.Kind == providerregistry.RuntimeControlHostOption && strings.TrimSpace(value) == "" {
		return d.removeMetadataValue()
	}
	parsed, ok := parseRuntimeControlValue(item.control, value)
	if !ok {
		return ActionCmd{Cmd: util.ReportError(fmt.Errorf("invalid %s value %q", item.label, value))}
	}
	return d.mutate(instructionMutation{kind: instrMetadataValue, id: item.id,
		controlTarget: item.controlState.Target, value: parsed})
}

func (d *Instructions) removeMetadataValue() Action {
	item := &d.items[d.cursor]
	if err := d.checkControlItem(item); err != nil {
		return ActionCmd{Cmd: util.ReportError(err)}
	}
	return d.mutate(instructionMutation{kind: instrMetadataValue, id: item.id,
		controlTarget: item.controlState.Target, remove: true})
}

func runtimeControlInputValue(value config.RuntimeControlValue) string {
	if !value.Present {
		return ""
	}
	var text string
	if json.Unmarshal(value.Value, &text) == nil {
		return text
	}
	return string(value.Value)
}

func runtimeControlDisplayValue(value config.RuntimeControlValue) string {
	if !value.Present {
		return "unset"
	}
	var text string
	if json.Unmarshal(value.Value, &text) == nil {
		if text == "" || strings.TrimSpace(text) != text || strings.ContainsAny(text, "\n\r\t") {
			return strconv.Quote(text)
		}
		return text
	}
	return string(value.Value)
}

func runtimeControlRowValue(item instrItem) string {
	if item.controlError != "" {
		return "Unavailable: " + item.controlError
	}
	if item.controlState == nil {
		if item.controlLoading {
			return "Loading…"
		}
		return "Unresolved"
	}
	state := item.controlState
	source := state.Source.Kind
	if state.GlobalOverride != nil {
		source = "global override " + state.GlobalOverride.ConfigKey
	} else if source == "host-option" {
		source = "global setting"
	}
	if state.Source.Scope != nil {
		if *state.Source.Scope == config.ScopeWorkspace {
			source += " (workspace)"
		} else {
			source += " (global)"
		}
	}
	value := runtimeControlDisplayValue(state.Effective) + " · " + source
	if state.RuntimeDependent {
		if state.Effective.Present {
			value = "At request time · configured fallback: " + value
		} else {
			value = "At request time · no configured fallback · " + source
		}
	}
	if state.ScopedKnown && !state.Scoped.Present {
		value += " · inherited"
	} else if !state.ScopedKnown {
		value += " · saved scope unknown"
	}
	if item.controlLoading {
		value += " · refreshing"
	}
	return value
}

func parseRuntimeControlValue(control *providerregistry.RuntimeControlSurface, value string) (json.RawMessage, bool) {
	if control == nil || !control.Available {
		return nil, false
	}
	var parsed any
	switch control.Type {
	case "enum":
		value = strings.TrimSpace(value)
		if !slices.Contains(control.Values, value) {
			return nil, false
		}
		parsed = value
	case "string":
		parsed = value
	case "boolean":
		var err error
		parsed, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return nil, false
		}
	case "integer", "number":
		value = strings.TrimSpace(value)
		if !json.Valid([]byte(value)) || value == "" || !strings.ContainsAny(value[:1], "-0123456789") {
			return nil, false
		}
		if control.Type == "integer" && strings.ContainsAny(value, ".eE") {
			return nil, false
		}
		return json.RawMessage(value), true
	default:
		return nil, false
	}
	raw, err := json.Marshal(parsed)
	return raw, err == nil
}
