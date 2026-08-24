package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShellConfigOptionBooleans(t *testing.T) {
	store := loadCruxSh(t, `option debug true
option progress false
option auto-lsp false`)

	opts := store.Config().Options
	require.True(t, opts.Debug, "debug should be on")
	require.NotNil(t, opts.Progress)
	require.False(t, *opts.Progress, "progress should be off")
	require.NotNil(t, opts.AutoLSP)
	require.False(t, *opts.AutoLSP, "auto-lsp should be off")
}

func TestShellConfigOptionPromptControls(t *testing.T) {
	store := loadCruxSh(t, `option system-prompt-override true
option response-verbosity high
option analysis-effort max`)

	opts := store.Config().Options
	require.True(t, opts.SystemPromptOverride)
	require.Equal(t, "high", opts.ResponseVerbosity)
	require.Equal(t, "max", opts.AnalysisEffort)
}

func TestShellConfigOptionPromptControlsRejectInvalidValues(t *testing.T) {
	_, err := loadCruxShErr(t, `option response-verbosity verbose`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "response_verbosity")
}

func TestShellConfigOptionUI(t *testing.T) {
	store := loadCruxSh(t, `option ui compact true
option ui diff split
option ui transparent false
option ui scrollbar always
option ui completions-max-depth 4
option ui completions-max-items 200`)

	ui := store.Config().Options.TUI
	require.NotNil(t, ui)
	require.True(t, ui.CompactMode)
	require.Equal(t, "split", ui.DiffMode)
	require.NotNil(t, ui.Transparent)
	require.False(t, *ui.Transparent)
	require.Equal(t, "always", ui.Scrollbar)
	require.NotNil(t, ui.Completions.MaxDepth)
	require.Equal(t, 4, *ui.Completions.MaxDepth)
	require.NotNil(t, ui.Completions.MaxItems)
	require.Equal(t, 200, *ui.Completions.MaxItems)
}

func TestShellConfigOptionUIRejectsInvalidValue(t *testing.T) {
	_, err := loadCruxShErr(t, `option ui diff side-by-side`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects unified or split")
}

func TestShellConfigOptionListAppends(t *testing.T) {
	store := loadCruxSh(t, `option disable-skill crux-config
option disable-skill jq`)

	require.Subset(t, store.Config().Options.DisabledSkills, []string{"crux-config", "jq"})
}

// reset wipes values added earlier (or via source) while keeping anything
// added after it — observable in the effective config.
func TestShellConfigOptionReset(t *testing.T) {
	store := loadCruxSh(t, `option skill-path ./inherited-a
option skill-path ./inherited-b
option reset skill-path
option skill-path ./mine`)

	paths := store.Config().Options.SkillsPaths
	require.Contains(t, paths, "./mine")
	require.NotContains(t, paths, "./inherited-a")
	require.NotContains(t, paths, "./inherited-b")
}

func TestShellConfigOptionUnknownKeyFails(t *testing.T) {
	_, err := loadCruxShErr(t, `option bogus-key value`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestShellConfigOptionDisableToolRemoved(t *testing.T) {
	_, err := loadCruxShErr(t, `option disable-tool bash`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestShellConfigOptionResetRejectsNonList(t *testing.T) {
	_, err := loadCruxShErr(t, `option reset debug`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not one")
}
