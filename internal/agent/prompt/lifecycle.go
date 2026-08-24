package prompt

import (
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/config"
)

var planningSectionIDs = map[SectionID]bool{
	"critical_rules":        true,
	"code_references":       true,
	"operating_constraints": true,
	"decision_making":       true,
	"memory":                true,
	"tool_usage":            true,
}

var executionSectionIDs = map[SectionID]bool{
	"critical_rules":        true,
	"code_references":       true,
	"workflow":              true,
	"operating_constraints": true,
	"decision_making":       true,
	"editing_files":         true,
	"whitespace":            true,
	"task_completion":       true,
	"memory":                true,
	"code_conventions":      true,
	"tool_usage":            true,
	"final_answers":         true,
}

func validateLifecycle(lifecycle Lifecycle) error {
	lifecycle.Plan = strings.TrimSpace(lifecycle.Plan)
	switch lifecycle.Stage {
	case LifecycleDefault, LifecycleDraft:
		if lifecycle.Plan != "" {
			return fmt.Errorf("lifecycle stage %q cannot include a persisted plan", lifecycle.Stage)
		}
	case LifecycleRevision, LifecycleExecution:
		if lifecycle.Plan == "" {
			return fmt.Errorf("lifecycle stage %q requires a persisted plan", lifecycle.Stage)
		}
	default:
		return fmt.Errorf("invalid lifecycle stage %q", lifecycle.Stage)
	}
	return nil
}

func lifecycleToolingInstructions(provider string, cfg *config.Config, stage LifecycleStage) (string, error) {
	if stage == LifecycleDefault {
		return toolingInstructions(provider, cfg)
	}

	allowed := planningSectionIDs
	if stage == LifecycleExecution {
		allowed = executionSectionIDs
	}
	sections := FilterSections(AllSections(), cfg.Options.DisabledInstructionSections)
	selected := make([]Section, 0, len(sections))
	for _, section := range sections {
		if allowed[section.ID] {
			selected = append(selected, section)
		}
	}
	return SectionsToString(selected), nil
}

func lifecycleInstructions(lifecycle Lifecycle) string {
	switch lifecycle.Stage {
	case LifecycleDraft:
		return `<plan_lifecycle stage="draft">
Plan drafting is active. Investigate the request and repository with the available read-only tools. Do not implement, edit files, execute processes, or claim the work is complete.

Build a complete, actionable implementation plan grounded in verified code. Memory, project and user context, skills, codebase search, diagnostics, and other read-only discovery services remain available when relevant.

State transitions:
- When the plan is complete, present it and call exit_plan with the same complete plan.
- User revision feedback moves the lifecycle to revision. Improve the persisted plan and call exit_plan again.
- Plan approval moves to approved execution. It does not restore normal instructions.
- Only complete_plan followed by user approval that the work itself is complete restores normal instructions.
</plan_lifecycle>`
	case LifecycleRevision:
		return fmt.Sprintf(`<plan_lifecycle stage="revision">
Revise the persisted plan. Focus on understanding the reviewer feedback, resolving plan gaps, and producing the complete improved plan. Do not implement changes.

Present the complete improved plan and call exit_plan with that same plan. Plan approval moves to approved execution. Only approval of completed implementation through complete_plan ends the lifecycle.

<persisted_plan>
%s
</persisted_plan>
</plan_lifecycle>`, strings.TrimSpace(lifecycle.Plan))
	case LifecycleExecution:
		return fmt.Sprintf(`<plan_lifecycle stage="execution">
Execute the user-approved plan below completely. Full implementation tools are available and their ordinary permission requests are automatically approved for this execution stage. Treat the plan as authoritative while following security boundaries, project and user context, relevant skills, memory guidance, code conventions, focused editing, and validation requirements.

Track progress, implement every plan item end to end, preserve unrelated work, and run relevant validation. Automatic tool approval does not bypass hooks or completion review. Plan approval does not restore normal mode.

When every plan item is implemented and validated, present a concise completion summary and call complete_plan with that same summary. If completion is not approved, continue the persisted plan using the user's feedback and call complete_plan again. Only user approval that the work itself is complete restores normal instructions.

<approved_plan>
%s
</approved_plan>
</plan_lifecycle>`, strings.TrimSpace(lifecycle.Plan))
	default:
		return ""
	}
}
