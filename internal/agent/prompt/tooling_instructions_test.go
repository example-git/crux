package prompt

import (
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth/codex"
)

func TestToolingInstructionsDefaultsToCrux(t *testing.T) {
	cfg := &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			codex.ID: {},
		}),
	}

	instructions, err := toolingInstructions(codex.ID, cfg)
	if err != nil {
		t.Fatalf("select default tooling instructions: %v", err)
	}
	expected := SectionsToString(AllSections())
	if instructions != expected {
		t.Fatal("default tooling instructions did not use Crux sections")
	}
}

func TestToolingInstructionsSelectsProviderNativeProfile(t *testing.T) {
	tests := []struct {
		providerID string
		expected   string
	}{
		{providerID: codex.ID, expected: codex.StandardToolingInstructions()},
	}

	for _, test := range tests {
		t.Run(test.providerID, func(t *testing.T) {
			cfg := &config.Config{
				Options: &config.Options{},
				Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
					test.providerID: {ToolingInstructions: config.ToolingInstructionsNative},
				}),
			}

			instructions, err := toolingInstructions(test.providerID, cfg)
			if err != nil {
				t.Fatalf("select native tooling instructions: %v", err)
			}
			if instructions != test.expected {
				t.Fatalf("native tooling instructions for %q did not match provider profile", test.providerID)
			}
		})
	}
}

func TestToolingInstructionsRejectsUnsupportedNativeProvider(t *testing.T) {
	cfg := &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"anthropic": {ToolingInstructions: config.ToolingInstructionsNative},
		}),
	}

	_, err := toolingInstructions("anthropic", cfg)
	if err == nil || !strings.Contains(err.Error(), "does not provide native tooling instructions") {
		t.Fatalf("expected unsupported provider error, got %v", err)
	}
}

func TestToolingInstructionsRejectsInvalidProfile(t *testing.T) {
	cfg := &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			codex.ID: {ToolingInstructions: "official"},
		}),
	}

	_, err := toolingInstructions(codex.ID, cfg)
	if err == nil || !strings.Contains(err.Error(), "unsupported tooling instruction profile") {
		t.Fatalf("expected invalid profile error, got %v", err)
	}
}

func TestToolingInstructionsOnlyFiltersCruxSections(t *testing.T) {
	cruxConfig := &config.Config{
		Options: &config.Options{DisabledInstructionSections: []string{"identity"}},
	}
	cruxInstructions, err := toolingInstructions(codex.ID, cruxConfig)
	if err != nil {
		t.Fatalf("select Crux tooling instructions: %v", err)
	}
	if strings.Contains(cruxInstructions, "You predict the next token") {
		t.Fatal("disabled Crux identity section remained in tooling instructions")
	}

	nativeConfig := &config.Config{
		Options: &config.Options{DisabledInstructionSections: []string{"identity"}},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			codex.ID: {ToolingInstructions: config.ToolingInstructionsNative},
		}),
	}
	nativeInstructions, err := toolingInstructions(codex.ID, nativeConfig)
	if err != nil {
		t.Fatalf("select native tooling instructions: %v", err)
	}
	if nativeInstructions != codex.StandardToolingInstructions() {
		t.Fatal("Crux section filters changed provider-native instructions")
	}
}
