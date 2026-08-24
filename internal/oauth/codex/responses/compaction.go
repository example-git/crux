package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/gorilla/websocket"
)

const (
	remoteCompactionSummary                 = "Conversation compacted by Codex"
	remoteCompactionV2MaxAttempts           = 3
	remoteCompactionV2RetainedMessageTokens = 64_000
	remoteCompactionV2TruncationTag         = "\n[... earlier retained message truncated ...]\n"
	compactionBytesPerToken                 = 4
)

func (g *languageModel) Compact(ctx context.Context, call fantasy.Call) (*CompactionResult, error) {
	started := time.Now()
	frame, _, err := g.prepareRequest(call)
	if err != nil {
		return nil, err
	}

	conversationID := call.Headers["x-session-id"]
	promptInput := cloneInputItems(frame.Input)
	frame.Input = append(frame.Input, inputItem{Type: "compaction_trigger"})
	frame.RequestKind = "compaction"
	compaction, usage, usageAvailable, err := g.compactRemotelyV2(ctx, frame, conversationID)
	if err != nil {
		g.clearCompactionChain(conversationID)
		slog.Debug("Codex compaction failed",
			"implementation", CompactionRemoteV2,
			"duration", time.Since(started),
			"error_type", fmt.Sprintf("%T", err),
		)
		return nil, err
	}

	history := &CompactedHistory{Items: buildRemoteV2CompactedHistory(promptInput, compaction)}
	result := &CompactionResult{
		History:           history,
		Summary:           remoteCompactionSummary,
		Usage:             usage,
		UsageAvailable:    usageAvailable,
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

func (g *languageModel) compactRemotelyV2(ctx context.Context, frame *requestFrame, conversationID string) (inputItem, fantasy.Usage, bool, error) {
	var lastErr error
	for attempt := 0; attempt < remoteCompactionV2MaxAttempts; attempt++ {
		result, retryable, err := g.compactRemoteV2Attempt(ctx, frame, conversationID)
		if err == nil {
			return result.compaction, result.usage, result.usageAvailable, nil
		}
		lastErr = err
		if !retryable || attempt == remoteCompactionV2MaxAttempts-1 {
			break
		}
	}
	return inputItem{}, fantasy.Usage{}, false, lastErr
}

func (g *languageModel) compactRemoteV2Attempt(ctx context.Context, frame *requestFrame, conversationID string) (remoteCompactionV2Attempt, bool, error) {
	events := g.client.stream(ctx, frame, g.provider, conversationID, "conversation")
	outputItems := 0
	compactionItems := 0
	var compaction inputItem
	for event, err := range events {
		if err != nil {
			return remoteCompactionV2Attempt{}, isRemoteCompactionV2Retryable(err), err
		}
		switch event.Type {
		case "response.output_item.done":
			outputItems++
			var item inputItem
			if err := json.Unmarshal(event.Item, &item); err != nil {
				return remoteCompactionV2Attempt{}, false, fmt.Errorf("codex: decode compaction output item: %w", err)
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
			err := codexProviderError(responseError, "codex remote compaction v2 did not complete")
			return remoteCompactionV2Attempt{}, isRemoteCompactionV2Retryable(err), err
		case "error":
			err := codexProviderError(event.Error, "codex remote compaction v2 failed")
			return remoteCompactionV2Attempt{}, isRemoteCompactionV2Retryable(err), err
		case "response.completed":
			if event.Response == nil {
				return remoteCompactionV2Attempt{}, false, fmt.Errorf("codex: compaction response missing payload")
			}
			if event.Response.Error != nil {
				return remoteCompactionV2Attempt{}, false, codexProviderError(event.Response.Error, "codex remote compaction v2 failed")
			}
			if event.Response.ID == "" {
				return remoteCompactionV2Attempt{}, false, fmt.Errorf("codex: remote compaction v2 completed without a response id")
			}
			if compactionItems != 1 || compaction.EncryptedContent == "" {
				return remoteCompactionV2Attempt{}, false, fmt.Errorf("codex: remote compaction v2 expected exactly one encrypted compaction output item, got %d from %d output items", compactionItems, outputItems)
			}
			return remoteCompactionV2Attempt{
				compaction:     compaction,
				usage:          usageFromWire(event.Response.Usage),
				usageAvailable: event.Response.Usage != nil,
			}, false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return remoteCompactionV2Attempt{}, false, err
	}
	return remoteCompactionV2Attempt{}, true, fmt.Errorf("codex: remote compaction v2 stream closed before response.completed")
}

func isRemoteCompactionV2Retryable(err error) bool {
	if fantasy.IsTransportError(err) {
		return true
	}
	var providerError *fantasy.ProviderError
	if errors.As(err, &providerError) && providerError.IsRetryable() {
		return true
	}
	return websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
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

func buildRemoteV2CompactedHistory(input []inputItem, compaction inputItem) []inputItem {
	retained := make([]inputItem, 0, len(input))
	for _, item := range input {
		if item.Type == "message" && (item.Role == "user" || item.Role == "developer" || item.Role == "system") {
			retained = append(retained, item)
		}
	}
	retained = truncateRemoteV2RetainedMessages(retained, remoteCompactionV2RetainedMessageTokens)
	return append(retained, compaction)
}

func truncateRemoteV2RetainedMessages(items []inputItem, maxTokens int) []inputItem {
	remaining := maxTokens
	retainedReversed := make([]inputItem, 0, len(items))
	for i := len(items) - 1; i >= 0 && remaining > 0; i-- {
		item := items[i]
		tokens := messageItemTextTokens(item)
		if tokens <= remaining {
			retainedReversed = append(retainedReversed, item)
			remaining -= tokens
			continue
		}
		if truncated, ok := truncateMessageItem(item, remaining); ok {
			retainedReversed = append(retainedReversed, truncated)
		}
		break
	}
	for left, right := 0, len(retainedReversed)-1; left < right; left, right = left+1, right-1 {
		retainedReversed[left], retainedReversed[right] = retainedReversed[right], retainedReversed[left]
	}
	return retainedReversed
}

func messageItemTextTokens(item inputItem) int {
	tokens := 0
	for _, content := range item.Content {
		if content.Text != "" {
			tokens += approximateTextTokens(content.Text)
		}
	}
	return max(tokens, 1)
}

func truncateMessageItem(item inputItem, maxTokens int) (inputItem, bool) {
	if maxTokens <= 0 {
		return inputItem{}, false
	}
	parts := make([]string, 0, len(item.Content))
	for _, content := range item.Content {
		if content.Text != "" {
			parts = append(parts, content.Text)
		}
	}
	text := utf8Middle(strings.Join(parts, "\n"), maxTokens*compactionBytesPerToken, remoteCompactionV2TruncationTag)
	if text == "" {
		return inputItem{}, false
	}
	item.Content = []messageContent{{Type: "input_text", Text: text}}
	return item, true
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

func approximateTextTokens(value string) int {
	if value == "" {
		return 0
	}
	return (len(value) + compactionBytesPerToken - 1) / compactionBytesPerToken
}

func utf8Middle(value string, maxBytes int, marker string) string {
	if len(value) <= maxBytes {
		return value
	}
	available := maxBytes - len(marker)
	if available <= 0 {
		return ""
	}
	headBytes := available / 2
	tailBytes := available - headBytes
	headEnd := headBytes
	for headEnd > 0 && !utf8.RuneStart(value[headEnd]) {
		headEnd--
	}
	tailStart := len(value) - tailBytes
	for tailStart < len(value) && !utf8.RuneStart(value[tailStart]) {
		tailStart++
	}
	return value[:headEnd] + marker + value[tailStart:]
}

func approximateInputTokens(items []inputItem) int64 {
	data, err := json.Marshal(items)
	if err != nil || len(data) == 0 {
		return 0
	}
	return int64((len(data) + compactionBytesPerToken - 1) / compactionBytesPerToken)
}
