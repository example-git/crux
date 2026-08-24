package responses

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
)

func TestTruncateToolOutput(t *testing.T) {
	for _, test := range []struct {
		name    string
		modelID string
		limit   int
	}{
		{name: "unknown model byte fallback", modelID: "gpt-unknown", limit: 12_000},
		{name: "known byte model", modelID: "gpt-5.2", limit: 12_000},
		{name: "known token model", modelID: "gpt-5.6-sol", limit: 48_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := toolOutputLimitBytes(test.modelID); got != test.limit {
				t.Fatalf("limit = %d, want %d", got, test.limit)
			}
			value := strings.Repeat("a", test.limit)
			if got := TruncateToolOutput(test.modelID, value); got != value {
				t.Fatalf("output changed at limit")
			}

			value = "HEAD" + strings.Repeat("x", test.limit*2) + "TAIL"
			got := TruncateToolOutput(test.modelID, value)
			if len(got) > test.limit {
				t.Fatalf("truncated bytes = %d", len(got))
			}
			if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
				t.Fatalf("head or tail missing")
			}
			if !strings.Contains(got, toolOutputTruncationTag) {
				t.Fatalf("truncation marker missing")
			}
		})
	}

	t.Run("utf8 remains valid", func(t *testing.T) {
		modelID := "gpt-5.6-sol"
		limit := toolOutputLimitBytes(modelID)
		value := strings.Repeat("🙂漢字", limit)
		got := TruncateToolOutput(modelID, value)
		if !utf8.ValidString(got) {
			t.Fatalf("truncated output is invalid UTF-8")
		}
		if len(got) > limit {
			t.Fatalf("truncated bytes = %d", len(got))
		}
		if !strings.Contains(got, toolOutputTruncationTag) {
			t.Fatalf("truncation marker missing")
		}
	})
}

func TestWireToolsAreCanonicalizedByName(t *testing.T) {
	tools, warnings := toWireTools([]fantasy.Tool{
		fantasy.FunctionTool{Name: "zeta"},
		fantasy.FunctionTool{Name: "alpha"},
		fantasy.FunctionTool{Name: "middle"},
	})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if got := []string{tools[0].Name, tools[1].Name, tools[2].Name}; !slices.Equal(got, []string{"alpha", "middle", "zeta"}) {
		t.Fatalf("tool order = %v", got)
	}
}

