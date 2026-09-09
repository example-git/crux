package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
)

const (
	MaxRuntimeControlRequestBytes  = 64 << 10
	MaxRuntimeControlResponseBytes = 1 << 20
	MaxRuntimeControlValueBytes    = 16 << 10
)

// RuntimeControlRequest addresses a declared control, never a configuration path.
type RuntimeControlRequest struct {
	Scope  *config.Scope               `json:"scope"`
	Target config.RuntimeControlTarget `json:"target"`
}

// SetRuntimeControlRequest distinguishes a present primitive value from removal.
type SetRuntimeControlRequest struct {
	Scope  *config.Scope               `json:"scope"`
	Target config.RuntimeControlTarget `json:"target"`
	Value  json.RawMessage             `json:"value"`
}

func ValidateRuntimeControlRequest(scope *config.Scope, target config.RuntimeControlTarget, value json.RawMessage, mutation, set bool) error {
	if scope == nil || (*scope != config.ScopeGlobal && *scope != config.ScopeWorkspace) {
		return fmt.Errorf("runtime control requires global or workspace scope")
	}
	if err := target.Validate(mutation); err != nil {
		return err
	}
	if set {
		return validateRuntimeControlScalar(value)
	}
	return nil
}

// DecodeRuntimeControlRequest rejects ambiguous JSON before backend dispatch.
func DecodeRuntimeControlRequest(body []byte, mutation, set bool) (SetRuntimeControlRequest, error) {
	var request SetRuntimeControlRequest
	if len(body) > MaxRuntimeControlRequestBytes || validateRuntimeControlJSON(body) != nil {
		return request, fmt.Errorf("invalid runtime control request")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var err error
	if set {
		err = decoder.Decode(&request)
	} else {
		var address RuntimeControlRequest
		err = decoder.Decode(&address)
		request.Scope, request.Target = address.Scope, address.Target
	}
	if err != nil || decoder.Decode(new(any)) != io.EOF || ValidateRuntimeControlRequest(request.Scope, request.Target, request.Value, mutation, set) != nil {
		return request, fmt.Errorf("invalid runtime control request")
	}
	return request, nil
}

func DecodeRuntimeControlState(body []byte) (config.RuntimeControlState, error) {
	var state config.RuntimeControlState
	if len(body) > MaxRuntimeControlResponseBytes || validateRuntimeControlJSON(body) != nil {
		return state, fmt.Errorf("invalid runtime control response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, fmt.Errorf("decode runtime control response: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return state, fmt.Errorf("runtime control response must contain exactly one state")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || !runtimeControlFieldsPresent(fields, "scope", "target", "binding", "scoped_known", "scoped", "effective", "source", "models") {
		return state, fmt.Errorf("runtime control response is missing required fields")
	}
	for _, name := range []string{"scoped", "effective"} {
		var value map[string]json.RawMessage
		if json.Unmarshal(fields[name], &value) != nil || !runtimeControlFieldsPresent(value, "present") {
			return state, fmt.Errorf("runtime control response is missing %s presence", name)
		}
	}
	if err := ValidateRuntimeControlState(state); err != nil {
		return state, err
	}
	return state, nil
}

func runtimeControlFieldsPresent(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

func ValidateRuntimeControlState(state config.RuntimeControlState) error {
	if err := ValidateRuntimeControlRequest(&state.Scope, state.Target, nil, true, false); err != nil {
		return err
	}
	if err := state.Models.Validate(); err != nil {
		return fmt.Errorf("runtime control model state: %w", err)
	}
	if state.Models.Large == nil || state.Models.Small == nil {
		return fmt.Errorf("runtime control acknowledgement requires complete model state")
	}
	selected := state.Models.Large
	if state.Target.Selection.ModelType == config.SelectedModelTypeSmall {
		selected = state.Models.Small
	}
	if selected == nil || selected.Owner != state.Target.Owner || selected.Model.Provider != state.Target.Owner.ProviderID || selected.Model.Model != state.Target.Selection.ModelID {
		return fmt.Errorf("runtime control acknowledgement changed the selected model or owner")
	}
	knownHost := func(option string) bool { return option == "response-verbosity" || option == "analysis-effort" }
	switch state.Binding.Kind {
	case "host-option":
		key := "options.response_verbosity"
		if state.Binding.HostOption == providerregistry.HostAnalysisEffort {
			key = "options.analysis_effort"
		}
		if !knownHost(string(state.Binding.HostOption)) || state.Target.ControlID != key || state.Binding.GlobalOverride != nil || state.Target.Selection.ModelType != config.SelectedModelTypeLarge {
			return fmt.Errorf("invalid runtime control host binding")
		}
	case "model-option", "provider-option":
		if state.Binding.HostOption != "" {
			return fmt.Errorf("invalid runtime control option binding")
		}
	default:
		return fmt.Errorf("invalid runtime control binding")
	}
	if state.Binding.GlobalOverride != nil && !knownHost(string(*state.Binding.GlobalOverride)) {
		return fmt.Errorf("invalid runtime control global binding")
	}
	switch state.Binding.FallbackMode {
	case "", "if-absent", "before-request-transform", "before-request-options":
	default:
		return fmt.Errorf("invalid runtime control fallback binding")
	}
	if state.RuntimeDependent && state.Binding.FallbackMode == "" {
		return fmt.Errorf("runtime-dependent control requires a fallback binding")
	}
	if !state.ScopedKnown && (state.Scoped.Present || len(state.Scoped.Value) != 0) {
		return fmt.Errorf("unknown runtime control scope cannot contain a value")
	}
	for _, value := range []config.RuntimeControlValue{state.Scoped, state.Effective} {
		if value.Present {
			if err := validateRuntimeControlScalar(value.Value); err != nil {
				return err
			}
		} else if len(value.Value) != 0 {
			return fmt.Errorf("absent runtime control value contains data")
		}
	}
	switch state.Source.Kind {
	case "host-option", "model", "provider", "catalog", "manifest", "absent", "run-override":
	default:
		return fmt.Errorf("invalid runtime control source")
	}
	if state.Source.Scope != nil && *state.Source.Scope != config.ScopeGlobal && *state.Source.Scope != config.ScopeWorkspace {
		return fmt.Errorf("invalid runtime control source scope")
	}
	if state.Effective.Present == (state.Source.Kind == "absent") {
		return fmt.Errorf("runtime control source does not match effective presence")
	}
	if state.Effective.Present && state.Source.Key == "" {
		return fmt.Errorf("runtime control source key is missing")
	}
	if override := state.GlobalOverride; override != nil {
		key := "options.response_verbosity"
		if override.Option == "analysis-effort" {
			key = "options.analysis_effort"
		}
		if !knownHost(string(override.Option)) || override.ConfigKey != key || validateRuntimeControlScalar(override.Value) != nil {
			return fmt.Errorf("invalid runtime control global override")
		}
	}
	return nil
}

func validateRuntimeControlScalar(value json.RawMessage) error {
	if len(value) == 0 || len(value) > MaxRuntimeControlValueBytes || !utf8.Valid(value) {
		return fmt.Errorf("runtime control value must be a primitive JSON value of at most 16 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if decoder.Decode(&decoded) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid runtime control value")
	}
	switch decoded.(type) {
	case bool:
		return providerregistry.ValidateRuntimeControlValue(manifest.RuntimeControl{Type: "boolean"}, value)
	case string:
		return providerregistry.ValidateRuntimeControlValue(manifest.RuntimeControl{Type: "string"}, value)
	case json.Number:
		return providerregistry.ValidateRuntimeControlValue(manifest.RuntimeControl{Type: "number"}, value)
	default:
		return fmt.Errorf("runtime control value must be a string, number, or boolean")
	}
}

func ValidateRuntimeControlAcknowledgement(state config.RuntimeControlState, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage, mutation, set bool) error {
	if err := ValidateRuntimeControlState(state); err != nil {
		return err
	}
	expected := target
	if !mutation && expected.DescriptorDigest == "" {
		expected.DescriptorDigest = state.Target.DescriptorDigest
	}
	if state.Scope != scope || state.Target != expected {
		return fmt.Errorf("runtime control acknowledgement changed the requested target")
	}
	if mutation {
		if !state.ScopedKnown || state.Scoped.Present != set {
			return fmt.Errorf("runtime control acknowledgement did not confirm the scoped change")
		}
		if set && (state.RuntimeDependent || !config.RuntimeControlJSONEqual(state.Scoped.Value, value) || !state.Effective.Present || !config.RuntimeControlJSONEqual(state.Effective.Value, value)) {
			return fmt.Errorf("runtime control acknowledgement differs from the requested value")
		}
	}
	return nil
}

// RuntimeControlEffectiveStatesEqual ignores disk-only provenance while keeping
// the complete accepted model state and effective control identity exact.
func RuntimeControlEffectiveStatesEqual(left, right config.RuntimeControlState) bool {
	left.ScopedKnown, right.ScopedKnown = false, false
	left.Scoped, right.Scoped = config.RuntimeControlValue{}, config.RuntimeControlValue{}
	left.Source.Scope, right.Source.Scope = nil, nil
	if left.Source.Kind == "run-override" && right.Source.Kind == "model" {
		left.Source.Kind = "model"
	}
	if right.Source.Kind == "run-override" && left.Source.Kind == "model" {
		right.Source.Kind = "model"
	}
	a, ae := json.Marshal(left)
	b, be := json.Marshal(right)
	return ae == nil && be == nil && config.RuntimeControlJSONEqual(a, b)
}

// Requests and replies share the same bounded JSON structure checks, including
// duplicate keys inside owners, model provider options, and raw values.
func validateRuntimeControlJSON(body []byte) error {
	if !utf8.Valid(body) {
		return fmt.Errorf("invalid runtime control JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return fmt.Errorf("runtime control JSON exceeds nesting limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, nested := token.(json.Delim)
		if !nested {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || len(key) > 1024 || seen[key] {
					return fmt.Errorf("duplicate or invalid runtime control JSON field")
				}
				seen[key] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid runtime control JSON")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("runtime control JSON must contain exactly one value")
	}
	return nil
}
