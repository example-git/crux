package providertransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/redact"
)

const (
	maxMappedErrorBodyBytes = 1 << 20
	MaxMappedErrorTextRunes = 4096
)

type providerErrorFieldSource interface {
	ProviderErrorField(string) (string, bool)
}

func MapError(mappings []manifest.ErrorMapping, err error) error {
	mapping, providerErr, body, fields, ok := matchingErrorMapping(mappings, err)
	if !ok {
		return err
	}
	applyErrorMapping(mapping, providerErr, body, fields)
	return err
}

func ErrorMappingRetryable(mappings []manifest.ErrorMapping, err error) bool {
	mapping, _, _, _, ok := matchingErrorMapping(mappings, err)
	return ok && mapping.Retryable
}

func RetryOperationError(policy manifest.RetryPolicy, mappings []manifest.ErrorMapping, err error, emitted bool) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || IsOwnerValidationError(err) {
		return false
	}
	if policy.ReplayRequirement == "never" || policy.ReplayRequirement == "before-first-event" && emitted {
		return false
	}
	if policy.UnexpectedEOF && errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if policy.TransportErrors && fantasy.IsTransportError(err) {
		return true
	}
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		if containsInt(policy.Statuses, providerErr.StatusCode) {
			return true
		}
		if len(policy.Codes) > 0 && containsCode(policy.Codes, providerErr.ResponseBody) {
			return true
		}
	}
	return ErrorMappingRetryable(mappings, err)
}

func RetryAfterHeader(err error) string {
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return ""
	}
	for name, value := range providerErr.ResponseHeaders {
		if strings.EqualFold(name, "Retry-After") {
			return value
		}
	}
	return ""
}

func matchingErrorMapping(mappings []manifest.ErrorMapping, err error) (manifest.ErrorMapping, *fantasy.ProviderError, any, providerErrorFieldSource, bool) {
	if err == nil || len(mappings) == 0 {
		return manifest.ErrorMapping{}, nil, nil, nil, false
	}
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return manifest.ErrorMapping{}, nil, nil, nil, false
	}
	var fields providerErrorFieldSource
	_ = errors.As(err, &fields)
	var body any
	if len(providerErr.ResponseBody) > 0 && len(providerErr.ResponseBody) <= maxMappedErrorBodyBytes {
		decoder := json.NewDecoder(strings.NewReader(string(providerErr.ResponseBody)))
		decoder.UseNumber()
		_ = decoder.Decode(&body)
	}
	for _, mapping := range mappings {
		if errorMappingMatches(mapping, providerErr.StatusCode, body, fields) {
			return mapping, providerErr, body, fields, true
		}
	}
	return manifest.ErrorMapping{}, providerErr, body, fields, false
}

func errorMappingMatches(mapping manifest.ErrorMapping, status int, body any, fields providerErrorFieldSource) bool {
	if len(mapping.Statuses) > 0 && !containsInt(mapping.Statuses, status) {
		return false
	}
	if len(mapping.Codes) == 0 {
		return true
	}
	code, ok := providerErrorString(fields, body, mapping.CodePointer)
	return ok && containsString(mapping.Codes, code)
}

func applyErrorMapping(mapping manifest.ErrorMapping, providerErr *fantasy.ProviderError, body any, fields providerErrorFieldSource) {
	providerErr.Class = fantasy.ProviderErrorClass(mapping.Class)
	providerErr.AuthError = mapping.Class == "authentication"
	providerErr.TransientError = mapping.Retryable
	providerErr.UnlimitedRetry = fantasy.UnlimitedRetryNone
	contextOverflow := mapping.Class == "context-overflow" || mapping.ContextOverflow
	providerErr.ContextTooLargeErr = contextOverflow
	if !contextOverflow {
		providerErr.ContextUsedTokens = 0
		providerErr.ContextMaxTokens = 0
	}
	defaultTitle := "Provider error"
	switch mapping.Class {
	case "authentication":
		defaultTitle = "Authentication required"
	case "authorization":
		defaultTitle = "Authorization denied"
	case "rate-limit":
		defaultTitle = "Rate limit reached"
	case "capacity":
		defaultTitle = "Provider capacity unavailable"
	case "context-overflow":
		defaultTitle = "Context limit exceeded"
	case "invalid-request":
		defaultTitle = "Invalid provider request"
	case "content-filter":
		defaultTitle = "Content blocked"
	case "server":
		defaultTitle = "Provider server error"
	case "transport":
		defaultTitle = "Provider transport error"
	}
	providerErr.Title = defaultTitle
	if mapping.Title != "" {
		providerErr.Title = mapping.Title
	}
	if message, ok := providerErrorString(fields, body, mapping.MessagePointer); ok && message != "" {
		providerErr.Message = message
	}
	providerErr.Title = boundedErrorText(providerErr.Title)
	providerErr.Message = boundedErrorText(providerErr.Message)
}

func providerErrorString(fields providerErrorFieldSource, body any, pointer string) (string, bool) {
	if fields != nil {
		if value, ok := fields.ProviderErrorField(pointer); ok {
			return value, true
		}
	}
	return jsonPointerString(body, pointer)
}

func jsonPointerString(value any, pointer string) (string, bool) {
	if pointer == "" {
		return "", false
	}
	current := value
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[token]
			if !ok {
				return "", false
			}
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) {
				return "", false
			}
			current = node[index]
		default:
			return "", false
		}
	}
	switch value := current.(type) {
	case string:
		return value, true
	case json.Number:
		return value.String(), true
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), true
	default:
		return "", false
	}
}

func boundedErrorText(value string) string {
	value = redact.String(value)
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	runes := []rune(value)
	if len(runes) > MaxMappedErrorTextRunes {
		runes = runes[:MaxMappedErrorTextRunes]
	}
	return string(runes)
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
