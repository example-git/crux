package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go/option"
)

type EfficiencyPolicy struct {
	PromptCaching     bool   `json:"prompt_caching"`
	TTL               string `json:"ttl,omitempty" jsonschema:"enum=5m,enum=1h"`
	ContextManagement bool   `json:"context_management,omitempty"`
	RedactedThinking  bool   `json:"redacted_thinking,omitempty"`
}

func (p EfficiencyPolicy) Validate() error {
	if p.TTL != "" && p.TTL != "5m" && p.TTL != "1h" {
		return fmt.Errorf("Anthropic cache TTL must be 5m or 1h, got %q", p.TTL)
	}
	return nil
}

func WithEfficiencyPolicy(policy EfficiencyPolicy) Option {
	return func(o *options) { o.efficiency = policy }
}

type RequestCachePolicy struct {
	Enabled        bool
	TTL            string
	SkipCacheWrite bool
}

type requestCachePolicyKey struct{}

func CachePolicyFromContext(ctx context.Context) (RequestCachePolicy, bool) {
	policy, ok := ctx.Value(requestCachePolicyKey{}).(RequestCachePolicy)
	return policy, ok
}

func (a languageModel) cacheContext(ctx context.Context, options *ProviderOptions) (context.Context, error) {
	policy := RequestCachePolicy{Enabled: a.options.efficiency.PromptCaching, TTL: a.options.efficiency.TTL}
	if options != nil {
		if options.PromptCaching != nil {
			if *options.PromptCaching && !policy.Enabled {
				return nil, fmt.Errorf("prompt caching is not supported by this Anthropic provider")
			}
			policy.Enabled = *options.PromptCaching
		}
		if options.CacheTTL != "" {
			policy.TTL = options.CacheTTL
		}
		policy.SkipCacheWrite = options.SkipCacheWrite
	}
	if err := (EfficiencyPolicy{TTL: policy.TTL}).Validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, requestCachePolicyKey{}, policy), nil
}

type cacheHTTPClient struct {
	base option.HTTPClient
}

func (c cacheHTTPClient) Do(request *http.Request) (*http.Response, error) {
	policy, ok := CachePolicyFromContext(request.Context())
	if !ok || request.Body == nil || request.Body == http.NoBody {
		return c.base.Do(request)
	}
	body, err := io.ReadAll(request.Body)
	_ = request.Body.Close()
	if err != nil {
		return nil, err
	}
	body, err = ApplyRequestCachePolicy(body, policy)
	if err != nil {
		return nil, err
	}
	request = request.Clone(request.Context())
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	request.ContentLength = int64(len(body))
	return c.base.Do(request)
}

func ApplyRequestCachePolicy(body []byte, policy RequestCachePolicy) ([]byte, error) {
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode Anthropic cache request: %w", err)
	}
	if err := ApplyCachePolicy(document, policy); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func ApplyCachePolicy(document map[string]any, policy RequestCachePolicy) error {
	if err := (EfficiencyPolicy{TTL: policy.TTL}).Validate(); err != nil {
		return err
	}
	delete(document, "cache_control")
	messages, _ := document["messages"].([]any)
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		for _, block := range cacheBlocks(msg["content"]) {
			delete(block, "cache_control")
		}
	}
	for _, surface := range []string{"tools", "system"} {
		for _, block := range cacheBlocks(document[surface]) {
			if !policy.Enabled {
				delete(block, "cache_control")
				continue
			}
			if cache, ok := block["cache_control"].(map[string]any); ok && policy.TTL != "" {
				cache["ttl"] = policy.TTL
			}
		}
	}
	if !policy.Enabled {
		return nil
	}
	index := len(messages) - 1
	if policy.SkipCacheWrite {
		index--
	}
	if index >= 0 {
		msg, _ := messages[index].(map[string]any)
		if text, ok := msg["content"].(string); ok {
			msg["content"] = []any{map[string]any{"type": "text", "text": text}}
		}
		blocks := cacheBlocks(msg["content"])
		if len(blocks) > 0 {
			last := blocks[len(blocks)-1]
			kind, _ := last["type"].(string)
			if kind != "thinking" && kind != "redacted_thinking" {
				cache := map[string]any{"type": "ephemeral"}
				if policy.TTL != "" {
					cache["ttl"] = policy.TTL
				}
				last["cache_control"] = cache
			}
		}
	}
	return ValidateCacheMarkers(document)
}

func cacheBlocks(raw any) []map[string]any {
	values, _ := raw.([]any)
	blocks := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if block, ok := value.(map[string]any); ok {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func ValidateCacheMarkers(document map[string]any) error {
	blocks := append(cacheBlocks(document["tools"]), cacheBlocks(document["system"])...)
	for _, msg := range cacheBlocks(document["messages"]) {
		blocks = append(blocks, cacheBlocks(msg["content"])...)
	}
	count := 0
	shortTTL := false
	for _, block := range blocks {
		cache, exists := block["cache_control"]
		if !exists {
			continue
		}
		value, ok := cache.(map[string]any)
		if !ok || value["type"] != "ephemeral" {
			return fmt.Errorf("Anthropic cache_control must be an ephemeral object")
		}
		count++
		ttl, ok := value["ttl"].(string)
		if raw, exists := value["ttl"]; exists && (!ok || raw == "") {
			return fmt.Errorf("Anthropic cache TTL must be 5m or 1h")
		}
		if err := (EfficiencyPolicy{TTL: ttl}).Validate(); err != nil {
			return err
		}
		if ttl == "1h" && shortTTL {
			return fmt.Errorf("Anthropic one-hour cache markers must precede five-minute markers")
		}
		shortTTL = shortTTL || ttl != "1h"
	}
	if count > 4 {
		return fmt.Errorf("Anthropic request has %d cache markers; maximum is 4", count)
	}
	return nil
}
