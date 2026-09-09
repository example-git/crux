package providerregistry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

type RuntimeControlBindingKind string

const (
	RuntimeControlHostOption     RuntimeControlBindingKind = "host-option"
	RuntimeControlModelOption    RuntimeControlBindingKind = "model-option"
	RuntimeControlProviderOption RuntimeControlBindingKind = "provider-option"
	MaxRuntimeControlValueBytes                            = 16 << 10
)

type HostRuntimeControl string

const (
	HostResponseVerbosity HostRuntimeControl = "response-verbosity"
	HostAnalysisEffort    HostRuntimeControl = "analysis-effort"
)

// RuntimeControlBinding is derived by the host from a validated registration.
// A control ID is never interpreted as an arbitrary persisted config path.
type RuntimeControlBinding struct {
	Kind           RuntimeControlBindingKind `json:"kind"`
	HostOption     HostRuntimeControl        `json:"host_option,omitempty"`
	GlobalOverride *HostRuntimeControl       `json:"global_override,omitempty"`
	FallbackMode   string                    `json:"fallback_mode,omitempty"`
}

func ResolveRuntimeControlBinding(reg Registration, control manifest.RuntimeControl) (RuntimeControlBinding, error) {
	var result RuntimeControlBinding
	matched := false
	for _, declared := range reg.RuntimeControls {
		if declared.ID == control.ID {
			if matched || !reflect.DeepEqual(declared, control) {
				return result, errors.New("runtime control declaration changed or is duplicated")
			}
			matched = true
			continue
		}
		if declared.RequestPath != "" && (declared.RequestPath == control.RequestPath ||
			strings.HasPrefix(declared.RequestPath, control.RequestPath+"/") ||
			strings.HasPrefix(control.RequestPath, declared.RequestPath+"/")) {
			return result, fmt.Errorf("runtime controls %q and %q have overlapping request paths", control.ID, declared.ID)
		}
	}
	if !matched || control.ID == "" || control.RequestPath == "" || !manifest.ValidJSONPointer(control.RequestPath) {
		return result, errors.New("runtime control has no exact executable declaration")
	}
	if reg.Construction == ConstructionCodex {
		if reg.Manifest != nil {
			delegation := reg.Manifest.Capabilities.Compatibility
			if reg.CompatibilityAdapter != ConstructionCodex || delegation == nil || !slices.Contains(delegation.Delegates, "runtime") {
				return result, errors.New("runtime control requires the declared host runtime delegation")
			}
		}
		for _, expected := range codexRuntimeControls() {
			if reflect.DeepEqual(expected, control) {
				result.Kind = RuntimeControlHostOption
				if control.ID == "options.analysis_effort" {
					result.HostOption = HostAnalysisEffort
				} else {
					result.HostOption = HostResponseVerbosity
				}
				return result, nil
			}
		}
		return result, errors.New("runtime control does not match the host adapter declaration")
	}
	if strings.HasPrefix(control.ID, "options.") {
		return result, errors.New("runtime control uses a reserved host option ID without its host binding")
	}
	switch reg.Construction {
	case ConstructionOpenAIResponses, ConstructionGenericJSON, ConstructionGeminiContent, ConstructionGeminiInteraction:
	default:
		return result, errors.New("runtime control storage is unavailable for this construction")
	}
	switch control.Scope {
	case "model":
		result.Kind = RuntimeControlModelOption
	case "provider":
		if reg.Construction != ConstructionOpenAIResponses {
			return result, errors.New("provider-scoped runtime controls are unavailable for this construction")
		}
		result.Kind = RuntimeControlProviderOption
	default:
		return result, errors.New("runtime control has no persistent model or provider scope")
	}
	var override HostRuntimeControl
	switch {
	case IsResponseVerbosityControl(control):
		override = HostResponseVerbosity
	case IsAnalysisEffortControl(control):
		override = HostAnalysisEffort
	}
	if override != "" {
		result.GlobalOverride = &override
	}
	if reg.Construction == ConstructionOpenAIResponses {
		// Native request defaults fill absent fields before request transforms.
		// The actual SDK body can already contain the path.
		result.FallbackMode = "if-absent"
	} else {
		result.FallbackMode = "before-request-options"
		if reg.Operation != nil && reg.Operation.RequestTransform != nil {
			for _, operation := range reg.Operation.RequestTransform.Operations {
				if runtimeControlPathsOverlap(control.RequestPath, operation.Path) || (operation.Operation == "move" || operation.Operation == "rename-key") && runtimeControlPathsOverlap(control.RequestPath, operation.From) {
					result.FallbackMode = "before-request-transform"
					break
				}
			}
		}
	}
	return result, nil
}

