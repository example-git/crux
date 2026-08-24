// Derived from Catwalk v0.51.23 and modified by the Crux project.
package foundation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const MaxProviderPresetBytes = 1 << 20

type ProviderType string

const ProviderTypeOpenAICompat ProviderType = "openai-compat"

type ProviderID string

type ProviderPreset struct {
	Name                string            `json:"name" jsonschema:"required,minLength=1,maxLength=128"`
	ID                  ProviderID        `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$,maxLength=64"`
	Type                ProviderType      `json:"type" jsonschema:"required,enum=openai-compat"`
	APIKey              string            `json:"api_key,omitempty" jsonschema:"pattern=^$|^\\$[A-Z][A-Z0-9_]*$"`
	APIEndpoint         string            `json:"api_endpoint,omitempty" jsonschema:"maxLength=2048"`
	DefaultLargeModelID string            `json:"default_large_model_id" jsonschema:"required,minLength=1,maxLength=256"`
	DefaultSmallModelID string            `json:"default_small_model_id" jsonschema:"required,minLength=1,maxLength=256"`
	Models              []ModelPreset     `json:"models" jsonschema:"required,minItems=1,maxItems=512"`
	DefaultHeaders      map[string]string `json:"default_headers,omitempty" jsonschema:"maxProperties=256"`
}

