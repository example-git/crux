package redact

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONPreservesEmbeddedDocumentsAndRedactsTheirValues(t *testing.T) {
	const secret = "nested-document-secret-\"quoted\""
	Register(":true", secret)
	inner, err := json.Marshal(map[string]any{"success": true, "token": secret, "literal": ":true", "requested": 1})
	require.NoError(t, err)
	outer, err := json.Marshal(map[string]any{"output": string(inner), "literal": secret})
	require.NoError(t, err)
	redacted, err := JSON(outer)
	require.NoError(t, err)
	var result map[string]string
	require.NoError(t, json.Unmarshal(redacted, &result))
	require.Equal(t, Replacement, result["literal"])
	var document map[string]any
	require.NoError(t, json.Unmarshal([]byte(result["output"]), &document))
	require.Equal(t, true, document["success"])
	require.Equal(t, float64(1), document["requested"])
	require.Equal(t, Replacement, document["token"])
	require.Equal(t, Replacement, document["literal"])
}

func TestJSONKeepsWholeRegisteredDocumentsOpaque(t *testing.T) {
	const secret = `{"opaque-document":"whole-document-secret"}`
	Register(secret)
	outer, err := json.Marshal(map[string]string{"output": secret})
	require.NoError(t, err)
	redacted, err := JSON(outer)
	require.NoError(t, err)
	var result map[string]string
	require.NoError(t, json.Unmarshal(redacted, &result))
	require.Equal(t, Replacement, result["output"])
}

func TestJSONDecodesEscapedEmbeddedValuesAndPreservesUnchangedText(t *testing.T) {
	const secret = "escaped-inner-value-\"private\""
	const unchanged = "  { \"unchanged\" : [1, 2] }\n"
	Register(secret)
	inner, err := json.Marshal([]string{secret})
	require.NoError(t, err)
	outer, err := json.Marshal(map[string]string{"output": string(inner), "unchanged": unchanged})
	require.NoError(t, err)
	redacted, err := JSON(outer)
	require.NoError(t, err)
	var result map[string]string
	require.NoError(t, json.Unmarshal(redacted, &result))
	var document []string
	require.NoError(t, json.Unmarshal([]byte(result["output"]), &document))
	require.Equal(t, []string{Replacement}, document)
	require.Equal(t, unchanged, result["unchanged"])
}

func TestRegisterRedactsExactValuesLongestFirst(t *testing.T) {
	Register("overlap", "overlap-long", "overlap")
	require.Equal(t, "[REDACTED] [REDACTED]", String("overlap-long overlap"))
	require.Equal(t, []byte("before [REDACTED] after"), Bytes([]byte("before overlap-long after")))
}

func TestRegisterJSONValuesRecursesWithoutRegisteringKeys(t *testing.T) {
	mapSecret := "structured-map-secret-value"
	arraySecret := "structured-array-secret-value"
	rawSecret := "structured-raw-secret-value"
	publicKey := "structured-public-key"
	RegisterJSONValue(map[string]any{
		publicKey: map[string]any{"nested": []any{mapSecret, map[string]string{"value": arraySecret}}},
	})
	RegisterJSONBytes([]byte(`{"opaque":{"token":"` + rawSecret + `"}}`))

	for _, secret := range []string{mapSecret, arraySecret, rawSecret} {
		require.Equal(t, Replacement, String(secret))
	}
	require.Equal(t, publicKey, String(publicKey))
}

func TestRegisterIsConcurrent(t *testing.T) {
	var wait sync.WaitGroup
	for index := range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			Register(fmt.Sprintf("concurrent-secret-%d", index))
		}()
	}
	wait.Wait()
	for index := range 64 {
		require.NotContains(t, String(fmt.Sprintf("concurrent-secret-%d", index)), "concurrent-secret")
	}
}
