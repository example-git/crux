package config

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// RemoteRuntimeControls carries execution choices, without copying client
// filesystem paths, daemon settings or UI preferences onto the execution host.
type RemoteRuntimeControls struct {
	DisableAutoSummarize        bool     `json:"disable_auto_summarize,omitempty"`
	SummarizationContextCap     int64    `json:"summarization_context_cap,omitempty"`
	SummarizationMaxTokens      int64    `json:"summarization_max_tokens,omitempty"`
	SummarizationFastMode       bool     `json:"summarization_fast_mode,omitempty"`
	CodexCompactionV2           bool     `json:"codex_compaction_v2,omitempty"`
	InstructionMode             string   `json:"instruction_mode,omitempty"`
	DisabledInstructionSections []string `json:"disabled_instruction_sections,omitempty"`
	ResponseVerbosity           string   `json:"response_verbosity,omitempty"`
	AnalysisEffort              string   `json:"analysis_effort,omitempty"`
}

func remoteControlsFromOptions(options *Options) RemoteRuntimeControls {
	if options == nil {
		return RemoteRuntimeControls{}
	}
	return RemoteRuntimeControls{
		DisableAutoSummarize:        options.DisableAutoSummarize,
		SummarizationContextCap:     options.SummarizationContextCap,
		SummarizationMaxTokens:      options.SummarizationMaxTokens,
		SummarizationFastMode:       options.SummarizationFastMode,
		CodexCompactionV2:           options.CodexCompactionV2,
		InstructionMode:             options.InstructionMode,
		DisabledInstructionSections: slices.Clone(options.DisabledInstructionSections),
		ResponseVerbosity:           options.ResponseVerbosity,
		AnalysisEffort:              options.AnalysisEffort,
	}
}

func (controls RemoteRuntimeControls) options() (*Options, error) {
	options := &Options{
		DisableAutoSummarize:        controls.DisableAutoSummarize,
		SummarizationContextCap:     controls.SummarizationContextCap,
		SummarizationMaxTokens:      controls.SummarizationMaxTokens,
		SummarizationFastMode:       controls.SummarizationFastMode,
		CodexCompactionV2:           controls.CodexCompactionV2,
		InstructionMode:             controls.InstructionMode,
		DisabledInstructionSections: slices.Clone(controls.DisabledInstructionSections),
		ResponseVerbosity:           controls.ResponseVerbosity,
		AnalysisEffort:              controls.AnalysisEffort,
	}
	if err := options.validatePromptOptions(); err != nil {
		return nil, err
	}
	if !slices.Contains([]string{"", "all", "project", "native"}, controls.InstructionMode) {
		return nil, errors.New("client instruction_mode must be all, project, or native")
	}
	return options, nil
}

func (controls RemoteRuntimeControls) apply(options *Options) {
	options.DisableAutoSummarize = controls.DisableAutoSummarize
	options.SummarizationContextCap = controls.SummarizationContextCap
	options.SummarizationMaxTokens = controls.SummarizationMaxTokens
	options.SummarizationFastMode = controls.SummarizationFastMode
	options.CodexCompactionV2 = controls.CodexCompactionV2
	options.InstructionMode = controls.InstructionMode
	options.DisabledInstructionSections = slices.Clone(controls.DisabledInstructionSections)
	options.ResponseVerbosity = controls.ResponseVerbosity
	options.AnalysisEffort = controls.AnalysisEffort
}

// ClientRuntimeSetting identifies fields whose effects are transported to
// the receiver, or are presentation preferences read from the client view.
func ClientRuntimeSetting(key string) bool {
	switch key {
	case "options.disable_auto_summarize", "options.summarization_context_cap", "options.summarization_max_tokens", "options.summarization_fast_mode", "options.codex_compaction_v2",
		"options.instruction_mode", "options.disabled_instruction_sections", "options.response_verbosity", "options.analysis_effort",
		"options.tui.compact_mode", "options.tui.transparent", "options.tui.delivery_mode", "options.notifications", "options.progress":
		return true
	default:
		return false
	}
}

func ClientPresentationSetting(key string) bool {
	switch key {
	case "options.tui.compact_mode", "options.tui.transparent", "options.tui.delivery_mode", "options.notifications", "options.progress":
		return true
	default:
		return false
	}
}

// WithClientPresentation copies local display choices without exposing local
// provider/model changes that have not been accepted by the receiver.
func (c *Config) WithClientPresentation(local *Config) *Config {
	next := c.cloneForWrite()
	if local == nil || local.Options == nil {
		return next
	}
	if next.Options == nil {
		next.Options = &Options{}
	}
	next.Options.TUI = clonePointer(local.Options.TUI)
	next.Options.Progress = clonePointer(local.Options.Progress)
	next.Options.Notifications = local.Options.Notifications
	return next
}

// ValidateClientRuntimeSetting checks transported field types and values before
// the generic disk setter runs. UI preferences keep their existing validation.
func ValidateClientRuntimeSetting(key string, value any) error {
	if !ClientRuntimeSetting(key) {
		return errors.New("unsupported client runtime setting")
	}
	if ClientPresentationSetting(key) {
		return nil
	}
	data, err := json.Marshal(map[string]any{strings.TrimPrefix(key, "options."): value})
	if err != nil {
		return errors.New("invalid client runtime setting value")
	}
	var controls RemoteRuntimeControls
	if err := json.Unmarshal(data, &controls); err != nil {
		return errors.New("invalid client runtime setting type")
	}
	_, err = controls.options()
	return err
}
