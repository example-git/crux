package codex

import (
	"strings"
	"testing"
)

func TestModelsSupportImages(t *testing.T) {
	models := Models()
	if len(models) == 0 {
		t.Fatal("Codex model catalog is empty")
	}
	for _, model := range models {
		if !model.SupportsImages {
			t.Fatalf("model %q does not advertise image support", model.ID)
		}
	}
}

func TestStandardToolingInstructions(t *testing.T) {
	instructions := StandardToolingInstructions()

	for _, expected := range []string{
		"You are a coding agent running in the Codex CLI",
		"todos",
		"view",
		"edit",
		"write",
		"glob",
		"grep",
		"bash",
		"When codebase_search is available and its background index is ready",
		"Do not use it while the index is being built or refreshed",
		"use LSP or grep for known exact symbols and literals",
	} {
		if !strings.Contains(instructions, expected) {
			t.Fatalf("standard tooling instructions missing %q", expected)
		}
	}
	for _, unavailable := range []string{"apply_patch", "update_plan"} {
		if strings.Contains(instructions, unavailable) {
			t.Fatalf("standard tooling instructions mention unavailable tool %q", unavailable)
		}
	}
}
