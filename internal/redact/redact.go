package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const Replacement = "[REDACTED]"

var registry struct {
	mu       sync.Mutex
	values   map[string]struct{}
	snapshot atomic.Pointer[[]string]
}

func Register(values ...string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.values == nil {
		registry.values = make(map[string]struct{})
	}
	changed := false
	for _, value := range values {
		if value == "" || value == Replacement {
			continue
		}
		if _, exists := registry.values[value]; exists {
			continue
		}
		registry.values[value] = struct{}{}
		changed = true
	}
	if !changed {
		return
	}
	valuesSnapshot := make([]string, 0, len(registry.values))
	for value := range registry.values {
		valuesSnapshot = append(valuesSnapshot, value)
	}
	sort.Slice(valuesSnapshot, func(i, j int) bool {
		if len(valuesSnapshot[i]) == len(valuesSnapshot[j]) {
			return valuesSnapshot[i] < valuesSnapshot[j]
		}
		return len(valuesSnapshot[i]) > len(valuesSnapshot[j])
	})
	registry.snapshot.Store(&valuesSnapshot)
}

func String(value string) string {
	snapshot := registry.snapshot.Load()
	if snapshot == nil || value == "" {
		return value
	}
	for _, secret := range *snapshot {
		value = strings.ReplaceAll(value, secret, Replacement)
	}
	return value
}

func Bytes(value []byte) []byte {
	snapshot := registry.snapshot.Load()
	if snapshot == nil || len(value) == 0 {
		return value
	}
	result := value
	copied := false
	for _, secret := range *snapshot {
		if !bytes.Contains(result, []byte(secret)) {
			continue
		}
		if !copied {
			result = bytes.Clone(result)
			copied = true
		}
		result = bytes.ReplaceAll(result, []byte(secret), []byte(Replacement))
	}
	return result
}

func JSON(value []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode JSON for redaction: %w", err)
	}
	decoded = redactJSONValue(decoded)
	result, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("encode redacted JSON: %w", err)
	}
	return result, nil
}

func redactJSONValue(value any) any {
	switch typed := value.(type) {
	case string:
		return String(typed)
	case map[string]any:
		for key, item := range typed {
			typed[key] = redactJSONValue(item)
		}
	case []any:
		for index, item := range typed {
			typed[index] = redactJSONValue(item)
		}
	}
	return value
}