func TestToInputBasic(t *testing.T) {
	prompt := fantasy.Prompt{
		fantasy.NewSystemMessage("be helpful"),
		fantasy.NewUserMessage("hello"),
	}
	instructions, items, warnings := toInput("gpt-test", prompt)
	if instructions != "be helpful" {
		t.Fatalf("instructions = %q", instructions)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(items) != 1 || items[0].Type != "message" || items[0].Role != "user" {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Content[0].Type != "input_text" || items[0].Content[0].Text != "hello" {
		t.Fatalf("content = %+v", items[0].Content)
	}
}

func TestToInputEncodesImagesInContentOrder(t *testing.T) {
	prompt := fantasy.Prompt{{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "before"},
			fantasy.FilePart{Filename: "pixel.png", MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
			fantasy.TextPart{Text: "after"},
		},
	}}

	_, items, warnings := toInput("gpt-test", prompt)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if len(items) != 1 || len(items[0].Content) != 3 {
		t.Fatalf("items = %+v", items)
	}
	image := items[0].Content[1]
	if image.Type != "input_image" || image.ImageURL != "data:image/png;base64,iVBORw==" || image.Detail != "high" {
		t.Fatalf("image = %+v", image)
	}
	if items[0].Content[0].Text != "before" || items[0].Content[2].Text != "after" {
		t.Fatalf("content order = %+v", items[0].Content)
	}
	encoded, err := json.Marshal(items[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"text":""`) {
		t.Fatalf("image serialized empty text: %s", encoded)
	}
}

func TestToInputKeepsImageOnlyMessagesAndWarnsForInvalidFiles(t *testing.T) {
	prompt := fantasy.Prompt{
		{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{
				fantasy.FilePart{MediaType: "image/jpeg", Data: []byte{1, 2, 3}},
			},
		},
		{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{
				fantasy.FilePart{MediaType: "application/pdf", Data: []byte("pdf")},
				fantasy.FilePart{MediaType: "image/png"},
			},
		},
	}

	_, items, warnings := toInput("gpt-test", prompt)
	if len(items) != 1 || len(items[0].Content) != 1 || items[0].Content[0].Type != "input_image" {
		t.Fatalf("items = %+v", items)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v", warnings)
	}
	if !strings.Contains(warnings[0].Message, "application/pdf") || !strings.Contains(warnings[1].Message, "empty") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestFunctionCallIDConverter(t *testing.T) {
	convert := newFunctionCallIDConverter()

	if got := convert("call_recXOBCVOQYcNr61QESWXUUL"); got != "fc_recXOBCVOQYcNr61QESWXUUL" {
		t.Fatalf("converted id = %q", got)
	}
	if got := convert("call_recXOBCVOQYcNr61QESWXUUL"); got != "fc_recXOBCVOQYcNr61QESWXUUL" {
		t.Fatalf("repeated converted id = %q", got)
	}
	if got := convert("fc_existing"); got != "fc_existing" {
		t.Fatalf("existing function-call id = %q", got)
	}
}

func TestToInputConvertsToolCallIDWhenItemIDIsMissing(t *testing.T) {
	prompt := fantasy.Prompt{
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{
					ToolCallID: "call_123",
					ToolName:   "view",
					Input:      `{}`,
				},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_123",
					Output:     fantasy.ToolResultOutputContentText{Text: "ok"},
				},
			},
		},
	}

	_, items, _ := toInput("gpt-test", prompt)
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].ID != "fc_123" || items[0].CallID != "call_123" {
		t.Fatalf("function call = %+v", items[0])
	}
}

func TestToInputToolRoundTrip(t *testing.T) {
	prompt := fantasy.Prompt{
		fantasy.NewUserMessage("read a file"),
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{
					Text: "thinking...",
					ProviderOptions: fantasy.ProviderOptions{
						Name: &ReasoningMetadata{
							ItemID:           "rs_1",
							EncryptedContent: "opaque",
						},
					},
				},
				fantasy.ToolCallPart{
					ToolCallID: "call_1",
					ToolName:   "view",
					Input:      `{"path":"main.go"}`,
					ProviderOptions: fantasy.ProviderOptions{
						Name: &ToolCallMetadata{ItemID: "fc_1"},
					},
				},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output:     fantasy.ToolResultOutputContentText{Text: "package main"},
				},
			},
		},
	}
	_, items, _ := toInput("gpt-test", prompt)

	var types []string
	for _, item := range items {
		types = append(types, item.Type)
	}
	want := []string{"message", "reasoning", "function_call", "function_call_output"}
	if len(types) != len(want) {
		t.Fatalf("types = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types = %v, want %v", types, want)
		}
	}

	fc := items[2]
	if fc.ID != "fc_1" || fc.CallID != "call_1" || fc.Name != "view" || fc.Arguments != `{"path":"main.go"}` {
		t.Fatalf("function_call = %+v", fc)
	}
	rs := items[1]
	if rs.ID != "rs_1" || rs.EncryptedContent != "opaque" || string(rs.Summary) != "[]" {
		t.Fatalf("reasoning = %+v", rs)
	}
	out := items[3]
	if out.CallID != "call_1" || out.Output == nil || *out.Output != "package main" {
		t.Fatalf("output = %+v", out)
	}
}

func TestToInputToolOutputTypesRemainValidAndBounded(t *testing.T) {
	large := strings.Repeat("tool output", 4_000)
	prompt := fantasy.Prompt{
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "call_text", ToolName: "text", Input: `{}`},
				fantasy.ToolCallPart{ToolCallID: "call_error", ToolName: "error", Input: `{}`},
				fantasy.ToolCallPart{ToolCallID: "call_media", ToolName: "media", Input: `{}`},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_text",
					Output:     fantasy.ToolResultOutputContentText{Text: large},
				},
				fantasy.ToolResultPart{
					ToolCallID: "call_error",
					Output:     fantasy.ToolResultOutputContentError{Error: errors.New(large)},
				},
				fantasy.ToolResultPart{
					ToolCallID: "call_media",
					Output: fantasy.ToolResultOutputContentMedia{
						MediaType: "image/png",
						Text:      large,
						Data:      "encoded-media",
					},
				},
			},
		},
	}

	_, items, warnings := toInput("gpt-5.2", prompt)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	outputs := make(map[string]string)
	for _, item := range items {
		if item.Type == "function_call_output" && item.Output != nil {
			outputs[item.CallID] = *item.Output
		}
	}
	if len(outputs) != 3 {
		t.Fatalf("outputs = %v", outputs)
	}
	for _, callID := range []string{"call_text", "call_error"} {
		output := outputs[callID]
		if len(output) > toolOutputLimitBytes("gpt-5.2") {
			t.Fatalf("%s output bytes = %d", callID, len(output))
		}
		if !utf8.ValidString(output) || !strings.Contains(output, toolOutputTruncationTag) {
			t.Fatalf("invalid bounded %s output = %q", callID, output)
		}
	}
	if outputs["call_media"] != "[Tool returned media content]" {
		t.Fatalf("media output = %q", outputs["call_media"])
	}
}

func TestSplitDynamicEnvironment(t *testing.T) {
	instructions := "native\n\n<env>\nWorking directory: /workspace\nIs directory a git repo: yes\nPlatform: darwin\nToday's date: 8/21/2026\n\nGit status (snapshot at conversation start - may be outdated):\nCurrent branch: main\nStatus: clean\n</env>\n\n<skills>stable</skills>"
	stable, dynamic := splitDynamicEnvironment(instructions)
	if stable != "native\n\n<env>\nWorking directory: /workspace\nIs directory a git repo: yes\nPlatform: darwin\n</env>\n\n<skills>stable</skills>" {
		t.Fatalf("stable instructions = %q", stable)
	}
	if dynamic != "Today's date: 8/21/2026\n\nGit status (snapshot at conversation start - may be outdated):\nCurrent branch: main\nStatus: clean" {
		t.Fatalf("dynamic environment = %q", dynamic)
	}

	withoutGit := "<env>\nWorking directory: /workspace\nIs directory a git repo: no\nPlatform: linux\nToday's date: 8/22/2026\n</env>"
	stable, dynamic = splitDynamicEnvironment(withoutGit)
	if stable != "<env>\nWorking directory: /workspace\nIs directory a git repo: no\nPlatform: linux\n</env>" {
		t.Fatalf("stable instructions without git = %q", stable)
	}
	if dynamic != "Today's date: 8/22/2026" {
		t.Fatalf("dynamic environment without git = %q", dynamic)
	}
}

func TestSplitDynamicEnvironmentFailsClosed(t *testing.T) {
	valid := "<env>\nWorking directory: /workspace\nIs directory a git repo: no\nPlatform: linux\nToday's date: 8/21/2026\n</env>"
	tests := map[string]string{
		"missing close":       strings.TrimSuffix(valid, "</env>"),
		"duplicate blocks":    valid + "\n" + valid,
		"invalid date":        strings.Replace(valid, "8/21/2026", "2026-08-21", 1),
		"task environment":    "<env>\nWorking directory: /workspace\nPlatform: linux\nToday's date: 8/21/2026\n</env>",
		"embedded close":      strings.Replace(valid, "/workspace", "/work</env>space", 1),
		"unexpected git data": strings.Replace(valid, "Today's date: 8/21/2026\n", "Today's date: 8/21/2026\n\nStatus: clean\n", 1),
	}
	for name, instructions := range tests {
		t.Run(name, func(t *testing.T) {
			stable, dynamic := splitDynamicEnvironment(instructions)
			if stable != instructions || dynamic != "" {
				t.Fatalf("split malformed environment: stable=%q dynamic=%q", stable, dynamic)
			}
		})
	}
}

func TestDynamicEnvironmentItemIsBoundedUTF8(t *testing.T) {
	item := dynamicEnvironmentItem("Today's date: 8/21/2026\n" + strings.Repeat("界", maxDynamicEnvironmentBytes))
	if item.Type != "message" || item.Role != "user" || len(item.Content) != 1 {
		t.Fatalf("item = %+v", item)
	}
	text := item.Content[0].Text
	if len(text) > maxDynamicEnvironmentBytes {
		t.Fatalf("dynamic environment bytes = %d", len(text))
	}
	if !utf8.ValidString(text) {
		t.Fatal("dynamic environment is invalid UTF-8")
	}
	if !strings.Contains(text, dynamicEnvironmentTag) {
		t.Fatalf("dynamic environment missing truncation tag: %q", text)
	}
}

func TestDropUnpairedCalls(t *testing.T) {
	items := []inputItem{
		{Type: "function_call", CallID: "a"},
		{Type: "function_call", CallID: "b"},
		{Type: "function_call_output", CallID: "a"},
		{Type: "function_call_output", CallID: "c"},
		{Type: "message", Role: "user"},
	}
	filtered := dropUnpairedCalls(items)
	if len(filtered) != 3 {
		t.Fatalf("filtered = %+v", filtered)
	}
	for _, item := range filtered {
		if item.Type == "function_call" && item.CallID != "a" {
			t.Fatalf("unpaired call kept: %+v", item)
		}
		if item.Type == "function_call_output" && item.CallID != "a" {
			t.Fatalf("unpaired output kept: %+v", item)
		}
	}
}

func TestPrepareRequestShape(t *testing.T) {
	g := &languageModel{modelID: "gpt-5.5", provider: Name, client: &client{}}
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	frame, _, err := g.prepareRequest(fantasy.Call{
		Prompt: fantasy.Prompt{
			fantasy.NewSystemMessage("sys"),
			fantasy.NewUserMessage("hi"),
		},
		Tools: []fantasy.Tool{fantasy.FunctionTool{Name: "view", InputSchema: schema}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "response.create" || m["model"] != "gpt-5.5" || m["stream"] != true {
		t.Fatalf("frame = %v", m)
	}
	if m["tool_choice"] != "auto" || m["parallel_tool_calls"] != true {
		t.Fatalf("tools frame = %v", m)
	}
	reasoning := m["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %v", reasoning)
	}
	include := m["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v", include)
	}
	text := m["text"].(map[string]any)["format"].(map[string]any)
	if text["type"] != "text" {
		t.Fatalf("text = %v", text)
	}
}

func TestPrepareRequestWarnsForUnsupportedOutputLimitWithoutSerializingIt(t *testing.T) {
	g := &languageModel{modelID: "gpt-5.5", provider: Name, client: &client{}}
	maxOutputTokens := int64(4096)
	frame, warnings, err := g.prepareRequest(fantasy.Call{
		Prompt:          fantasy.Prompt{fantasy.NewUserMessage("hi")},
		MaxOutputTokens: &maxOutputTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0].Type != fantasy.CallWarningTypeUnsupportedSetting || warnings[0].Setting != "max_output_tokens" {
		t.Fatalf("warnings = %+v", warnings)
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if _, ok := request["max_output_tokens"]; ok {
		t.Fatalf("unsupported output limit serialized: %s", data)
	}
	if _, ok := request["max_tokens"]; ok {
		t.Fatalf("invented output limit serialized: %s", data)
	}
}

func TestPrepareRequestKeepsDynamicEnvironmentOutOfFrameMetadata(t *testing.T) {
	g := &languageModel{modelID: "gpt-5.5", provider: Name, client: &client{}}
	frame, _, err := g.prepareRequest(fantasy.Call{Prompt: fantasy.Prompt{
		fantasy.NewSystemMessage("native\n\n<env>\nWorking directory: /workspace\nIs directory a git repo: no\nPlatform: linux\nToday's date: 8/21/2026\n</env>\n\nskills"),
		fantasy.NewUserMessage("hi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Instructions != "native\n\n<env>\nWorking directory: /workspace\nIs directory a git repo: no\nPlatform: linux\n</env>\n\nskills" {
		t.Fatalf("instructions = %q", frame.Instructions)
	}
	if frame.DynamicContext != "Today's date: 8/21/2026" {
		t.Fatalf("dynamic context = %q", frame.DynamicContext)
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "DynamicContext") || strings.Contains(string(data), "dynamic_context") {
		t.Fatalf("transport metadata serialized: %s", data)
	}
}

func TestPrepareRequestSerializesRuntimeControls(t *testing.T) {
	g := &languageModel{modelID: "gpt-5.6-sol", provider: Name, client: &client{}}
	frame, _, err := g.prepareRequest(fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")},
		ProviderOptions: fantasy.ProviderOptions{
			Name: &ProviderOptions{
				ReasoningEffort:   "max",
				ResponseVerbosity: "high",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if got := request["reasoning"].(map[string]any)["effort"]; got != "max" {
		t.Fatalf("reasoning effort = %v, want max", got)
	}
	if got := request["text"].(map[string]any)["verbosity"]; got != "high" {
		t.Fatalf("text verbosity = %v, want high", got)
	}
}

func TestPrepareRequestDisablesReasoning(t *testing.T) {
	g := &languageModel{modelID: "gpt-5.5", provider: Name, client: &client{}}
	frame, _, err := g.prepareRequest(fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")},
		ProviderOptions: fantasy.ProviderOptions{
			Name: &ProviderOptions{DisableReasoning: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Reasoning != nil {
		t.Fatalf("reasoning = %+v", frame.Reasoning)
	}
	if frame.Include != nil {
		t.Fatalf("include = %v", frame.Include)
	}
}

func TestToInputReplaysRoundtrippedCompactedHistory(t *testing.T) {
	// Simulate the auto-summarize continuation turn: the compacted history
	// produced by Compact is persisted as a TextContent field (type-wrapped
	// JSON), reloaded from the DB, and attached to the summary message's
	// text part. toInput must replay the history items verbatim.
	original := &CompactedHistory{Items: []inputItem{{
		Type: "message",
		Role: "user",
		Content: []messageContent{
			{Type: "input_text", Text: "handoff summary"},
			{Type: "input_image", ImageURL: "data:image/png;base64,iVBORw==", Detail: "high"},
		},
	}}}
	wrapped, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	restored := &CompactedHistory{}
	if err := json.Unmarshal(wrapped, restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Items) != 1 {
		t.Fatalf("roundtripped history items = %+v", restored.Items)
	}

	prompt := fantasy.Prompt{{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{
			Text:            "detailed summary report",
			ProviderOptions: fantasy.ProviderOptions{Name: restored},
		}},
	}}
	_, items, _ := toInput("gpt-test", prompt)
	if len(items) != 1 || items[0].Role != "user" {
		t.Fatalf("items = %+v", items)
	}
	if len(items[0].Content) != 2 || items[0].Content[0].Text != "handoff summary" {
		t.Fatalf("history not replayed, got %+v", items[0].Content)
	}
	image := items[0].Content[1]
	if image.Type != "input_image" || image.ImageURL != "data:image/png;base64,iVBORw==" || image.Detail != "high" {
		t.Fatalf("history image not replayed, got %+v", image)
	}
}
