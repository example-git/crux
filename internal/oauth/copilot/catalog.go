package copilot

import "github.com/example-git/crux/foundation/catalog"

func CatalogProvider() catalog.Provider {
	return catalog.Provider{
		Name:                "GitHub Copilot",
		ID:                  "copilot",
		APIEndpoint:         "https://api.githubcopilot.com",
		Type:                "openai-compat",
		DefaultLargeModelID: "claude-sonnet-4.6",
		DefaultSmallModelID: "claude-haiku-4.5",
		Models: []catalog.Model{
			{ID: "claude-fable-5", Name: "Claude Fable 5", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-haiku-4.5", Name: "Claude Haiku 4.5", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 200000, DefaultMaxTokens: 32000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-opus-4.5", Name: "Claude Opus 4.5", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 200000, DefaultMaxTokens: 32000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-opus-4.7", Name: "Claude Opus 4.7", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-opus-4.8", Name: "Claude Opus 4.8", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-opus-4.8-fast", Name: "Claude Opus 4.8 (fast mode)", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-opus-5", Name: "Claude Opus 5", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-sonnet-4.5", Name: "Claude Sonnet 4.5", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 200000, DefaultMaxTokens: 32000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-sonnet-4.6", Name: "Claude Sonnet 4.6", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gemini-3.1-pro-preview", Name: "Gemini 3.1 Pro", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gemini-3.6-flash", Name: "Gemini 3.6 Flash", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-3.5-turbo", Name: "GPT 3.5 Turbo", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 16384, DefaultMaxTokens: 4096, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: false},
			{ID: "gpt-4", Name: "GPT 4", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 32768, DefaultMaxTokens: 4096, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: false},
			{ID: "gpt-4-0125-preview", Name: "GPT 4 Turbo", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 128000, DefaultMaxTokens: 4096, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: false},
			{ID: "gpt-4.1", Name: "GPT-4.1", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 128000, DefaultMaxTokens: 16384, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-4o", Name: "GPT-4o", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 128000, DefaultMaxTokens: 4096, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-4o-mini", Name: "GPT-4o mini", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 128000, DefaultMaxTokens: 4096, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: false},
			{ID: "gpt-5-mini", Name: "GPT-5 mini", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 264000, DefaultMaxTokens: 64000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-5.3-codex", Name: "GPT-5.3-Codex", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 400000, DefaultMaxTokens: 128000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-5.4", Name: "GPT-5.4", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 400000, DefaultMaxTokens: 128000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-5.4-mini", Name: "GPT-5.4 mini", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 400000, DefaultMaxTokens: 128000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-5.5", Name: "GPT-5.5", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 400000, DefaultMaxTokens: 128000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 328000, DefaultMaxTokens: 128000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 400000, DefaultMaxTokens: 128000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "gpt-5.6-terra", Name: "GPT-5.6 Terra", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 400000, DefaultMaxTokens: 128000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "grok-4.5", Name: "Grok 4.5", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 328000, DefaultMaxTokens: 128000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 256000, DefaultMaxTokens: 32000, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
			{ID: "kimi-k3", Name: "Kimi K3", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0, ContextWindow: 1048576, DefaultMaxTokens: 131072, CanReason: false, ReasoningLevels: nil, DefaultReasoningEffort: "", SupportsImages: true},
		},
	}
}
