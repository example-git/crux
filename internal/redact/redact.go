package redact

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/maphash"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const Replacement = "[REDACTED]"

// The registry deliberately retains keyed fingerprints rather than plaintext
// values. Historical fingerprints stay available for delayed log messages after
// their workspace/credential is gone. This is reference release, not physical
// zeroization of strings already copied elsewhere by the runtime or callers.
type secretDigest struct {
	length int
	digest [sha256.Size]byte
}

type secretPrefix struct {
	length int
	hash   uint64
}

type redactionSnapshot struct {
	prefixLengths []int
	index         map[secretPrefix][]secretDigest
}

var fingerprintKey = func() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("cannot initialize private redaction fingerprints")
	}
	return key
}()
var prefixSeed = maphash.MakeSeed()

var registry struct {
	mu       sync.Mutex
	values   map[secretDigest]secretPrefix
	snapshot atomic.Pointer[redactionSnapshot]
}

func fingerprint(value string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, fingerprintKey[:])
	_, _ = mac.Write([]byte(value))
	var result [sha256.Size]byte
	mac.Sum(result[:0])
	return result
}

func Register(values ...string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.values == nil {
		registry.values = make(map[secretDigest]secretPrefix)
	}
	changed := false
	for _, value := range values {
		if value == "" || value == Replacement {
			continue
		}
		digest := secretDigest{length: len(value), digest: fingerprint(value)}
		if _, exists := registry.values[digest]; exists {
			continue
		}
		length := min(len(value), 16)
		registry.values[digest] = secretPrefix{length: length, hash: maphash.String(prefixSeed, value[:length])}
		changed = true
	}
	if !changed {
		return
	}
	snapshot := &redactionSnapshot{index: make(map[secretPrefix][]secretDigest)}
	lengths := make(map[int]bool)
	for digest, prefix := range registry.values {
		snapshot.index[prefix] = append(snapshot.index[prefix], digest)
		lengths[prefix.length] = true
	}
	for length := range lengths {
		snapshot.prefixLengths = append(snapshot.prefixLengths, length)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(snapshot.prefixLengths)))
	for _, candidates := range snapshot.index {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].length != candidates[j].length {
				return candidates[i].length > candidates[j].length
			}
			return bytes.Compare(candidates[i].digest[:], candidates[j].digest[:]) < 0
		})
	}
	registry.snapshot.Store(snapshot)
}

func RegisterJSONValue(value any) {
	var values []string
	collectJSONStrings(value, &values)
	Register(values...)
}

func RegisterJSONBytes(value []byte) {
	if len(value) == 0 {
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return
	}
	RegisterJSONValue(decoded)
}

func collectJSONStrings(value any, values *[]string) {
	switch typed := value.(type) {
	case string:
		*values = append(*values, typed)
	case map[string]any:
		for _, item := range typed {
			collectJSONStrings(item, values)
		}
	case map[string]string:
		for _, item := range typed {
			collectJSONStrings(item, values)
		}
	case []any:
		for _, item := range typed {
			collectJSONStrings(item, values)
		}
	case []string:
		*values = append(*values, typed...)
	}
}

func String(value string) string {
	snapshot := registry.snapshot.Load()
	if snapshot == nil || value == "" {
		return value
	}
	type span struct{ start, end int }
	var spans []span
	for start := 0; start < len(value); start++ {
		end := start
		for _, length := range snapshot.prefixLengths {
			if length > len(value)-start {
				continue
			}
			prefix := secretPrefix{length: length, hash: maphash.String(prefixSeed, value[start:start+length])}
			previousLength := 0
			var digest [sha256.Size]byte
			for _, candidate := range snapshot.index[prefix] {
				if candidate.length > len(value)-start || candidate.length <= end-start {
					continue
				}
				if previousLength != candidate.length {
					digest = fingerprint(value[start : start+candidate.length])
					previousLength = candidate.length
				}
				if hmac.Equal(digest[:], candidate.digest[:]) {
					end = start + candidate.length
					break
				}
			}
		}
		if end == start {
			continue
		}
		// Merge every overlap in the original input. A shorter earlier match
		// must not expose the suffix of a longer overlapping secret.
		if len(spans) > 0 && start < spans[len(spans)-1].end {
			spans[len(spans)-1].end = max(spans[len(spans)-1].end, end)
		} else {
			spans = append(spans, span{start: start, end: end})
		}
	}
	if len(spans) == 0 {
		return value
	}
	var result strings.Builder
	last := 0
	for _, span := range spans {
		result.WriteString(value[last:span.start])
		result.WriteString(Replacement)
		last = span.end
	}
	result.WriteString(value[last:])
	return result.String()
}

