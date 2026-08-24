package responses

import (
	"encoding/json"

	fantasy "github.com/example-git/crux/foundation"
)

// Global type identifiers for Codex-specific provider data.
const (
	TypeProviderOptions   = Name + ".options"
	TypeReasoningMetadata = Name + ".reasoning_metadata"
	TypeToolCallMetadata  = Name + ".tool_call_metadata"
	TypeCompactedHistory  = Name + ".compacted_history"
)

func init() {
	fantasy.RegisterProviderType(TypeProviderOptions, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var v ProviderOptions
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return &v, nil
	})
	fantasy.RegisterProviderType(TypeReasoningMetadata, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var v ReasoningMetadata
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return &v, nil
	})
	fantasy.RegisterProviderType(TypeToolCallMetadata, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var v ToolCallMetadata
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return &v, nil
	})
	fantasy.RegisterProviderType(TypeCompactedHistory, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var v CompactedHistory
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return &v, nil
	})
}

// ProviderOptions carries per-call Codex options.
type ProviderOptions struct {
	// ReasoningEffort is one of "none", "low", "medium", "high", "xhigh", "max".
	// Defaults to "medium".
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
	ResponseVerbosity string `json:"response_verbosity,omitempty"`
	DisableReasoning  bool   `json:"disable_reasoning,omitempty"`
}

// Options implements fantasy.ProviderOptionsData.
func (o *ProviderOptions) Options() {}

// MarshalJSON implements custom JSON marshaling with type info.
func (o ProviderOptions) MarshalJSON() ([]byte, error) {
	type plain ProviderOptions
	return fantasy.MarshalProviderType(TypeProviderOptions, plain(o))
}

// UnmarshalJSON implements custom JSON unmarshaling with type info.
func (o *ProviderOptions) UnmarshalJSON(data []byte) error {
	type plain ProviderOptions
	var p plain
	if err := fantasy.UnmarshalProviderType(data, &p); err != nil {
		return err
	}
	*o = ProviderOptions(p)
	return nil
}

// Options implements fantasy.ProviderOptionsData.
func (m *ReasoningMetadata) Options() {}

// MarshalJSON implements custom JSON marshaling with type info.
func (m ReasoningMetadata) MarshalJSON() ([]byte, error) {
	type plain ReasoningMetadata
	return fantasy.MarshalProviderType(TypeReasoningMetadata, plain(m))
}

// UnmarshalJSON implements custom JSON unmarshaling with type info.
func (m *ReasoningMetadata) UnmarshalJSON(data []byte) error {
	type plain ReasoningMetadata
	var p plain
	if err := fantasy.UnmarshalProviderType(data, &p); err != nil {
		return err
	}
	*m = ReasoningMetadata(p)
	return nil
}

// Options implements fantasy.ProviderOptionsData.
func (m *ToolCallMetadata) Options() {}

// MarshalJSON implements custom JSON marshaling with type info.
func (m ToolCallMetadata) MarshalJSON() ([]byte, error) {
	type plain ToolCallMetadata
	return fantasy.MarshalProviderType(TypeToolCallMetadata, plain(m))
}

// UnmarshalJSON implements custom JSON unmarshaling with type info.
func (m *ToolCallMetadata) UnmarshalJSON(data []byte) error {
	type plain ToolCallMetadata
	var p plain
	if err := fantasy.UnmarshalProviderType(data, &p); err != nil {
		return err
	}
	*m = ToolCallMetadata(p)
	return nil
}

// Options implements the ProviderOptionsData interface.
func (h *CompactedHistory) Options() {}

// MarshalJSON preserves the provider type across message persistence.
func (h CompactedHistory) MarshalJSON() ([]byte, error) {
	type plain CompactedHistory
	return fantasy.MarshalProviderType(TypeCompactedHistory, plain(h))
}

// UnmarshalJSON restores a persisted compacted history. It accepts both the
// inner plain shape ({"items": [...]}) used inside provider-option maps and
// the type-wrapped shape produced by MarshalJSON when the history is
// persisted as a direct struct field.
func (h *CompactedHistory) UnmarshalJSON(data []byte) error {
	type plain CompactedHistory
	var p plain
	if err := fantasy.UnmarshalProviderType(data, &p); err != nil {
		return err
	}
	if len(p.Items) == 0 {
		var wrapper struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Type == TypeCompactedHistory && len(wrapper.Data) > 0 {
			if err := json.Unmarshal(wrapper.Data, &p); err != nil {
				return err
			}
		}
	}
	*h = CompactedHistory(p)
	return nil
}
