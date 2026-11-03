package antigravity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
)

// Tool calls must always carry an input object. The endpoint rejects the
// entire request when one arrives without it ("tool_use.input: Field
// required"), and dropping the call instead would orphan the tool result
// that follows it.
func TestToolCallAlwaysHasArgs(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "null", "{}", "not json", `{"a":1}`} {
		prompt := fantasy.Prompt{
			fantasy.Message{
				Role: fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{
					fantasy.ToolCallPart{
						ToolCallID: "call_1",
						ToolName:   "bash",
						Input:      input,
					},
				},
			},
		}

		_, content, _ := toWirePrompt(prompt)
		if len(content) == 0 || len(content[0].Parts) == 0 {
			t.Fatalf("input %q: tool call was dropped", input)
		}
		fc := content[0].Parts[0].FunctionCall
		if fc == nil {
			t.Fatalf("input %q: no function call emitted", input)
		}
		if fc.Args == nil {
			t.Errorf("input %q: Args is nil; would be omitted on the wire", input)
			continue
		}
		// A nil map marshals to "null"; an empty one must marshal to "{}".
		encoded, err := json.Marshal(fc.Args)
		if err != nil {
			t.Fatalf("input %q: marshal: %v", input, err)
		}
		if string(encoded) == "null" {
			t.Errorf("input %q: Args marshalled to null", input)
		}
		if fc.Name != "bash" || fc.ID != "call_1" {
			t.Errorf("input %q: call identity lost: %+v", input, fc)
		}
	}
}

func TestThirdTurnSignedThinkingOnGeminiWire(t *testing.T) {
	t.Parallel()

	for _, thinking := range []string{"signed thought\nwith original whitespace  ", ""} {
		t.Run(thinking, func(t *testing.T) {
			requests := make(chan wireEnvelope, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1internal:streamGenerateContent" {
					http.NotFound(w, r)
					return
				}
				var envelope wireEnvelope
				if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				requests <- envelope
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"third answer\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))
			}))
			defer server.Close()

			provider, err := New(WithBaseURL(server.URL), WithHTTPClient(server.Client()), WithUserAgent("test-agent"))
			if err != nil {
				t.Fatal(err)
			}
			model, err := provider.LanguageModel(t.Context(), "claude-sonnet-5-5-high")
			if err != nil {
				t.Fatal(err)
			}
			prompt := fantasy.Prompt{
				fantasy.NewUserMessage("first question"),
				{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "first answer"}}},
				fantasy.NewUserMessage("second question"),
				{
					Role: fantasy.MessageRoleAssistant,
					Content: []fantasy.MessagePart{
						fantasy.TextPart{Text: "second answer"},
						fantasy.ReasoningPart{Text: thinking, ProviderOptions: fantasy.ProviderOptions{
							Name: &ReasoningMetadata{Signature: "second-response-signature"},
						}},
					},
				},
				fantasy.NewUserMessage("third question"),
			}
			stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: prompt})
			if err != nil {
				t.Fatal(err)
			}
			finished := false
			for part := range stream {
				if part.Type == fantasy.StreamPartTypeError {
					t.Fatal(part.Error)
				}
				finished = finished || part.Type == fantasy.StreamPartTypeFinish
			}
			if !finished {
				t.Fatal("stream did not finish")
			}
			envelope := <-requests
			if envelope.Model != "claude-sonnet-5-5-high" || envelope.Request == nil || len(envelope.Request.Contents) != 5 {
				t.Fatalf("unexpected Gemini request envelope: %+v", envelope)
			}
			contents := envelope.Request.Contents
			for index, role := range []string{"user", "model", "user", "model", "user"} {
				if contents[index].Role != role {
					t.Fatalf("turn %d role = %q, want %q", index, contents[index].Role, role)
				}
			}
			if len(contents[1].Parts) != 1 || contents[1].Parts[0].Thought || contents[1].Parts[0].Text != "first answer" {
				t.Fatalf("first answer changed: %+v", contents[1])
			}
			parts := contents[3].Parts
			if len(parts) != 2 || !parts[0].Thought || parts[0].Text != thinking || parts[0].ThoughtSignature != "second-response-signature" {
				t.Fatalf("signed thinking was changed or reordered: %+v", parts)
			}
			if parts[1].Text != "second answer" || parts[1].Thought || parts[1].ThoughtSignature != "" {
				t.Fatalf("answer text changed: %+v", parts[1])
			}
		})
	}
}
