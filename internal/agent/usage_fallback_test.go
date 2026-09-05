package agent

import (
	"errors"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestUsageIsZero(t *testing.T) {
	t.Parallel()

	require.True(t, usageIsZero(fantasy.Usage{}))
	require.False(t, usageIsZero(fantasy.Usage{InputTokens: 1}))
	require.False(t, usageIsZero(fantasy.Usage{OutputTokens: 1}))
	require.False(t, usageIsZero(fantasy.Usage{TotalTokens: 1}))
	require.False(t, usageIsZero(fantasy.Usage{ReasoningTokens: 1}))
	require.False(t, usageIsZero(fantasy.Usage{CacheCreationTokens: 1}))
	require.False(t, usageIsZero(fantasy.Usage{CacheReadTokens: 1}))
}

func TestFallbackStepUsageKeepsProviderUsage(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  15,
	}
	step := fantasy.StepResult{
		Response: fantasy.Response{Usage: usage},
	}

	fallbackUsage, estimated := fallbackStepUsage(nil, step)
	require.False(t, estimated)
	require.Equal(t, usage, fallbackUsage)
}

func TestFallbackStepUsageEstimatesPromptAndAssistantText(t *testing.T) {
	t.Parallel()

	messages := []fantasy.Message{
		fantasy.NewUserMessage("please explain the implementation details"),
	}
	step := fantasy.StepResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "the implementation stores state safely"},
			},
		},
	}

	usage, estimated := fallbackStepUsage(messages, step)
	require.True(t, estimated)
	require.Positive(t, usage.InputTokens)
	require.Positive(t, usage.OutputTokens)
	require.Equal(t, usage.InputTokens+usage.OutputTokens, usage.TotalTokens)
}

func TestFallbackStepUsageEstimatesReasoning(t *testing.T) {
	t.Parallel()

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{Text: "first reason about the request"},
			},
		},
	}
	step := fantasy.StepResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.ReasoningContent{Text: "second reason about the answer"},
			},
		},
	}

	usage, estimated := fallbackStepUsage(messages, step)
	require.True(t, estimated)
	require.Positive(t, usage.InputTokens)
	require.Positive(t, usage.OutputTokens)
}

func TestFallbackStepUsageEstimatesToolCalls(t *testing.T) {
	t.Parallel()

	step := fantasy.StepResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.ToolCallContent{
					ToolCallID: "tool-call-1",
					ToolName:   "view",
					Input:      `{"file_path":"/tmp/example.go"}`,
				},
			},
		},
	}

	usage, estimated := fallbackStepUsage(nil, step)
	require.True(t, estimated)
	require.Zero(t, usage.InputTokens)
	require.Positive(t, usage.OutputTokens)
	require.Equal(t, usage.OutputTokens, usage.TotalTokens)
}

func TestFallbackStepUsageEstimatesToolResults(t *testing.T) {
	t.Parallel()

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "tool-call-1",
					Output: fantasy.ToolResultOutputContentText{
						Text: "file contents returned by the tool",
					},
				},
				fantasy.ToolResultPart{
					ToolCallID: "tool-call-2",
					Output: fantasy.ToolResultOutputContentError{
						Error: errors.New("permission denied"),
					},
				},
				fantasy.ToolResultPart{
					ToolCallID: "tool-call-3",
					Output: fantasy.ToolResultOutputContentMedia{
						MediaType: "image/png",
						Text:      "screenshot",
						Data:      "abc123",
					},
				},
			},
		},
	}

	usage, estimated := fallbackStepUsage(messages, fantasy.StepResult{})
	require.True(t, estimated)
	require.Positive(t, usage.InputTokens)
	require.Zero(t, usage.OutputTokens)
	require.Equal(t, usage.InputTokens, usage.TotalTokens)
}

