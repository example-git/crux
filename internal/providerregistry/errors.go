package providerregistry

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

// MapError applies the first registered declarative mapping whose status and
// code predicates both match. It mutates the provider error already carried in
// the error chain so Fantasy's retry and authentication handling observes the
// normalized flags without replacing wrapper context.
func (r Registration) MapError(err error) error {
	if err == nil || len(r.Errors) == 0 {
		return err
	}
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return err
	}

	var body any
	if len(providerErr.ResponseBody) > 0 {
		data := providerErr.ResponseBody
		if len(data) > maxMappedErrorBodyBytes {
			data = data[:maxMappedErrorBodyBytes]
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.UseNumber()
		_ = decoder.Decode(&body)
	}
	for _, mapping := range r.Errors {
		if !errorMappingMatches(mapping, providerErr.StatusCode, body) {
			continue
		}
		applyErrorMapping(mapping, providerErr, body)
		break
	}
	return err
}

func errorMappingMatches(mapping manifest.ErrorMapping, status int, body any) bool {
	if len(mapping.Statuses) > 0 && !containsInt(mapping.Statuses, status) {
		return false
	}
	if len(mapping.Codes) == 0 {
		return true
	}
	code, ok := jsonPointerString(body, mapping.CodePointer)
	return ok && containsString(mapping.Codes, code)
}

func applyErrorMapping(mapping manifest.ErrorMapping, providerErr *fantasy.ProviderError, body any) {
	if mapping.Title != "" {
		providerErr.Title = boundedErrorText(mapping.Title)
	}
	if message, ok := jsonPointerString(body, mapping.MessagePointer); ok && message != "" {
		providerErr.Message = boundedErrorText(message)
	}
	if mapping.Class == "authentication" {
		providerErr.AuthError = true
	}
	if mapping.Retryable {
		providerErr.TransientError = true
	}
	if mapping.Class == "context-overflow" || mapping.ContextOverflow {
		providerErr.ContextTooLargeErr = true
	}
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

const (
	maxMappedErrorBodyBytes = 1 << 20
	maxMappedErrorTextRunes = 4096
)

func boundedErrorText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	runes := []rune(value)
	if len(runes) > maxMappedErrorTextRunes {
		runes = runes[:maxMappedErrorTextRunes]
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
