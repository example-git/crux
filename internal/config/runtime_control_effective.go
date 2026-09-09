package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
)

// RuntimeControlEffectiveState resolves only accepted configuration and its
// exact registration. ScopedKnown is false: no persistence file is consulted.
func (c *Config) RuntimeControlEffectiveState(scope Scope, target RuntimeControlTarget) (RuntimeControlState, error) {
	if err := validateRuntimeControlSelection(c, scope, target); err != nil {
		return RuntimeControlState{}, err
	}
	registration, ok := c.ProviderRegistration(target.Owner.ProviderID)
	if !ok || registration.Owner() != target.Owner || registration.Runtime == nil || registration.Runtime.Available == nil || !registration.Runtime.Available(target.Selection.ModelID) {
		return RuntimeControlState{}, fmt.Errorf("runtime control owner or selected-model capability is unavailable")
	}
	for _, control := range registration.RuntimeControls {
		if control.ID != target.ControlID {
			continue
		}
		binding, err := providerregistry.ResolveRuntimeControlBinding(registration, control)
		if err != nil {
			return RuntimeControlState{}, err
		}
		return resolveRuntimeControlEffective(c, scope, target, control, binding, registration.RuntimeControls)
	}
	return RuntimeControlState{}, fmt.Errorf("runtime control %q is not declared by the selected owner", target.ControlID)
}

// RuntimeControlEffectiveStateForSurface checks a public acknowledgement using
// host-generated presentation metadata, without constructing a local registry.
func RuntimeControlEffectiveStateForSurface(c *Config, scope Scope, target RuntimeControlTarget, surface providerregistry.Surface) (RuntimeControlState, error) {
	if err := validateRuntimeControlSelection(c, scope, target); err != nil {
		return RuntimeControlState{}, err
	}
	if surface.ID != target.Owner.ProviderID || surface.Owner == nil || *surface.Owner != target.Owner || !surface.Available {
		return RuntimeControlState{}, fmt.Errorf("runtime control surface owner is unavailable or changed")
	}
	if target.Owner.HasPreset {
		return RuntimeControlState{}, fmt.Errorf("runtime control effective state is unavailable from redacted preset configuration")
	}
	controls := make([]manifest.RuntimeControl, len(surface.RuntimeControls))
	for i, control := range surface.RuntimeControls {
		controls[i] = control.RuntimeControl
	}
	for _, control := range surface.RuntimeControls {
		if control.ID != target.ControlID {
			continue
		}
		if !slices.Contains(control.AvailableModels, target.Selection.ModelID) || control.Binding == nil || control.DescriptorDigest == "" || control.DescriptorDigest != providerregistry.RuntimeControlDescriptorDigest(control.RuntimeControl, *control.Binding) {
			return RuntimeControlState{}, fmt.Errorf("runtime control surface descriptor is unavailable or changed")
		}
		return resolveRuntimeControlEffective(c, scope, target, control.RuntimeControl, *control.Binding, controls)
	}
	return RuntimeControlState{}, fmt.Errorf("runtime control is absent from the owner surface")
}

func validateRuntimeControlSelection(c *Config, scope Scope, target RuntimeControlTarget) error {
	if scope != ScopeGlobal && scope != ScopeWorkspace {
		return fmt.Errorf("invalid runtime control scope %d", scope)
	}
	if err := target.Validate(false); err != nil {
		return err
	}
	if c == nil || c.Providers == nil {
		return fmt.Errorf("runtime control configuration is unavailable")
	}
	selected, ok := c.Models[target.Selection.ModelType]
	if !ok || selected.Provider != target.Owner.ProviderID || selected.Model != target.Selection.ModelID {
		return fmt.Errorf("runtime control selected model changed")
	}
	owner, ok := c.ProviderOwner(target.Owner.ProviderID)
	provider, configured := c.Providers.Get(target.Owner.ProviderID)
	if !ok || owner != target.Owner || !configured || provider.Disable || c.GetModel(selected.Provider, selected.Model) == nil {
		return fmt.Errorf("runtime control provider owner or model is unavailable or changed")
	}
	return nil
}

