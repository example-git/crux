package foundation

import (
	"encoding/json"
	"strings"
)

const (
	InstructionOptionsKey      = "crux.instructions"
	TypeInstructionPartOptions = "crux.instructions.part"
)

type InstructionKind string

const (
	InstructionKindTooling         InstructionKind = "tooling"
	InstructionKindProviderPrefix  InstructionKind = "provider-prefix"
	InstructionKindProviderContext InstructionKind = "provider-context"
	InstructionKindEnvironment     InstructionKind = "environment"
	InstructionKindRuntime         InstructionKind = "runtime"
	InstructionKindLifecycle       InstructionKind = "lifecycle"
	InstructionKindSkills          InstructionKind = "skills"
	InstructionKindProjectContext  InstructionKind = "project-context"
	InstructionKindUserContext     InstructionKind = "user-context"
	InstructionKindProjectState    InstructionKind = "project-state"
	InstructionKindMemory          InstructionKind = "memory"
	InstructionKindMCP             InstructionKind = "mcp"
	InstructionKindRetrieval       InstructionKind = "retrieval"
	InstructionKindSchema          InstructionKind = "schema"
	InstructionKindAuxiliary       InstructionKind = "auxiliary"
	InstructionKindOther           InstructionKind = "other"
)

type InstructionStability string

const (
	InstructionStabilityStatic  InstructionStability = "static"
	InstructionStabilityDynamic InstructionStability = "dynamic"
)

type InstructionPolicy string

const (
	InstructionPolicyGeneric   InstructionPolicy = "generic"
	InstructionPolicyAnthropic InstructionPolicy = "anthropic"
	InstructionPolicyCodex     InstructionPolicy = "codex"
)

type InstructionSection struct {
	Kind      InstructionKind
	Stability InstructionStability
	Text      string
}

type Instructions struct {
	sections []InstructionSection
}

type InstructionPartOptions struct {
	Kinds         []InstructionKind    `json:"kinds,omitempty"`
	Stability     InstructionStability `json:"stability"`
	CacheBoundary bool                 `json:"cache_boundary,omitempty"`
}

func init() {
	RegisterProviderType(TypeInstructionPartOptions, func(data []byte) (ProviderOptionsData, error) {
		var options InstructionPartOptions
		if err := json.Unmarshal(data, &options); err != nil {
			return nil, err
		}
		return &options, nil
	})
}

func (*InstructionPartOptions) Options() {}

func (o InstructionPartOptions) MarshalJSON() ([]byte, error) {
	type plain InstructionPartOptions
	return MarshalProviderType(TypeInstructionPartOptions, plain(o))
}

func (o *InstructionPartOptions) UnmarshalJSON(data []byte) error {
	type plain InstructionPartOptions
	var value plain
	if err := UnmarshalProviderType(data, &value); err != nil {
		return err
	}
	*o = InstructionPartOptions(value)
	return nil
}

func StaticInstruction(kind InstructionKind, text string) InstructionSection {
	return InstructionSection{Kind: kind, Stability: InstructionStabilityStatic, Text: text}
}

func DynamicInstruction(kind InstructionKind, text string) InstructionSection {
	return InstructionSection{Kind: kind, Stability: InstructionStabilityDynamic, Text: text}
}

func NewInstructions(sections ...InstructionSection) Instructions {
	return Instructions{sections: normalizeInstructionSections(sections)}
}

func (i Instructions) Append(sections ...InstructionSection) Instructions {
	combined := make([]InstructionSection, 0, len(i.sections)+len(sections))
	combined = append(combined, i.sections...)
	combined = append(combined, sections...)
	return NewInstructions(combined...)
}

func (i Instructions) Prepend(sections ...InstructionSection) Instructions {
	combined := make([]InstructionSection, 0, len(i.sections)+len(sections))
	combined = append(combined, sections...)
	combined = append(combined, i.sections...)
	return NewInstructions(combined...)
}

func (i Instructions) Sections() []InstructionSection {
	return append([]InstructionSection(nil), i.sections...)
}

func (i Instructions) Empty() bool {
	return len(i.sections) == 0
}

func (i Instructions) String() string {
	parts := make([]string, 0, len(i.sections))
	for _, section := range i.sections {
		parts = append(parts, section.Text)
	}
	return joinInstructionText(parts)
}

func (i Instructions) Message(policy InstructionPolicy) Message {
	lastStatic := -1
	for index, section := range i.sections {
		if section.Stability == InstructionStabilityStatic {
			lastStatic = index
		}
	}
	content := make([]MessagePart, 0, len(i.sections))
	for index, section := range i.sections {
		content = append(content, instructionTextPart(
			section.Text,
			[]InstructionKind{section.Kind},
			section.Stability,
			policy == InstructionPolicyAnthropic && index == lastStatic,
		))
	}
	return Message{Role: MessageRoleSystem, Content: content}
}

func AppendDynamicInstruction(prompt Prompt, kind InstructionKind, text string) Prompt {
	result := append(Prompt(nil), prompt...)
	if strings.TrimSpace(text) == "" {
		return result
	}

	part := instructionTextPart(text, []InstructionKind{kind}, InstructionStabilityDynamic, false)
	for index, message := range result {
		if message.Role != MessageRoleSystem {
			continue
		}
		message.Content = append(append([]MessagePart(nil), message.Content...), part)
		result[index] = message
		return result
	}

	return append(Prompt{{Role: MessageRoleSystem, Content: []MessagePart{part}}}, result...)
}

func InstructionPartOptionsFrom(options ProviderOptions) *InstructionPartOptions {
	value, ok := options[InstructionOptionsKey]
	if !ok {
		return nil
	}
	result, _ := value.(*InstructionPartOptions)
	return result
}

func instructionTextPart(text string, kinds []InstructionKind, stability InstructionStability, cacheBoundary bool) TextPart {
	return TextPart{
		Text: text,
		ProviderOptions: ProviderOptions{
			InstructionOptionsKey: &InstructionPartOptions{
				Kinds:         kinds,
				Stability:     stability,
				CacheBoundary: cacheBoundary,
			},
		},
	}
}

func normalizeInstructionSections(sections []InstructionSection) []InstructionSection {
	result := make([]InstructionSection, 0, len(sections))
	appendStability := func(stability InstructionStability) {
		for _, section := range sections {
			if strings.TrimSpace(section.Text) == "" || section.Stability != stability {
				continue
			}
			kind := section.Kind
			if kind == "" {
				kind = InstructionKindOther
			}
			result = append(result, InstructionSection{Kind: kind, Stability: stability, Text: section.Text})
		}
	}
	appendStability(InstructionStabilityStatic)
	appendStability(InstructionStabilityDynamic)
	for _, section := range sections {
		if section.Stability == InstructionStabilityStatic || section.Stability == InstructionStabilityDynamic {
			continue
		}
		if strings.TrimSpace(section.Text) == "" {
			continue
		}
		kind := section.Kind
		if kind == "" {
			kind = InstructionKindOther
		}
		result = append(result, InstructionSection{Kind: kind, Stability: InstructionStabilityDynamic, Text: section.Text})
	}
	return result
}

func joinInstructionText(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	var builder strings.Builder
	builder.WriteString(strings.TrimRight(parts[0], "\n"))
	for _, part := range parts[1:] {
		builder.WriteString("\n\n")
		builder.WriteString(strings.Trim(part, "\n"))
	}
	return builder.String()
}