func Bytes(value []byte) []byte {
	if len(value) == 0 {
		return value
	}
	original := string(value)
	result := String(original)
	if result == original {
		return value
	}
	return []byte(result)
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
		return redactJSONString(typed)
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

// registeredValue checks one complete value against the immutable fingerprint
// index. Embedded containers need an exact whole-secret check, not repeated
// substring scans of all their descendants.
func registeredValue(value string) bool {
	snapshot := registry.snapshot.Load()
	if snapshot == nil || value == "" {
		return false
	}
	prefixLength := min(len(value), 16)
	prefix := secretPrefix{length: prefixLength, hash: maphash.String(prefixSeed, value[:prefixLength])}
	var digest [sha256.Size]byte
	hashed := false
	for _, candidate := range snapshot.index[prefix] {
		if candidate.length != len(value) {
			continue
		}
		if !hashed {
			digest = fingerprint(value)
			hashed = true
		}
		if hmac.Equal(digest[:], candidate.digest[:]) {
			return true
		}
	}
	return false
}

// redactJSONString treats valid object/array documents inside strings as JSON,
// while ordinary text retains substring redaction. A registered whole document
// stays opaque, including when its caller added surrounding whitespace.
func redactJSONString(value string) string {
	redacted := String(value)
	if redacted == Replacement {
		return redacted
	}
	trimmed := strings.TrimSpace(value)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') || !json.Valid([]byte(trimmed)) {
		return redacted
	}
	if registeredValue(trimmed) {
		return redacted
	}
	document, changed, err := redactEmbeddedJSON([]byte(trimmed))
	if err != nil {
		return redacted
	}
	if !changed {
		return value
	}
	return string(document)
}

// redactEmbeddedJSON checks every occurrence, including duplicate object keys.
// Decoding into a map would discard earlier duplicate values and could restore
// their secrets when returning the original text. Rebuild only changed nodes;
// untouched documents retain their formatting and numeric representations.
func redactEmbeddedJSON(raw []byte) ([]byte, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false, fmt.Errorf("empty embedded JSON value")
	}
	switch raw[0] {
	case '"':
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, false, err
		}
		redacted := redactJSONString(value)
		if redacted == value {
			return raw, false, nil
		}
		encoded, err := json.Marshal(redacted)
		return encoded, true, err
	case '{', '[':
		// A complete registered JSON object/array is itself a secret, even
		// if none of its individual values was independently registered.
		if registeredValue(string(raw)) {
			encoded, err := json.Marshal(Replacement)
			return encoded, true, err
		}
	default:
		// Keep numbers, booleans and null typed, just as outer JSON does.
		return raw, false, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if _, err := decoder.Token(); err != nil {
		return nil, false, err
	}
	object := raw[0] == '{'
	var result bytes.Buffer
	result.WriteByte(raw[0])
	changed, first := false, true
	for decoder.More() {
		if !first {
			result.WriteByte(',')
		}
		first = false
		if object {
			token, err := decoder.Token()
			if err != nil {
				return nil, false, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, false, fmt.Errorf("invalid embedded JSON object key")
			}
			redacted := String(key)
			changed = changed || redacted != key
			encoded, err := json.Marshal(redacted)
			if err != nil {
				return nil, false, err
			}
			result.Write(encoded)
			result.WriteByte(':')
		}
		var child json.RawMessage
		if err := decoder.Decode(&child); err != nil {
			return nil, false, err
		}
		encoded, childChanged, err := redactEmbeddedJSON(child)
		if err != nil {
			return nil, false, err
		}
		changed = changed || childChanged
		result.Write(encoded)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false, err
	}
	if !changed {
		return raw, false, nil
	}
	if object {
		result.WriteByte('}')
	} else {
		result.WriteByte(']')
	}
	return result.Bytes(), true, nil
}
