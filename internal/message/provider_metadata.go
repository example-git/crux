package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	fantasy "github.com/example-git/crux/foundation"
)

// ProviderMetadataScope identifies where opaque provider metadata is attached.
type ProviderMetadataScope string

const (
	ProviderMetadataScopeMessage      ProviderMetadataScope = "message"
	ProviderMetadataScopeReasoning    ProviderMetadataScope = "reasoning"
	ProviderMetadataScopeText         ProviderMetadataScope = "text"
	ProviderMetadataScopeToolCall     ProviderMetadataScope = "tool-call"
	ProviderMetadataScopeToolResult   ProviderMetadataScope = "tool-result"
	ProviderMetadataScopeContinuation ProviderMetadataScope = "continuation"
	ProviderMetadataScopeCompaction   ProviderMetadataScope = "compaction"
)

// ProviderMetadataEnvelope preserves one versioned provider-owned JSON value.
// Payload is []byte intentionally: encoding/json transports it as base64, so
// the authoritative JSON bytes are not normalized while the envelope itself is
// marshaled through the database, REST, or SSE layers.
type ProviderMetadataEnvelope struct {
	Namespace string                `json:"namespace"`
	Version   int                   `json:"version"`
	Scope     ProviderMetadataScope `json:"scope"`
	Payload   []byte                `json:"payload"`
}

// NewProviderMetadataEnvelope validates and copies an opaque JSON payload.
func NewProviderMetadataEnvelope(namespace string, version int, scope ProviderMetadataScope, payload []byte) (ProviderMetadataEnvelope, error) {
	envelope := ProviderMetadataEnvelope{
		Namespace: namespace,
		Version:   version,
		Scope:     scope,
		Payload:   bytes.Clone(payload),
	}
	if err := envelope.Validate(); err != nil {
		return ProviderMetadataEnvelope{}, err
	}
	return envelope, nil
}

// NewProviderMetadataValue encodes a known in-process value into an envelope.
// Unknown envelopes never use this path: their authoritative Payload bytes are
// copied without semantic decoding.
func NewProviderMetadataValue(namespace string, version int, scope ProviderMetadataScope, value any) (ProviderMetadataEnvelope, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return ProviderMetadataEnvelope{}, fmt.Errorf("encode provider metadata payload: %w", err)
	}
	return NewProviderMetadataEnvelope(namespace, version, scope, payload)
}

// UnmarshalJSON validates envelope framing while preserving decoded payload bytes.
func (e *ProviderMetadataEnvelope) UnmarshalJSON(data []byte) error {
	type envelopeAlias ProviderMetadataEnvelope
	var decoded envelopeAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	candidate := ProviderMetadataEnvelope(decoded)
	if err := candidate.Validate(); err != nil {
		return err
	}
	candidate.Payload = bytes.Clone(candidate.Payload)
	*e = candidate
	return nil
}

// Validate checks envelope framing without interpreting the provider payload.
func (e ProviderMetadataEnvelope) Validate() error {
	if e.Namespace == "" {
		return fmt.Errorf("provider metadata namespace is required")
	}
	if e.Version <= 0 {
		return fmt.Errorf("provider metadata version must be positive")
	}
	switch e.Scope {
	case ProviderMetadataScopeMessage,
		ProviderMetadataScopeReasoning,
		ProviderMetadataScopeText,
		ProviderMetadataScopeToolCall,
		ProviderMetadataScopeToolResult,
		ProviderMetadataScopeContinuation,
		ProviderMetadataScopeCompaction:
	default:
		return fmt.Errorf("invalid provider metadata scope %q", e.Scope)
	}
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		return fmt.Errorf("provider metadata payload must contain one valid JSON value")
	}
	return nil
}

// Clone returns a deep copy suitable for mutation-safe message cloning.
func (e ProviderMetadataEnvelope) Clone() ProviderMetadataEnvelope {
	e.Payload = bytes.Clone(e.Payload)
	return e
}

// ProviderMetadata is an ordered collection. Order and duplicate envelope
// identities are preserved; provider adapters choose how to interpret them.
type ProviderMetadata []ProviderMetadataEnvelope

