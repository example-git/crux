package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
)

// Tool results must be sent as their own model-role turns: the endpoint
// rejects conversations where functionResponse arrives in a user turn.
func TestUserImagesUseInlineDataInContentOrder(t *testing.T) {
	t.Parallel()

	prompt := fantasy.Prompt{{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "before"},
			fantasy.FilePart{Filename: "pixel.png", MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
			fantasy.TextPart{Text: "after"},
		},
	}}

	_, contents, warnings := toWirePrompt(prompt)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 3 {
		t.Fatalf("contents = %+v", contents)
	}
	image := contents[0].Parts[1].InlineData
	if image == nil || image.MIMEType != "image/png" || !bytes.Equal(image.Data, []byte{0x89, 0x50, 0x4e, 0x47}) {
		t.Fatalf("image = %+v", image)
	}
	if contents[0].Parts[0].Text != "before" || contents[0].Parts[2].Text != "after" {
		t.Fatalf("part order = %+v", contents[0].Parts)
	}
	encoded, err := json.Marshal(contents[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"inlineData":{"mimeType":"image/png","data":"iVBORw=="}`) {
		t.Fatalf("wire image = %s", encoded)
	}
}

func TestToolResultsAreModelRoleTurns(t *testing.T) {
	t.Parallel()

	prompt := fantasy.Prompt{
		fantasy.Message{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: "run ls"},
			},
		},
		fantasy.Message{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: "bash", Input: `{"cmd":"ls"}`},
			},
		},
		fantasy.Message{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output:     fantasy.ToolResultOutputContentText{Text: "foo bar"},
				},
			},
		},
	}

	_, contents, _ := toWirePrompt(prompt)
	if len(contents) != 3 {
		t.Fatalf("got %d turns, want 3", len(contents))
	}
	last := contents[2]
	if last.Role != "model" {
		t.Errorf("tool result turn role = %q, want model", last.Role)
	}
	if len(last.Parts) != 1 || last.Parts[0].FunctionResponse == nil {
		t.Fatalf("tool result turn missing functionResponse: %+v", last.Parts)
	}
	fr := last.Parts[0].FunctionResponse
	if fr.ID != "call_1" || fr.Name != "bash" {
		t.Errorf("functionResponse identity = (%q, %q), want (call_1, bash)", fr.ID, fr.Name)
	}
	if got := fr.Response["output"]; got != "foo bar" {
		t.Errorf("functionResponse output = %v, want foo bar", got)
	}
}

// Thought signatures must be echoed on the thinking part, the functionCall,
// and the functionResponse turn, or the endpoint rejects the conversation.
func TestThoughtSignaturePropagation(t *testing.T) {
	t.Parallel()

	prompt := fantasy.Prompt{
		fantasy.Message{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{
					Text: "thinking...",
					ProviderOptions: fantasy.ProviderOptions{
						Name: &ReasoningMetadata{Signature: "sig-1", ToolID: "call_1"},
					},
				},
				fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: "bash", Input: `{}`},
			},
		},
		fantasy.Message{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output:     fantasy.ToolResultOutputContentText{Text: "ok"},
				},
			},
		},
	}

	_, contents, _ := toWirePrompt(prompt)
	if len(contents) != 2 {
		t.Fatalf("got %d turns, want 2", len(contents))
	}

	assistant := contents[0]
	if len(assistant.Parts) != 2 {
		t.Fatalf("assistant turn has %d parts, want 2", len(assistant.Parts))
	}
	thought := assistant.Parts[0]
	if !thought.Thought || thought.ThoughtSignature != "sig-1" {
		t.Errorf("thinking part = %+v, want thought with sig-1", thought)
	}
	if thought.Text != "" {
		t.Errorf("signed thinking part echoed text %q, want signature-only", thought.Text)
	}
	if got := assistant.Parts[1].ThoughtSignature; got != "sig-1" {
		t.Errorf("functionCall signature = %q, want sig-1", got)
	}

	result := contents[1]
	if got := result.Parts[0].ThoughtSignature; got != "sig-1" {
		t.Errorf("functionResponse signature = %q, want sig-1", got)
	}
}

func TestUnsignedThinkingIsOmitted(t *testing.T) {
	t.Parallel()

	prompt := fantasy.Prompt{
		fantasy.Message{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{Text: "hmm"},
				fantasy.TextPart{Text: "answer"},
			},
		},
	}

	_, contents, _ := toWirePrompt(prompt)
	if len(contents) != 1 || len(contents[0].Parts) != 1 {
		t.Fatalf("unexpected contents: %+v", contents)
	}
	if p := contents[0].Parts[0]; p.Thought || p.Text != "answer" || p.ThoughtSignature != "" {
		t.Errorf("assistant text part = %+v", p)
	}
}

// Tool error results are sent as output like tui-files does.
func TestToolErrorResult(t *testing.T) {
	t.Parallel()

	prompt := fantasy.Prompt{
		fantasy.Message{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: "bash", Input: `{}`},
			},
		},
		fantasy.Message{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output:     fantasy.ToolResultOutputContentError{Error: errors.New("boom")},
				},
			},
		},
	}

	_, contents, _ := toWirePrompt(prompt)
	last := contents[len(contents)-1]
	if got := last.Parts[0].FunctionResponse.Response["output"]; got != "boom" {
		t.Errorf("error output = %v, want boom", got)
	}
}

// Schema keywords the endpoint rejects must be stripped recursively.
func TestSanitizeGeminiSchema(t *testing.T) {
	t.Parallel()

	in := map[string]any{
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"n": map[string]any{
				"type":             "integer",
				"exclusiveMinimum": 0,
				"const":            5,
			},
		},
		"required": []any{"n"},
	}

	out, ok := sanitizeGeminiSchema(in).(map[string]any)
	if !ok {
		t.Fatal("sanitized schema is not a map")
	}
	for _, key := range []string{"$schema", "additionalProperties"} {
		if _, present := out[key]; present {
			t.Errorf("%s not stripped", key)
		}
	}
	if _, present := out["required"]; !present {
		t.Error("required was stripped")
	}
	n, ok := out["properties"].(map[string]any)["n"].(map[string]any)
	if !ok {
		t.Fatal("nested property lost")
	}
	for _, key := range []string{"exclusiveMinimum", "const"} {
		if _, present := n[key]; present {
			t.Errorf("nested %s not stripped", key)
		}
	}
	if n["type"] != "integer" {
		t.Errorf("nested type lost: %v", n["type"])
	}
}