func TestFallbackStepUsageSkipsClientToolResultsAsOutput(t *testing.T) {
	t.Parallel()

	step := fantasy.StepResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.ToolResultContent{
					ToolCallID: "tool-call-1",
					ToolName:   "bash",
					Result: fantasy.ToolResultOutputContentText{
						Text: "large client-executed payload that should not count as model output tokens",
					},
				},
			},
		},
	}

	usage, estimated := fallbackStepUsage(nil, step)
	require.False(t, estimated)
	require.Zero(t, usage.OutputTokens)
}

func TestFallbackStepUsageCountsProviderToolResultsAsOutput(t *testing.T) {
	t.Parallel()

	step := fantasy.StepResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.ToolResultContent{
					ToolCallID:       "tool-call-1",
					ToolName:         "web_search",
					ProviderExecuted: true,
					ClientMetadata:   "provider metadata",
					Result:           fantasy.ToolResultOutputContentText{Text: "provider-executed result"},
				},
			},
		},
	}

	usage, estimated := fallbackStepUsage(nil, step)
	require.True(t, estimated)
	require.Positive(t, usage.OutputTokens)
	require.Equal(t, usage.OutputTokens, usage.TotalTokens)
}

func TestFallbackStepUsageReturnsZeroWithoutContent(t *testing.T) {
	t.Parallel()

	usage, estimated := fallbackStepUsage(nil, fantasy.StepResult{})
	require.False(t, estimated)
	require.True(t, usageIsZero(usage))
}

func TestUpdateSessionUsageSkipsEstimatedCost(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{ID: "session-id", Cost: 1.25}
	model := Model{CatalogModel: catalog.Model{CostPer1MIn: 10, CostPer1MOut: 20}}
	usage := fantasy.Usage{InputTokens: 1000, OutputTokens: 2000}

	agent.updateSessionUsage(model, currentSession, usage, nil, true)

	require.Equal(t, 1.25, currentSession.Cost)
	require.Equal(t, int64(1000), currentSession.PromptTokens)
	require.Equal(t, int64(2000), currentSession.CompletionTokens)
	require.True(t, currentSession.EstimatedUsage)
}

func TestUpdateSessionUsagePreservesAuthoritativeCodexOccupancy(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{
		ModelCfg:          config.SelectedModel{Provider: codexresponses.Name},
		InstructionPolicy: fantasy.InstructionPolicyCodex,
	}
	currentSession := &session.Session{
		ID:               "session-id",
		PromptTokens:     100_000,
		CompletionTokens: 5_000,
		Cost:             1.25,
	}

	agent.updateSessionUsage(model, currentSession, fantasy.Usage{
		InputTokens:  40_000,
		OutputTokens: 2_000,
		TotalTokens:  42_000,
	}, nil, true)

	require.Equal(t, int64(100_000), currentSession.PromptTokens)
	require.Equal(t, int64(5_000), currentSession.CompletionTokens)
	require.Equal(t, 1.25, currentSession.Cost)
	require.False(t, currentSession.EstimatedUsage)
}

func TestUpdateSessionUsageAllowsCodexEstimateWithoutAuthoritativeOccupancy(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{
		ModelCfg:          config.SelectedModel{Provider: codexresponses.Name},
		InstructionPolicy: fantasy.InstructionPolicyCodex,
	}
	currentSession := &session.Session{ID: "session-id"}

	agent.updateSessionUsage(model, currentSession, fantasy.Usage{InputTokens: 40_000, OutputTokens: 2_000}, nil, true)
	require.Equal(t, int64(40_000), currentSession.PromptTokens)
	require.Equal(t, int64(2_000), currentSession.CompletionTokens)
	require.True(t, currentSession.EstimatedUsage)

	agent.updateSessionUsage(model, currentSession, fantasy.Usage{InputTokens: 45_000, OutputTokens: 3_000}, nil, true)
	require.Equal(t, int64(45_000), currentSession.PromptTokens)
	require.Equal(t, int64(3_000), currentSession.CompletionTokens)
	require.True(t, currentSession.EstimatedUsage)

	agent.updateSessionUsage(model, currentSession, fantasy.Usage{InputTokens: 50_000, OutputTokens: 4_000}, nil, false)
	require.Equal(t, int64(50_000), currentSession.PromptTokens)
	require.Equal(t, int64(4_000), currentSession.CompletionTokens)
	require.False(t, currentSession.EstimatedUsage)
}