// Clone returns a deep copy of every envelope and payload.
func (m ProviderMetadata) Clone() ProviderMetadata {
	if len(m) == 0 {
		return nil
	}
	clone := make(ProviderMetadata, len(m))
	for i := range m {
		clone[i] = m[i].Clone()
	}
	return clone
}

// MetadataFromFantasyOptions captures currently interpreted Fantasy values as
// opaque envelopes. Provider names are sorted to make newly generated metadata
// deterministic; already-persisted envelope order is never changed.
func MetadataFromFantasyOptions(scope ProviderMetadataScope, options fantasy.ProviderOptions) (ProviderMetadata, error) {
	if len(options) == 0 {
		return nil, nil
	}
	providers := make([]string, 0, len(options))
	for provider := range options {
		providers = append(providers, provider)
	}
	sort.Strings(providers)

	metadata := make(ProviderMetadata, 0, len(providers))
	for _, provider := range providers {
		envelope, err := NewProviderMetadataValue(provider, 1, scope, options[provider])
		if err != nil {
			return nil, fmt.Errorf("encode %s provider metadata: %w", provider, err)
		}
		metadata = append(metadata, envelope)
	}
	return metadata, nil
}

// MetadataFromFantasyMetadata captures streamed provider metadata.
func MetadataFromFantasyMetadata(scope ProviderMetadataScope, metadata fantasy.ProviderMetadata) (ProviderMetadata, error) {
	return MetadataFromFantasyOptions(scope, fantasy.ProviderOptions(metadata))
}

