package message

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestProviderMetadataEnvelopePreservesExactPayload(t *testing.T) {
	payload := []byte(" { \"z\" : 1.2300e+02, \"a\" : \"<opaque>&\\u2028\" } \n")
	envelope, err := NewProviderMetadataEnvelope("missing.plugin/private", 7, ProviderMetadataScopeText, payload)
	if err != nil {
		t.Fatalf("create envelope: %v", err)
	}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var decoded ProviderMetadataEnvelope
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !bytes.Equal(decoded.Payload, payload) {
		t.Fatalf("payload changed:\n got %q\nwant %q", decoded.Payload, payload)
	}
}

func TestProviderMetadataSurvivesMessageMutationAndPartsRoundTrip(t *testing.T) {
	payload := []byte("{\n  \"unknown\": [3, 2, 1],\n  \"number\": -0.00e-0\n}")
	envelope, err := NewProviderMetadataEnvelope("absent.provider", 42, ProviderMetadataScopeText, payload)
	if err != nil {
		t.Fatalf("create envelope: %v", err)
	}
	msg := Message{Parts: []ContentPart{TextContent{
		Text:             "before",
		ProviderMetadata: ProviderMetadata{envelope},
	}}}

	msg.AppendContent(" after")
	encoded, err := marshalParts(msg.Parts)
	if err != nil {
		t.Fatalf("marshal parts: %v", err)
	}
	parts, err := unmarshalParts(encoded)
	if err != nil {
		t.Fatalf("unmarshal parts: %v", err)
	}
	text := parts[0].(TextContent)
	if text.Text != "before after" {
		t.Fatalf("unexpected text %q", text.Text)
	}
	if len(text.ProviderMetadata) != 1 || !bytes.Equal(text.ProviderMetadata[0].Payload, payload) {
		t.Fatalf("opaque payload changed after parts round trip: %q", text.ProviderMetadata[0].Payload)
	}

	clone := (&Message{Parts: parts}).Clone()
	cloneText := clone.Parts[0].(TextContent)
	cloneText.ProviderMetadata[0].Payload[0] = '['
	originalText := parts[0].(TextContent)
	if bytes.Equal(cloneText.ProviderMetadata[0].Payload, originalText.ProviderMetadata[0].Payload) {
		t.Fatal("clone shares opaque payload storage")
	}
}

func TestLegacyUnknownProviderOptionsMigrateWithoutPlugin(t *testing.T) {
	legacyPayload := []byte(`{"type":"text","data":{"text":"legacy","provider_options":{"missing.plugin": { "type" : "missing.private.v3", "data" : { "number" : 1.00e+2 } }}}}`)
	partsJSON := append([]byte("["), legacyPayload...)
	partsJSON = append(partsJSON, ']')

	parts, err := unmarshalParts(partsJSON)
	if err != nil {
		t.Fatalf("load legacy unknown metadata: %v", err)
	}
	text := parts[0].(TextContent)
	if len(text.ProviderMetadata) != 1 {
		t.Fatalf("expected migrated envelope, got %#v", text.ProviderMetadata)
	}
	if text.ProviderMetadata[0].Namespace != "missing.plugin" || text.ProviderMetadata[0].Scope != ProviderMetadataScopeText {
		t.Fatalf("unexpected envelope identity: %#v", text.ProviderMetadata[0])
	}
	want := []byte(`{ "type" : "missing.private.v3", "data" : { "number" : 1.00e+2 } }`)
	if !bytes.Equal(text.ProviderMetadata[0].Payload, want) {
		t.Fatalf("legacy payload changed:\n got %q\nwant %q", text.ProviderMetadata[0].Payload, want)
	}
}

func TestLegacyCodexAndGeminiFieldsMigrateToEnvelopes(t *testing.T) {
	partsJSON := []byte(`[
		{"type":"reasoning","data":{"thinking":"legacy","thought_signature":"gemini-signature","tool_id":"call-1","codex_reasoning_metadata":{"type":"codex.reasoning_metadata","data":{"item_id":"rs_1","encrypted_content":"opaque","summary":[]}}}},
		{"type":"text","data":{"text":"summary","codex_compacted_history":{"type":"codex.compacted_history","data":{"items":[{"type":"compaction","encrypted_content":"history"}]}}}},
		{"type":"tool_call","data":{"id":"call-1","name":"tool","input":"{}","finished":true,"codex_item_id":"fc_1"}}
	]`)

	parts, err := unmarshalParts(partsJSON)
	if err != nil {
		t.Fatalf("migrate legacy provider metadata: %v", err)
	}

	reasoning := parts[0].(ReasoningContent)
	if len(reasoning.ProviderMetadata) != 3 {
		t.Fatalf("expected google, antigravity, and codex reasoning envelopes, got %#v", reasoning.ProviderMetadata)
	}
	for i, namespace := range []string{"google", "antigravity", "codex"} {
		if reasoning.ProviderMetadata[i].Namespace != namespace || reasoning.ProviderMetadata[i].Scope != ProviderMetadataScopeReasoning {
			t.Fatalf("unexpected reasoning envelope %d: %#v", i, reasoning.ProviderMetadata[i])
		}
	}

	text := parts[1].(TextContent)
	if len(text.ProviderMetadata) != 1 || text.ProviderMetadata[0].Namespace != "codex" || text.ProviderMetadata[0].Scope != ProviderMetadataScopeCompaction {
		t.Fatalf("unexpected compaction envelope: %#v", text.ProviderMetadata)
	}

	toolCall := parts[2].(ToolCall)
	if len(toolCall.ProviderMetadata) != 1 || toolCall.ProviderMetadata[0].Namespace != "codex" || toolCall.ProviderMetadata[0].Scope != ProviderMetadataScopeToolCall {
		t.Fatalf("unexpected tool-call envelope: %#v", toolCall.ProviderMetadata)
	}
	if !bytes.Contains(toolCall.ProviderMetadata[0].Payload, []byte(`"item_id":"fc_1"`)) {
		t.Fatalf("Codex item ID was not migrated: %s", toolCall.ProviderMetadata[0].Payload)
	}
}