func TestUpdateSessionUsageDoesNotApplyCodexAccountingToSameIDGenericOwner(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	model := Model{
		ModelCfg:          config.SelectedModel{Provider: codexresponses.Name},
		InstructionPolicy: fantasy.InstructionPolicyGeneric,
	}
	currentSession := &session.Session{
		ID:               "session-id",
		PromptTokens:     100_000,
		CompletionTokens: 5_000,
	}

	agent.updateSessionUsage(model, currentSession, fantasy.Usage{InputTokens: 40_000, OutputTokens: 2_000, TotalTokens: 60_000}, nil, true)
	require.Equal(t, int64(40_000), currentSession.PromptTokens)
	require.Equal(t, int64(2_000), currentSession.CompletionTokens)
	require.True(t, currentSession.EstimatedUsage)
}

func TestUpdateSessionUsageKeepsCountersForZeroUsage(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{
		ID:               "session-id",
		PromptTokens:     123,
		CompletionTokens: 456,
		Cost:             1.25,
	}
	model := Model{CatalogModel: catalog.Model{CostPer1MIn: 10, CostPer1MOut: 20}}

	agent.updateSessionUsage(model, currentSession, fantasy.Usage{}, nil, false)

	require.Equal(t, 1.25, currentSession.Cost)
	require.Equal(t, int64(123), currentSession.PromptTokens)
	require.Equal(t, int64(456), currentSession.CompletionTokens)
}

func TestUpdateSessionUsagePreservesOmittedCountersForPartialUsage(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{
		ID:               "session-id",
		PromptTokens:     123,
		CompletionTokens: 456,
	}
	model := Model{CatalogModel: catalog.Model{CostPer1MIn: 10, CostPer1MOut: 20}}
	usage := fantasy.Usage{InputTokens: 789}

	agent.updateSessionUsage(model, currentSession, usage, nil, false)

	require.Equal(t, int64(789), currentSession.PromptTokens)
	require.Equal(t, int64(456), currentSession.CompletionTokens)
}

func TestUpdateSessionUsagePreservesCountersForTotalOnlyUsage(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{
		ID:               "session-id",
		PromptTokens:     123,
		CompletionTokens: 456,
	}
	model := Model{CatalogModel: catalog.Model{CostPer1MIn: 10, CostPer1MOut: 20}}
	usage := fantasy.Usage{TotalTokens: 100}

	agent.updateSessionUsage(model, currentSession, usage, nil, false)

	require.Equal(t, int64(123), currentSession.PromptTokens)
	require.Equal(t, int64(456), currentSession.CompletionTokens)
}

func TestUpdateSessionUsagePreservesPromptForOutputOnlyUsage(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{
		ID:               "session-id",
		PromptTokens:     123,
		CompletionTokens: 456,
	}
	model := Model{CatalogModel: catalog.Model{CostPer1MIn: 10, CostPer1MOut: 20}}
	usage := fantasy.Usage{OutputTokens: 50}

	agent.updateSessionUsage(model, currentSession, usage, nil, false)

	require.Equal(t, int64(123), currentSession.PromptTokens)
	require.Equal(t, int64(50), currentSession.CompletionTokens)
}

func TestUpdateSessionUsageKeepsCountersForEstimatedZeroUsage(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{
		ID:               "session-id",
		PromptTokens:     123,
		CompletionTokens: 456,
		Cost:             1.25,
	}
	model := Model{CatalogModel: catalog.Model{CostPer1MIn: 10, CostPer1MOut: 20}}

	agent.updateSessionUsage(model, currentSession, fantasy.Usage{}, nil, true)

	require.Equal(t, 1.25, currentSession.Cost)
	require.Equal(t, int64(123), currentSession.PromptTokens)
	require.Equal(t, int64(456), currentSession.CompletionTokens)
}