func migrateLegacyReasoningMetadata(data []byte, content *ReasoningContent) error {
	var legacy struct {
		Signature              string          `json:"signature"`
		ThoughtSignature       string          `json:"thought_signature"`
		ToolID                 string          `json:"tool_id"`
		CodexReasoningMetadata json.RawMessage `json:"codex_reasoning_metadata"`
		ResponsesData          json.RawMessage `json:"responses_data"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	var metadata ProviderMetadata
	if legacy.Signature != "" {
		payload, err := legacyTypedPayload("anthropic.reasoning_metadata", map[string]any{"signature": legacy.Signature})
		if err != nil {
			return err
		}
		metadata = append(metadata, mustLegacyEnvelope("anthropic", ProviderMetadataScopeReasoning, payload))
	}
	if legacy.ThoughtSignature != "" {
		value := map[string]any{"signature": legacy.ThoughtSignature, "tool_id": legacy.ToolID}
		for _, provider := range []string{"google", "antigravity"} {
			payload, err := legacyTypedPayload(provider+".reasoning_metadata", value)
			if err != nil {
				return err
			}
			metadata = append(metadata, mustLegacyEnvelope(provider, ProviderMetadataScopeReasoning, payload))
		}
	}
	if hasJSONValue(legacy.ResponsesData) {
		envelope, envelopeErr := NewProviderMetadataEnvelope("openai", 1, ProviderMetadataScopeReasoning, legacy.ResponsesData)
		if envelopeErr != nil {
			return envelopeErr
		}
		metadata = append(metadata, envelope)
	}
	if hasJSONValue(legacy.CodexReasoningMetadata) {
		envelope, envelopeErr := NewProviderMetadataEnvelope("codex", 1, ProviderMetadataScopeReasoning, legacy.CodexReasoningMetadata)
		if envelopeErr != nil {
			return envelopeErr
		}
		metadata = append(metadata, envelope)
	}
	content.ProviderMetadata = mergeLegacyMetadata(content.ProviderMetadata, metadata)
	return nil
}

func migrateLegacyTextMetadata(data []byte, content *TextContent) error {
	var legacy struct {
		ProviderOptions       map[string]json.RawMessage `json:"provider_options"`
		CodexCompactedHistory json.RawMessage            `json:"codex_compacted_history"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	metadata, err := metadataFromLegacyMap(ProviderMetadataScopeText, legacy.ProviderOptions)
	if err != nil {
		return err
	}
	if hasJSONValue(legacy.CodexCompactedHistory) {
		envelope, envelopeErr := NewProviderMetadataEnvelope("codex", 1, ProviderMetadataScopeCompaction, legacy.CodexCompactedHistory)
		if envelopeErr != nil {
			return envelopeErr
		}
		metadata = append(metadata, envelope)
	}
	content.ProviderMetadata = mergeLegacyMetadata(content.ProviderMetadata, metadata)
	return nil
}

func migrateLegacyToolCallMetadata(data []byte, content *ToolCall) error {
	var legacy struct {
		CodexItemID     string                     `json:"codex_item_id"`
		ProviderOptions map[string]json.RawMessage `json:"provider_options"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	metadata, err := metadataFromLegacyMap(ProviderMetadataScopeToolCall, legacy.ProviderOptions)
	if err != nil {
		return err
	}
	if legacy.CodexItemID != "" {
		payload, payloadErr := legacyTypedPayload("codex.tool_call_metadata", map[string]any{"item_id": legacy.CodexItemID})
		if payloadErr != nil {
			return payloadErr
		}
		metadata = append(metadata, mustLegacyEnvelope("codex", ProviderMetadataScopeToolCall, payload))
	}
	content.ProviderMetadata = mergeLegacyMetadata(content.ProviderMetadata, metadata)
	return nil
}

func mergeLegacyMetadata(current, legacy ProviderMetadata) ProviderMetadata {
	merged := current.Clone()
	for _, candidate := range legacy {
		found := false
		for _, existing := range current {
			if existing.Namespace == candidate.Namespace && existing.Version == candidate.Version && existing.Scope == candidate.Scope {
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, candidate.Clone())
		}
	}
	return merged
}

func metadataFromLegacyMap(scope ProviderMetadataScope, values map[string]json.RawMessage) (ProviderMetadata, error) {
	providers := make([]string, 0, len(values))
	for provider := range values {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	metadata := make(ProviderMetadata, 0, len(providers))
	for _, provider := range providers {
		payload := []byte(values[provider])
		if provider == "codex" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(values[provider], &fields); err == nil {
				if _, wrapped := fields["type"]; !wrapped {
					typeID := ""
					switch {
					case fields["item_id"] != nil:
						typeID = "codex.tool_call_metadata"
					case fields["items"] != nil:
						typeID = "codex.compacted_history"
					case fields["encrypted_content"] != nil:
						typeID = "codex.reasoning_metadata"
					}
					if typeID != "" {
						wrappedPayload, wrapErr := legacyTypedRawPayload(typeID, values[provider])
						if wrapErr != nil {
							return nil, wrapErr
						}
						payload = wrappedPayload
					}
				}
			}
		}
		envelope, err := NewProviderMetadataEnvelope(provider, 1, scope, payload)
		if err != nil {
			return nil, err
		}
		metadata = append(metadata, envelope)
	}
	return metadata, nil
}

func legacyTypedPayload(typeID string, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return legacyTypedRawPayload(typeID, data)
}

func legacyTypedRawPayload(typeID string, data json.RawMessage) ([]byte, error) {
	return json.Marshal(struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}{Type: typeID, Data: data})
}

func mustLegacyEnvelope(namespace string, scope ProviderMetadataScope, payload []byte) ProviderMetadataEnvelope {
	return ProviderMetadataEnvelope{Namespace: namespace, Version: 1, Scope: scope, Payload: bytes.Clone(payload)}
}

func hasJSONValue(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// FantasyOptions resolves envelopes known to the active core/provider adapter.
// Unknown namespaces, versions, and types remain preserved in ProviderMetadata
// and are deliberately omitted from the transient Fantasy request map.
func (m ProviderMetadata) FantasyOptions(scope ProviderMetadataScope) fantasy.ProviderOptions {
	var options fantasy.ProviderOptions
	for _, envelope := range m {
		if envelope.Scope != scope || envelope.Version != 1 {
			continue
		}
		decoded, err := fantasy.UnmarshalProviderOptions(map[string]json.RawMessage{
			envelope.Namespace: json.RawMessage(envelope.Payload),
		})
		if err != nil {
			continue
		}
		if options == nil {
			options = make(fantasy.ProviderOptions)
		}
		options[envelope.Namespace] = decoded[envelope.Namespace]
	}
	return options
}
