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