func TestSummaryCompletionTokens(t *testing.T) {
	t.Parallel()

	summaryMessage := message.Message{
		Parts: []message.ContentPart{
			message.TextContent{Text: "summary text"},
			message.ReasoningContent{Thinking: "reasoning text"},
		},
	}

	require.Equal(t, int64(42), summaryCompletionTokens(fantasy.Usage{OutputTokens: 42}, summaryMessage))
	require.Equal(t, approxTokenCount("summary text")+approxTokenCount("reasoning text"), summaryCompletionTokens(fantasy.Usage{}, summaryMessage))
	require.Zero(t, summaryCompletionTokens(fantasy.Usage{}, message.Message{}))
}

func TestUpdateSessionUsageCountsCachedInputOnce(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{ID: "session-id"}
	model := Model{CatalogModel: catalog.Model{
		CostPer1MIn:        10,
		CostPer1MOutCached: 2,
	}}
	usage := fantasy.Usage{
		InputTokens:     20_000,
		CacheReadTokens: 80_000,
		TotalTokens:     100_000,
	}

	agent.updateSessionUsage(model, currentSession, usage, nil, false)

	require.Equal(t, int64(100_000), currentSession.PromptTokens)
	require.InDelta(t, 0.36, currentSession.Cost, 0.000001)
}

func TestUpdateSessionUsageUsesAuthoritativeCodexTotalForMalformedCacheUsage(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{ID: "session-id"}
	model := Model{
		ModelCfg:          config.SelectedModel{Provider: codexresponses.Name},
		InstructionPolicy: fantasy.InstructionPolicyCodex,
		CatalogModel: catalog.Model{
			CostPer1MIn:        10,
			CostPer1MOut:       20,
			CostPer1MOutCached: 2,
		},
	}
	usage := fantasy.Usage{
		CacheReadTokens: 120,
		OutputTokens:    10,
		TotalTokens:     110,
	}

	agent.updateSessionUsage(model, currentSession, usage, nil, false)

	require.Equal(t, int64(100), currentSession.PromptTokens)
	require.Equal(t, int64(10), currentSession.CompletionTokens)
	require.InDelta(t, 0.0004, currentSession.Cost, 0.0000001)
}

func TestShouldAutoCompactUsesNormalizedOccupancy(t *testing.T) {
	t.Parallel()

	triggered, threshold := shouldAutoCompact(140_000, 100_000, 10_000, 0, 0, false)
	require.Equal(t, int64(28_000), threshold)
	require.False(t, triggered)

	triggered, _ = shouldAutoCompact(140_000, 180_000, 10_000, 0, 0, false)
	require.True(t, triggered)

	triggered, _ = shouldAutoCompact(140_000, 100_000, 10_000, 2_000, 0, false)
	require.True(t, triggered)

	triggered, _ = shouldAutoCompact(140_000, 180_000, 10_000, 0, 0, true)
	require.False(t, triggered)
}

func TestShouldAutoCompactUsesProviderRetainedTokenBudget(t *testing.T) {
	t.Parallel()

	triggered, threshold := shouldAutoCompact(100_000, 75_000, 0, 0, 30_000, false)
	require.Equal(t, int64(30_000), threshold)
	require.True(t, triggered)

	triggered, threshold = shouldAutoCompact(100_000, 75_000, 0, 0, 10_000, false)
	require.Equal(t, int64(10_000), threshold)
	require.False(t, triggered)
}

func TestUpdateSessionUsageAddsProviderCost(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	currentSession := &session.Session{ID: "session-id", Cost: 1.25}
	model := Model{CatalogModel: catalog.Model{CostPer1MIn: 10, CostPer1MOut: 20}}
	usage := fantasy.Usage{InputTokens: 1000, OutputTokens: 2000}

	agent.updateSessionUsage(model, currentSession, usage, nil, false)

	require.Equal(t, 1.3, currentSession.Cost)
	require.Equal(t, int64(1000), currentSession.PromptTokens)
	require.Equal(t, int64(2000), currentSession.CompletionTokens)
	require.False(t, currentSession.EstimatedUsage)
}
