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
		"search with files mode",
		"search with content mode",
		"bash",
		"The main agent must prefer codebase_search over search",
		"the relevant files are indexed",
		"Do not use it while the index is being built or refreshed",
		"use LSP or search in content mode for known exact symbols and literals",
		"Do not inspect Git as a routine start/end step",
		"prefer path-scoped inspection in large repositories",
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