func TestMixedLegacyAndEnvelopeMetadataMergeWithoutOverwriting(t *testing.T) {
	partsJSON := []byte(`[
		{"type":"reasoning","data":{"thinking":"mixed","provider_metadata":[{"namespace":"future","version":9,"scope":"reasoning","payload":"eyJmdXR1cmUiOnRydWV9"}],"thought_signature":"legacy-signature","tool_id":"call-1"}},
		{"type":"text","data":{"text":"mixed","provider_metadata":[{"namespace":"missing.plugin","version":1,"scope":"text","payload":"eyJuZXciOnRydWV9"}],"provider_options":{"missing.plugin":{"legacy":true},"other.plugin":{"legacy":true}},"codex_compacted_history":{"history":true}}},
		{"type":"tool_call","data":{"id":"call-1","name":"tool","input":"{}","finished":true,"provider_metadata":[{"namespace":"codex","version":1,"scope":"tool-call","payload":"eyJuZXciOnRydWV9"}],"codex_item_id":"legacy-item","provider_options":{"other.plugin":{"legacy":true}}}}
	]`)

	parts, err := unmarshalParts(partsJSON)
	if err != nil {
		t.Fatalf("migrate mixed provider metadata: %v", err)
	}

	reasoning := parts[0].(ReasoningContent).ProviderMetadata
	if len(reasoning) != 3 || reasoning[0].Namespace != "future" || reasoning[1].Namespace != "google" || reasoning[2].Namespace != "antigravity" {
		t.Fatalf("mixed reasoning metadata was not merged in stable order: %#v", reasoning)
	}
	text := parts[1].(TextContent).ProviderMetadata
	if len(text) != 3 || text[0].Namespace != "missing.plugin" || text[1].Namespace != "other.plugin" || text[2].Scope != ProviderMetadataScopeCompaction {
		t.Fatalf("mixed text metadata was not merged or deduplicated: %#v", text)
	}
	toolCall := parts[2].(ToolCall).ProviderMetadata
	if len(toolCall) != 2 || toolCall[0].Namespace != "codex" || toolCall[1].Namespace != "other.plugin" {
		t.Fatalf("mixed tool-call metadata was not merged or deduplicated: %#v", toolCall)
	}
	if !bytes.Equal(toolCall[0].Payload, []byte(`{"new":true}`)) {
		t.Fatalf("new envelope was overwritten by legacy metadata: %s", toolCall[0].Payload)
	}
}

func TestMessageAndContinuationMetadataRoundTrip(t *testing.T) {
	messagePayload := []byte("{\"message\":true}")
	continuationPayload := []byte("{ \"response_id\" : \"resp_123\" }")
	messageEnvelope, err := NewProviderMetadataEnvelope("future.provider", 2, ProviderMetadataScopeMessage, messagePayload)
	if err != nil {
		t.Fatal(err)
	}
	continuationEnvelope, err := NewProviderMetadataEnvelope("future.provider", 2, ProviderMetadataScopeContinuation, continuationPayload)
	if err != nil {
		t.Fatal(err)
	}

	parts := []ContentPart{ProviderMetadataContent{ProviderMetadata: ProviderMetadata{messageEnvelope, continuationEnvelope}}}
	encoded, err := marshalParts(parts)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unmarshalParts(encoded)
	if err != nil {
		t.Fatal(err)
	}
	msg := Message{Role: Assistant, Parts: decoded}
	metadata := msg.MetadataContent()
	if len(metadata) != 2 || !bytes.Equal(metadata[0].Payload, messagePayload) || !bytes.Equal(metadata[1].Payload, continuationPayload) {
		t.Fatalf("message metadata changed: %#v", metadata)
	}
}

func TestUnknownProviderMetadataVersionIsPreservedButNotInterpreted(t *testing.T) {
	payload := []byte(`{"type":"codex.reasoning_metadata","data":{"encrypted_content":"opaque"}}`)
	envelope, err := NewProviderMetadataEnvelope("codex", 99, ProviderMetadataScopeReasoning, payload)
	if err != nil {
		t.Fatal(err)
	}
	metadata := ProviderMetadata{envelope}
	if options := metadata.FantasyOptions(ProviderMetadataScopeReasoning); len(options) != 0 {
		t.Fatalf("unknown metadata version was interpreted: %#v", options)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ProviderMetadata
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Version != 99 || !bytes.Equal(decoded[0].Payload, payload) {
		t.Fatalf("unknown metadata version was not preserved: %#v", decoded)
	}
}

func TestProviderMetadataRejectsInvalidFraming(t *testing.T) {
	tests := []ProviderMetadataEnvelope{
		{Version: 1, Scope: ProviderMetadataScopeText, Payload: []byte("{}")},
		{Namespace: "provider", Scope: ProviderMetadataScopeText, Payload: []byte("{}")},
		{Namespace: "provider", Version: 1, Scope: "invalid", Payload: []byte("{}")},
		{Namespace: "provider", Version: 1, Scope: ProviderMetadataScopeText, Payload: []byte("{} {}")},
	}
	for _, test := range tests {
		if err := test.Validate(); err == nil {
			t.Fatalf("expected validation error for %#v", test)
		}
	}
}
