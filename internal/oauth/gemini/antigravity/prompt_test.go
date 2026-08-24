package antigravity

import (
	"encoding/json"
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
