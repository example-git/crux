package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"

	"github.com/example-git/crux/internal/providerregistry"
)

type RuntimeControlSelection struct {
	ModelType SelectedModelType `json:"model_type"`
	ModelID   string            `json:"model_id"`
}

type RuntimeControlTarget struct {
	Owner            providerregistry.RegistrationOwner `json:"owner"`
	ControlID        string                             `json:"control_id"`
	DescriptorDigest string                             `json:"descriptor_digest,omitempty"`
	Selection        RuntimeControlSelection            `json:"selection"`
}

type RuntimeControlValue struct {
	Present bool            `json:"present"`
	Value   json.RawMessage `json:"value,omitempty"`
}

type RuntimeControlSource struct {
	Kind  string `json:"kind"`
	Scope *Scope `json:"scope,omitempty"`
	Key   string `json:"key,omitempty"`
}

type RuntimeControlOverride struct {
	Option    providerregistry.HostRuntimeControl `json:"option"`
	Value     json.RawMessage                     `json:"value"`
	ConfigKey string                              `json:"config_key"`
}

type RuntimeControlState struct {
	Scope            Scope                                  `json:"scope"`
	Target           RuntimeControlTarget                   `json:"target"`
	Binding          providerregistry.RuntimeControlBinding `json:"binding"`
	ScopedKnown      bool                                   `json:"scoped_known"`
	Scoped           RuntimeControlValue                    `json:"scoped"`
	Effective        RuntimeControlValue                    `json:"effective"`
	RuntimeDependent bool                                   `json:"runtime_dependent,omitempty"`
	Source           RuntimeControlSource                   `json:"source"`
	GlobalOverride   *RuntimeControlOverride                `json:"global_override,omitempty"`
	Models           AgentModelState                        `json:"models"`
}

func (t RuntimeControlTarget) Validate(requireDigest bool) error {
	if t.Owner.ProviderID == "" || t.ControlID == "" || t.Selection.ModelID == "" {
		return fmt.Errorf("runtime control requires its owner, control ID and selected model")
	}
	if t.Selection.ModelType != SelectedModelTypeLarge && t.Selection.ModelType != SelectedModelTypeSmall {
		return fmt.Errorf("runtime control model slot must be large or small")
	}
	if requireDigest && t.DescriptorDigest == "" {
		return fmt.Errorf("runtime control descriptor is required; resolve the control again")
	}
	return nil
}

// RuntimeControlJSONEqual compares JSON without rounding numbers through
// float64. Validation of a control's allowed primitive type is separate.
func RuntimeControlJSONEqual(left, right json.RawMessage) bool {
	decode := func(raw json.RawMessage) (any, bool) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
			return nil, false
		}
		return value, true
	}
	a, aok := decode(left)
	b, bok := decode(right)
	if !aok || !bok {
		return false
	}
	return runtimeControlJSONValuesEqual(a, b)
}

func runtimeControlJSONValuesEqual(a, b any) bool {
	switch value := a.(type) {
	case json.Number:
		other, ok := b.(json.Number)
		if !ok {
			return false
		}
		x, xe := runtimeControlNumber(string(value))
		y, ye := runtimeControlNumber(string(other))
		return x == y && xe.Cmp(ye) == 0
	case map[string]any:
		other, ok := b.(map[string]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for key, item := range value {
			candidate, exists := other[key]
			if !exists || !runtimeControlJSONValuesEqual(item, candidate) {
				return false
			}
		}
		return true
	case []any:
		other, ok := b.([]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for index, item := range value {
			if !runtimeControlJSONValuesEqual(item, other[index]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// Normalize a valid JSON number without expanding its exponent. Even a very
// large exponent therefore requires space proportional only to the input.
func runtimeControlNumber(number string) (string, *big.Int) {
	exponent := new(big.Int)
	if index := strings.IndexAny(number, "eE"); index >= 0 {
		exponent.SetString(number[index+1:], 10)
		number = number[:index]
	}
	negative := strings.HasPrefix(number, "-")
	number = strings.TrimPrefix(number, "-")
	if index := strings.IndexByte(number, '.'); index >= 0 {
		exponent.Sub(exponent, big.NewInt(int64(len(number)-index-1)))
		number = number[:index] + number[index+1:]
	}
	number = strings.TrimLeft(number, "0")
	if number == "" {
		return "0", new(big.Int)
	}
	trimmed := strings.TrimRight(number, "0")
	exponent.Add(exponent, big.NewInt(int64(len(number)-len(trimmed))))
	if negative {
		trimmed = "-" + trimmed
	}
	return trimmed, exponent
}
