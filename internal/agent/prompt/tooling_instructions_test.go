package prompt

import (
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/providerregistry"
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
	if !strings.Contains(instructions, "Do not inspect Git as a routine start/end step") {
		t.Fatal("default tooling instructions did not discourage routine Git inspection")
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
			cfg = config.NewTestStoreWithRegistrations(cfg, providerregistry.Integrated()...).RuntimeSnapshot().Config()

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

func TestToolingInstructionsRejectsSameIDPresetNativeProfile(t *testing.T) {
	cfg := &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			codex.ID: {
				Preset:              &config.ProviderPresetReference{ID: "example.codex-preset", Version: "1"},
				ToolingInstructions: config.ToolingInstructionsNative,
			},
		}),
	}

	_, err := toolingInstructions(codex.ID, cfg)
	if err == nil || !strings.Contains(err.Error(), "does not provide native tooling instructions") {
		t.Fatalf("expected same-ID preset native profile error, got %v", err)
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
	nativeConfig = config.NewTestStoreWithRegistrations(nativeConfig, providerregistry.Integrated()...).RuntimeSnapshot().Config()
	nativeInstructions, err := toolingInstructions(codex.ID, nativeConfig)
	if err != nil {
		t.Fatalf("select native tooling instructions: %v", err)
	}
	if nativeInstructions != codex.StandardToolingInstructions() {
		t.Fatal("Crux section filters changed provider-native instructions")
	}
}