type ModelPresetOptions struct {
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	TopK             *int64         `json:"top_k,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	ProviderOptions  map[string]any `json:"provider_options,omitempty"`
}

type ModelPreset struct {
	ID                     string             `json:"id" jsonschema:"required,minLength=1,maxLength=256"`
	Name                   string             `json:"name" jsonschema:"required,minLength=1,maxLength=256"`
	CostPer1MIn            float64            `json:"cost_per_1m_in,omitempty" jsonschema:"minimum=0"`
	CostPer1MOut           float64            `json:"cost_per_1m_out,omitempty" jsonschema:"minimum=0"`
	CostPer1MInCached      float64            `json:"cost_per_1m_in_cached,omitempty" jsonschema:"minimum=0"`
	CostPer1MOutCached     float64            `json:"cost_per_1m_out_cached,omitempty" jsonschema:"minimum=0"`
	ContextWindow          int64              `json:"context_window" jsonschema:"required,minimum=1"`
	DefaultMaxTokens       int64              `json:"default_max_tokens" jsonschema:"required,minimum=1"`
	CanReason              bool               `json:"can_reason,omitempty"`
	ReasoningLevels        []string           `json:"reasoning_levels,omitempty" jsonschema:"uniqueItems=true,maxItems=32"`
	DefaultReasoningEffort string             `json:"default_reasoning_effort,omitempty" jsonschema:"maxLength=64"`
	SupportsImages         bool               `json:"supports_attachments,omitempty"`
	Options                ModelPresetOptions `json:"options,omitzero"`
}

var (
	providerPresetIDPattern     = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	providerPresetEnvPattern    = regexp.MustCompile(`^\$[A-Z][A-Z0-9_]*$`)
	providerPresetHeaderPattern = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")
)

func KnownProviderPresetTypes() []ProviderType {
	return []ProviderType{ProviderTypeOpenAICompat}
}

func DecodeProviderPreset(data []byte) (ProviderPreset, error) {
	if len(data) > MaxProviderPresetBytes {
		return ProviderPreset{}, fmt.Errorf("provider preset exceeds %d bytes", MaxProviderPresetBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var preset ProviderPreset
	if err := decoder.Decode(&preset); err != nil {
		return ProviderPreset{}, fmt.Errorf("decoding provider preset: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return ProviderPreset{}, errors.New("provider preset contains multiple JSON values")
		}
		return ProviderPreset{}, fmt.Errorf("decoding trailing provider preset data: %w", err)
	}
	if err := ValidateProviderPreset(preset); err != nil {
		return ProviderPreset{}, err
	}
	return preset, nil
}

func ValidateProviderPreset(preset ProviderPreset) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if strings.TrimSpace(preset.Name) == "" || len(preset.Name) > 128 {
		add("name is invalid")
	}
	if !providerPresetIDPattern.MatchString(string(preset.ID)) || len(preset.ID) > 64 {
		add("id is invalid")
	}
	if !slices.Contains(KnownProviderPresetTypes(), preset.Type) {
		add("type %q is unsupported", preset.Type)
	}
	if preset.APIKey != "" && !providerPresetEnvPattern.MatchString(preset.APIKey) {
		add("api_key must be an environment variable reference")
	}
	if preset.APIEndpoint != "" {
		endpoint, err := url.Parse(preset.APIEndpoint)
		if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" || (endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !isLoopbackHost(endpoint.Hostname()))) {
			add("api_endpoint must use HTTPS or loopback HTTP without credentials or a fragment")
		}
	}
	if len(preset.Models) == 0 || len(preset.Models) > 512 {
		add("models must contain between 1 and 512 entries")
	}
	if len(preset.DefaultHeaders) > 256 {
		add("default_headers exceeds 256 entries")
	}
	for name, value := range preset.DefaultHeaders {
		if len(name) > 256 || !providerPresetHeaderPattern.MatchString(name) {
			add("default_headers contains invalid header name %q", name)
		}
		if len(value) > 8192 || strings.ContainsAny(value, "\r\n") {
			add("default_headers[%q] contains an invalid value", name)
		}
	}
	modelIDs := make(map[string]struct{}, len(preset.Models))
	for index, model := range preset.Models {
		prefix := fmt.Sprintf("models[%d]", index)
		if model.ID == "" || len(model.ID) > 256 {
			add("%s.id is invalid", prefix)
		}
		if _, exists := modelIDs[model.ID]; exists {
			add("%s.id %q is duplicated", prefix, model.ID)
		}
		modelIDs[model.ID] = struct{}{}
		if strings.TrimSpace(model.Name) == "" || len(model.Name) > 256 {
			add("%s.name is invalid", prefix)
		}
		if model.ContextWindow < 1 || model.DefaultMaxTokens < 1 || model.DefaultMaxTokens > model.ContextWindow {
			add("%s token limits are invalid", prefix)
		}
		for _, cost := range []float64{model.CostPer1MIn, model.CostPer1MOut, model.CostPer1MInCached, model.CostPer1MOutCached} {
			if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
				add("%s costs must be finite and nonnegative", prefix)
				break
			}
		}
		if len(model.ReasoningLevels) > 32 {
			add("%s.reasoning_levels exceeds 32 entries", prefix)
		}
		seenReasoning := make(map[string]struct{}, len(model.ReasoningLevels))
		for _, level := range model.ReasoningLevels {
			if strings.TrimSpace(level) == "" || len(level) > 64 {
				add("%s.reasoning_levels contains an invalid value", prefix)
			}
			if _, exists := seenReasoning[level]; exists {
				add("%s.reasoning_levels contains duplicate %q", prefix, level)
			}
			seenReasoning[level] = struct{}{}
		}
		if !model.CanReason && (len(model.ReasoningLevels) > 0 || model.DefaultReasoningEffort != "") {
			add("%s reasoning configuration requires can_reason", prefix)
		}
		if model.DefaultReasoningEffort != "" && !slices.Contains(model.ReasoningLevels, model.DefaultReasoningEffort) {
			add("%s.default_reasoning_effort is not in reasoning_levels", prefix)
		}
	}
	for field, modelID := range map[string]string{
		"default_large_model_id": preset.DefaultLargeModelID,
		"default_small_model_id": preset.DefaultSmallModelID,
	} {
		if _, exists := modelIDs[modelID]; !exists {
			add("%s references unknown model %q", field, modelID)
		}
	}
	return errors.Join(errs...)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
