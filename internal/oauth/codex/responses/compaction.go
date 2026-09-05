package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	fantasy "github.com/example-git/crux/foundation"
)

const (
	remoteCompactionSummary = "Conversation compacted by Codex"
	compactionBytesPerToken = 4
)

func (g *languageModel) Compact(ctx context.Context, call fantasy.Call) (*CompactionResult, error) {
	started := time.Now()
	frame, _, err := g.prepareRequest(call)
	if err != nil {
		return nil, err
	}
	profile := g.client.compaction.clone()
	if profile.retry.MaxAttempts < 1 {
		return nil, fmt.Errorf("codex: remote compaction execution policy is unavailable")
	}

	conversationID := call.Headers["x-session-id"]
	frame.Input = append(frame.Input, inputItem{Type: "compaction_trigger"})
	frame.RequestKind = "compaction"
	attempt, err := g.compactRemoteV2Attempt(ctx, frame, conversationID, profile)
	if err != nil {
		g.clearCompactionChain(conversationID)
		slog.Debug("Codex compaction failed",
			"implementation", CompactionRemoteV2,
			"duration", time.Since(started),
			"error_type", fmt.Sprintf("%T", err),
		)
		return nil, err
	}

	history := &CompactedHistory{Items: []inputItem{attempt.compaction}}
	result := &CompactionResult{
		History:           history,
		Summary:           remoteCompactionSummary,
		Usage:             attempt.usage,
		UsageAvailable:    attempt.usageAvailable,
		Implementation:    CompactionRemoteV2,
		ActiveInputTokens: approximateInputTokens(history.Items),
		finalize: func() {
			g.clearCompactionChain(conversationID)
		},
	}
	slog.Debug("Codex compaction completed",
		"implementation", result.Implementation,
		"usage_available", result.UsageAvailable,
		"active_input_tokens", result.ActiveInputTokens,
		"duration", time.Since(started),
	)
	return result, nil
}

type remoteCompactionV2Attempt struct {
	compaction     inputItem
	usage          fantasy.Usage
	usageAvailable bool
}

func (g *languageModel) compactRemoteV2Attempt(ctx context.Context, frame *requestFrame, conversationID string, profile executionProfile) (remoteCompactionV2Attempt, error) {
	events := g.client.streamWithProfile(ctx, frame, g.provider, conversationID, "conversation", profile)
	outputItems := 0
	compactionItems := 0
	var compaction inputItem
	for event, err := range events {
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return remoteCompactionV2Attempt{}, contextErr
			}
			return remoteCompactionV2Attempt{}, fmt.Errorf("codex: remote compaction v2 stream closed before response.completed: %w: %w", err, io.ErrUnexpectedEOF)
		}
		switch event.Type {
		case "response.output_item.done":
			outputItems++
			var item inputItem
			if err := json.Unmarshal(event.Item, &item); err != nil {
				return remoteCompactionV2Attempt{}, fmt.Errorf("codex: decode compaction output item: %w", err)
			}
			if item.Type == "compaction" {
				compactionItems++
				if compactionItems == 1 {
					compaction = item
				}
			}
		case "response.failed", "response.incomplete":
			var responseError *wireError
			if event.Response != nil {
				responseError = event.Response.Error
			}
			return remoteCompactionV2Attempt{}, resolvedCodexEventError(event, responseError, "codex remote compaction v2 did not complete")
		case "error":
			return remoteCompactionV2Attempt{}, resolvedCodexEventError(event, event.Error, "codex remote compaction v2 failed")
		case "response.completed":
			if event.Response == nil {
				return remoteCompactionV2Attempt{}, fmt.Errorf("codex: compaction response missing payload")
			}
			if event.Response.Error != nil {
				return remoteCompactionV2Attempt{}, resolvedCodexEventError(event, event.Response.Error, "codex remote compaction v2 failed")
			}
			if event.Response.ID == "" {
				return remoteCompactionV2Attempt{}, fmt.Errorf("codex: remote compaction v2 completed without a response id")
			}
			if compactionItems != 1 || compaction.EncryptedContent == "" {
				return remoteCompactionV2Attempt{}, fmt.Errorf("codex: remote compaction v2 expected exactly one encrypted compaction output item, got %d from %d output items", compactionItems, outputItems)
			}
			return remoteCompactionV2Attempt{
				compaction:     compaction,
				usage:          usageFromWire(event.Response.Usage),
				usageAvailable: event.Response.Usage != nil,
			}, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return remoteCompactionV2Attempt{}, err
	}
	return remoteCompactionV2Attempt{}, fmt.Errorf("codex: remote compaction v2 stream closed before response.completed: %w", io.ErrUnexpectedEOF)
}

func (g *languageModel) clearCompactionChain(conversationID string) {
	if g.client.sessionStore == nil || conversationID == "" {
		return
	}
	token := ""
	if g.client.token != nil {
		token = g.client.token()
	}
	accountID := g.client.chatGPTAccountID(token)
	account := accountDiscriminator(accountID, token)
	g.client.sessionStore.clearChain(newTransportStateKey(
		g.client.url,
		g.provider,
		account,
		g.modelID,
		conversationID,
		"conversation",
		g.client.transportIdentity(),
	))
}

func usageFromWire(usage *wireUsage) fantasy.Usage {
	if usage == nil {
		return fantasy.Usage{}
	}
	cachedTokens := int64(0)
	cacheWriteTokens := int64(0)
	if usage.InputTokensDetails != nil {
		cachedTokens = max(usage.InputTokensDetails.CachedTokens, 0)
		cacheWriteTokens = max(usage.InputTokensDetails.CacheWriteTokens, 0)
	}
	result := fantasy.Usage{
		InputTokens:         max(usage.InputTokens-cachedTokens, 0),
		OutputTokens:        usage.OutputTokens,
		TotalTokens:         usage.InputTokens + usage.OutputTokens,
		CacheReadTokens:     cachedTokens,
		CacheCreationTokens: cacheWriteTokens,
	}
	if usage.OutputTokensDetails != nil {
		result.ReasoningTokens = usage.OutputTokensDetails.ReasoningTokens
	}
	return result
}

func approximateInputTokens(items []inputItem) int64 {
	data, err := json.Marshal(items)
	if err != nil || len(data) == 0 {
		return 0
	}
	return int64((len(data) + compactionBytesPerToken - 1) / compactionBytesPerToken)
}
