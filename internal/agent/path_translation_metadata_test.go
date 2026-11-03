package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/anthropic"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/stretchr/testify/require"
)

func TestPathTranslationMetadataThroughNativeProviderRequest(t *testing.T) {
	state := t.TempDir()
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "metadata"}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: "/private/library", Recursive: true}, {Path: "/private/elsewhere/selected name 日本.txt"}}}})
	require.NoError(t, err)
	requests := make(chan string, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		requests <- string(body)
		var request struct {
			Stream bool `json:"stream"`
		}
		if json.Unmarshal(body, &request) == nil && request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []struct{ kind, data string }{
				{"message_start", `{"type":"message_start","message":{"id":"stream","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`},
				{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
				{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"stream reasoning"}}`},
				{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"stream-signature"}}`},
				{"content_block_stop", `{"type":"content_block_stop","index":0}`},
				{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
				{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}`},
				{"content_block_stop", `{"type":"content_block_stop","index":1}`},
				{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`},
				{"message_stop", `{"type":"message_stop"}`},
			} {
				_, _ = io.WriteString(w, "event: "+event.kind+"\ndata: "+event.data+"\n\n")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"response","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"thinking","thinking":"safe reasoning","signature":"fresh-signature"},{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":10}}`)
	}))
	defer server.Close()
	provider, err := anthropic.New(anthropic.WithAPIKey("fixture-key"), anthropic.WithBaseURL(server.URL))
	require.NoError(t, err)
	native, err := provider.LanguageModel(t.Context(), "claude-sonnet-4-20250514")
	require.NoError(t, err)
	resolveCalls := 0
	model := translatedLanguageModel{LanguageModel: native, resolve: func(ctx context.Context) (*pathtranslate.Translator, error) {
		resolveCalls++
		names, err := pathtranslate.NewStore(conn).Names(ctx)
		if err != nil {
			return nil, err
		}
		return pathtranslate.New(names, policy.Settings, "/private")
	}}
	old := fantasy.ReasoningPart{Text: "Read /private/library/private-file.txt", ProviderOptions: fantasy.ProviderOptions{anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "old-signature"}}}
	call := fantasy.Call{Prompt: fantasy.Prompt{
		fantasy.NewUserMessage("Continue with selected name 日本.txt."),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{old}},
		fantasy.NewUserMessage("Finish"),
	}}
	response, err := model.Generate(t.Context(), call)
	require.NoError(t, err)
	require.Equal(t, 1, resolveCalls)
	first := <-requests
	require.NotContains(t, first, "private-file")
	require.NotContains(t, first, "selected name 日本.txt")
	require.NotContains(t, first, "old-signature")
	require.Contains(t, first, pathtranslate.AliasPrefix)
	require.Contains(t, first, "Prior reasoning")
	var reasoning fantasy.ReasoningContent
	var text string
	for _, part := range response.Content {
		if value, ok := fantasy.AsContentType[fantasy.ReasoningContent](part); ok {
			reasoning = value
		}
		if value, ok := fantasy.AsContentType[fantasy.TextContent](part); ok {
			text += value.Text
		}
	}
	require.Equal(t, "safe reasoning", reasoning.Text)
	metadata, err := message.MetadataFromFantasyMetadata(message.ProviderMetadataScopeReasoning, reasoning.ProviderMetadata)
	require.NoError(t, err)
	persisted := message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.ReasoningContent{Thinking: reasoning.Text, ProviderMetadata: metadata}, message.TextContent{Text: text}}}
	call.Prompt[1] = persisted.ToAIMessage()[0]
	_, err = model.Generate(t.Context(), call)
	require.NoError(t, err)
	second := <-requests
	require.Contains(t, second, "fresh-signature")
	require.Contains(t, second, `"thinking":"safe reasoning"`)
	require.NotContains(t, second, "Prior reasoning")
	policy.Settings.Targets = append(policy.Settings.Targets, pathtranslate.Target{Path: "/private/another"})
	_, err = model.Generate(t.Context(), call)
	require.NoError(t, err)
	third := <-requests
	require.NotContains(t, third, "fresh-signature")
	require.Contains(t, third, "safe reasoning")
	require.Contains(t, third, "Prior reasoning")
	original, err := json.Marshal(persisted)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(metadata[0].Payload), "fresh-signature"))
	require.NotEmpty(t, original)
	require.Equal(t, "old-signature", old.ProviderOptions[anthropic.Name].(*anthropic.ReasoningOptionMetadata).Signature)
	stream, err := model.Stream(t.Context(), call)
	require.NoError(t, err)
	var streamMetadata fantasy.ProviderMetadata
	var streamText strings.Builder
	for part := range stream {
		require.NoError(t, part.Error)
		if part.Type == fantasy.StreamPartTypeReasoningDelta {
			streamText.WriteString(part.Delta)
		}
		if part.Type == fantasy.StreamPartTypeReasoningEnd {
			streamMetadata = part.ProviderMetadata
		}
	}
	<-requests
	require.Equal(t, "stream reasoning", streamText.String())
	require.NotEmpty(t, streamMetadata)
	call.Prompt[1].Content = []fantasy.MessagePart{fantasy.TextPart{Text: "done"}, fantasy.ReasoningPart{Text: streamText.String(), ProviderOptions: fantasy.ProviderOptions(streamMetadata)}}
	_, err = model.Generate(t.Context(), call)
	require.NoError(t, err)
	streamReplay := <-requests
	require.Contains(t, streamReplay, "stream-signature")
	require.Contains(t, streamReplay, `"thinking":"stream reasoning"`)
	require.Equal(t, 5, resolveCalls)
}

type translationObjectModel struct {
	finishStreamModel
	calls    []fantasy.ObjectCall
	metadata fantasy.ProviderMetadata
}

func (m *translationObjectModel) GenerateObject(_ context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.calls = append(m.calls, call)
	return &fantasy.ObjectResponse{Object: map[string]any{"result": "done"}, ProviderMetadata: m.metadata}, nil
}

func (m *translationObjectModel) StreamObject(_ context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	m.calls = append(m.calls, call)
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeFinish, ProviderMetadata: m.metadata})
	}, nil
}

func TestPathTranslationStructuredCallsPreserveSchemasAndCapturePolicy(t *testing.T) {
	state := t.TempDir()
	conn, err := db.Connect(t.Context(), state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(state)) })
	store := pathtranslate.NewStore(conn)
	policy, err := store.Save(t.Context(), pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: "object"}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: "/private/library", Recursive: true}}}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := pathtranslate.New(names, policy.Settings, "/private")
	require.NoError(t, err)
	provider := &translationObjectModel{metadata: fantasy.ProviderMetadata{anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "object-signature"}}}
	resolved := 0
	model := translatedLanguageModel{LanguageModel: provider, resolve: func(context.Context) (*pathtranslate.Translator, error) { resolved++; return translator, nil }}
	call := fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("Read /private/library/input.txt")}, SchemaDescription: "Files in /private/library", Schema: fantasy.Schema{Type: "object", Properties: map[string]*fantasy.Schema{"file": {Type: "string", Enum: []any{"/private/library/input.txt"}}}, Required: []string{"file"}}}
	_, err = model.GenerateObject(t.Context(), call)
	require.NoError(t, err)
	stream, err := model.StreamObject(t.Context(), call)
	require.NoError(t, err)
	for part := range stream {
		require.NoError(t, part.Error)
	}
	require.Equal(t, 2, resolved)
	require.Len(t, provider.calls, 2)
	for _, received := range provider.calls {
		encoded, err := json.Marshal(map[string]any{"prompt": received.Prompt, "schema": received.Schema, "description": received.SchemaDescription})
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "library")
		require.Contains(t, string(encoded), pathtranslate.AliasPrefix)
		require.Equal(t, []string{"file"}, received.Schema.Required)
	}
	require.Equal(t, "/private/library/input.txt", call.Schema.Properties["file"].Enum[0])
	prompt, err := translator.Prompt(t.Context(), fantasy.Prompt{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "reasoning", ProviderOptions: fantasy.ProviderOptions(provider.metadata)}}}})
	require.NoError(t, err)
	_, native := fantasy.AsMessagePart[fantasy.ReasoningPart](prompt[0].Content[0])
	require.True(t, native)
}