func runtimeControlPathsOverlap(left, right string) bool {
	return left == right || left == "" || right == "" || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func RuntimeControlDescriptorDigest(control manifest.RuntimeControl, binding RuntimeControlBinding) string {
	data, err := json.Marshal(struct {
		Control manifest.RuntimeControl `json:"control"`
		Binding RuntimeControlBinding   `json:"binding"`
	}{control, binding})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RuntimeControlOptionKeys preserves the established canonical-ID then suffix
// alias order. Callers must process controls in declaration order: aliases are
// consumed only once by ResolveRuntimeControlOptions.
func RuntimeControlOptionKeys(control manifest.RuntimeControl) []string {
	keys := []string{control.ID}
	if index := strings.LastIndex(control.ID, "."); index >= 0 {
		keys = append(keys, control.ID[index+1:])
	}
	return keys
}

func ResolveRuntimeControlOptions(controls []manifest.RuntimeControl, merged map[string]any) (remaining, byPath map[string]any, keysByID map[string]string) {
	remaining = maps.Clone(merged)
	if remaining == nil {
		remaining = map[string]any{}
	}
	byPath, keysByID = map[string]any{}, map[string]string{}
	for _, control := range controls {
		for _, key := range RuntimeControlOptionKeys(control) {
			if value, ok := remaining[key]; ok {
				byPath[control.RequestPath] = value
				keysByID[control.ID] = key
				delete(remaining, key)
				break
			}
		}
	}
	return remaining, byPath, keysByID
}

func ValidateRuntimeControlValue(control manifest.RuntimeControl, raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > MaxRuntimeControlValueBytes || !utf8.Valid(raw) {
		return errors.New("runtime control value must be valid UTF-8 JSON of at most 16 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("runtime control value must contain exactly one JSON value")
	}
	valid := false
	switch control.Type {
	case "enum", "string":
		text, ok := value.(string)
		valid = ok && validControlStringEscapes(bytes.TrimSpace(raw)) && (control.Type == "string" || slices.Contains(control.Values, text))
	case "boolean":
		_, valid = value.(bool)
	case "integer", "number":
		number, ok := value.(json.Number)
		if ok {
			if index := strings.LastIndexAny(string(number), "eE"); index >= 0 {
				exponent, err := strconv.Atoi(string(number)[index+1:])
				if err != nil || exponent < -1000 || exponent > 1000 {
					return errors.New("runtime control number exponent exceeds supported limits")
				}
			}
			floating, err := number.Float64()
			exact, parsed := new(big.Rat).SetString(string(number))
			valid = err == nil && parsed && !math.IsNaN(floating) && !math.IsInf(floating, 0) &&
				(floating != 0 || exact.Sign() == 0) && (control.Type != "integer" || exact.IsInt())
		}
	}
	if !valid {
		return fmt.Errorf("runtime control %q requires a valid %s value", control.ID, control.Type)
	}
	return nil
}

// JSON decoding replaces unpaired surrogate escapes; reject them instead of
// persisting a value different from the user's explicit string.
func validControlStringEscapes(raw []byte) bool {
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		value, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, _ := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