type runtimeControlResolvedValue struct {
	value  any
	source RuntimeControlSource
}

// runtimeControlMergedOptions is shared by effective resolution and mutation
// preflight after the exact selected model has been validated.
func runtimeControlMergedOptions(c *Config, target RuntimeControlTarget) (map[string]runtimeControlResolvedValue, map[string]any) {
	selected := c.Models[target.Selection.ModelType]
	provider, _ := c.Providers.Get(target.Owner.ProviderID)
	model := c.GetModel(selected.Provider, selected.Model)
	merged := map[string]runtimeControlResolvedValue{}
	values := map[string]any{}
	for _, layer := range []struct {
		kind   string
		values map[string]any
	}{
		{"catalog", model.Options.ProviderOptions},
		{"provider", provider.ProviderOptions},
		{"model", selected.ProviderOptions},
	} {
		for key, value := range layer.values {
			merged[key] = runtimeControlResolvedValue{value: value, source: RuntimeControlSource{Kind: layer.kind, Key: key}}
			values[key] = value
		}
	}
	return merged, values
}

func resolveRuntimeControlEffective(c *Config, scope Scope, target RuntimeControlTarget, control manifest.RuntimeControl, binding providerregistry.RuntimeControlBinding, controls []manifest.RuntimeControl) (RuntimeControlState, error) {
	digest := providerregistry.RuntimeControlDescriptorDigest(control, binding)
	if target.DescriptorDigest != "" && target.DescriptorDigest != digest {
		return RuntimeControlState{}, fmt.Errorf("runtime control descriptor changed; resolve the control again")
	}
	target.DescriptorDigest = digest
	if binding.Kind == providerregistry.RuntimeControlHostOption && target.Selection.ModelType != SelectedModelTypeLarge {
		return RuntimeControlState{}, fmt.Errorf("host runtime options apply only to the selected large model")
	}
	state := RuntimeControlState{Scope: scope, Target: target, Binding: binding, Source: RuntimeControlSource{Kind: "absent"}, Models: c.AgentModelState()}
	state.Binding.GlobalOverride = clonePointer(binding.GlobalOverride)
	selected := c.Models[target.Selection.ModelType]
	model := c.GetModel(selected.Provider, selected.Model)
	merged, values := runtimeControlMergedOptions(c, target)
	effort, effortSource := runtimeControlReasoningEffort(selected, *model)
	var result runtimeControlResolvedValue
	present := false
	fallbackOptionCollision := false
	if binding.Kind == providerregistry.RuntimeControlHostOption {
		if value, key := runtimeControlHostValue(c, binding.HostOption); value != "" {
			result, present = runtimeControlResolvedValue{value, RuntimeControlSource{Kind: "host-option", Key: key}}, true
		} else if binding.HostOption == providerregistry.HostAnalysisEffort && model.CanReason {
			// The Codex constructor first consumes merged reasoning_effort,
			// then the selected/catalog reasoning choice.
			if configured, exists := merged["reasoning_effort"]; exists {
				if text, ok := configured.value.(string); ok && text != "" {
					result, present = configured, true
				}
			}
			if !present && effort != "" {
				result, present = runtimeControlResolvedValue{effort, effortSource}, true
			}
		}
	} else {
		remaining, byPath, keys := providerregistry.ResolveRuntimeControlOptions(controls, values)
		ancestor := strings.SplitN(strings.TrimPrefix(control.RequestPath, "/"), "/", 2)[0]
		ancestor = strings.ReplaceAll(strings.ReplaceAll(ancestor, "~1", "/"), "~0", "~")
		_, fallbackOptionCollision = remaining[ancestor]
		if key, exists := keys[control.ID]; exists {
			result, present = merged[key], true
			result.value = byPath[control.RequestPath]
		}
		if !present && effort != "" {
			semantic := strings.ToLower(control.ID + " " + control.RequestPath)
			if strings.Contains(semantic, "reasoning") || strings.Contains(semantic, "analysis_effort") {
				result, present = runtimeControlResolvedValue{effort, effortSource}, true
			}
		}
		// Main requests apply globals after the per-call options. Auxiliary
		// small-model calls use the constructor's global only as a fallback:
		// mapped options and inferred reasoning effort replace it on the wire.
		if binding.GlobalOverride != nil && (target.Selection.ModelType == SelectedModelTypeLarge || !present) {
			if value, key := runtimeControlHostValue(c, *binding.GlobalOverride); value != "" {
				raw, _ := json.Marshal(value)
				state.GlobalOverride = &RuntimeControlOverride{Option: *binding.GlobalOverride, ConfigKey: key, Value: raw}
				result, present = runtimeControlResolvedValue{value, RuntimeControlSource{Kind: "host-option", Key: key}}, true
			}
		}
	}
	if !present && control.Default != nil {
		result, present = runtimeControlResolvedValue{control.Default, RuntimeControlSource{Kind: "manifest", Key: control.ID}}, true
	}
	// Constructor fallbacks can yield to an existing native request field or
	// a declared request transform. Keep their configured value visible while
	// distinguishing it from a per-call value applied after those operations.
	fallbackSensitive := binding.FallbackMode == "if-absent" || binding.FallbackMode == "before-request-transform" ||
		(binding.FallbackMode == "before-request-options" && fallbackOptionCollision)
	state.RuntimeDependent = fallbackSensitive && (!present || result.source.Kind == "manifest" ||
		(target.Selection.ModelType == SelectedModelTypeSmall && state.GlobalOverride != nil))
	if present {
		raw, err := json.Marshal(result.value)
		if err != nil {
			return RuntimeControlState{}, fmt.Errorf("runtime control effective value cannot be encoded")
		}
		if err := providerregistry.ValidateRuntimeControlValue(control, raw); err != nil {
			return RuntimeControlState{}, fmt.Errorf("runtime control effective value is invalid: %w", err)
		}
		state.Effective = RuntimeControlValue{Present: true, Value: bytes.Clone(raw)}
		state.Source = result.source
	}
	return state, nil
}

func runtimeControlHostValue(c *Config, option providerregistry.HostRuntimeControl) (string, string) {
	if c.Options == nil {
		return "", runtimeControlHostKey(option)
	}
	switch option {
	case providerregistry.HostResponseVerbosity:
		return c.Options.ResponseVerbosity, runtimeControlHostKey(option)
	case providerregistry.HostAnalysisEffort:
		return c.Options.AnalysisEffort, runtimeControlHostKey(option)
	default:
		return "", ""
	}
}

func runtimeControlHostKey(option providerregistry.HostRuntimeControl) string {
	switch option {
	case providerregistry.HostResponseVerbosity:
		return "options.response_verbosity"
	case providerregistry.HostAnalysisEffort:
		return "options.analysis_effort"
	default:
		return ""
	}
}

func runtimeControlReasoningEffort(selected SelectedModel, model catalog.Model) (string, RuntimeControlSource) {
	if model.CanReason {
		if selected.ReasoningEffort != "" && slices.Contains(model.ReasoningLevels, selected.ReasoningEffort) {
			return selected.ReasoningEffort, RuntimeControlSource{Kind: "model", Key: "reasoning_effort"}
		}
		if model.DefaultReasoningEffort != "" && slices.Contains(model.ReasoningLevels, model.DefaultReasoningEffort) {
			return model.DefaultReasoningEffort, RuntimeControlSource{Kind: "catalog", Key: "default_reasoning_effort"}
		}
		if len(model.ReasoningLevels) > 0 {
			return model.ReasoningLevels[0], RuntimeControlSource{Kind: "catalog", Key: "reasoning_levels"}
		}
	}
	return "", RuntimeControlSource{Kind: "absent"}
}
