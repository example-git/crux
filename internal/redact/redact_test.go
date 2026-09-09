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

func embeddedJSONOutput(t *testing.T, document string) string {
	t.Helper()
	outer, err := json.Marshal(map[string]string{"output": document})
	require.NoError(t, err)
	redacted, err := JSON(outer)
	require.NoError(t, err)
	var result map[string]string
	require.NoError(t, json.Unmarshal(redacted, &result))
	return result["output"]
}

func TestJSONEmbeddedRedactsSecretKeysAndEveryDuplicateOccurrence(t *testing.T) {
	const keySecret = "embedded-key-secret"
	const valueSecret = "embedded-duplicate-secret"
	const quotedKey = "embedded-key-\"quoted\"-secret"
	Register(keySecret, valueSecret, quotedKey)
	cases := []struct{ name, document, expected string }{
		{"key", `{"embedded-key-secret":1}`, `{"[REDACTED]":1}`},
		{"duplicate-first", `{"x":"embedded-duplicate-secret","x":"safe"}`, `{"x":"[REDACTED]","x":"safe"}`},
		{"duplicate-all", `{"x":"embedded-duplicate-secret","x":"embedded-duplicate-secret"}`, `{"x":"[REDACTED]","x":"[REDACTED]"}`},
		{"duplicate-key", `{"embedded-key-secret":"one","embedded-key-secret":"two"}`, `{"[REDACTED]":"one","[REDACTED]":"two"}`},
		{"escaped-key", `{"embedded-key-\"quoted\"-secret":true}`, `{"[REDACTED]":true}`},
		{"escaped-duplicate", `{"x":"embedded-duplicate-\u0073ecret","x":"safe"}`, `{"x":"[REDACTED]","x":"safe"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := embeddedJSONOutput(t, tc.document)
			require.True(t, json.Valid([]byte(actual)))
			require.Equal(t, tc.expected, actual)
			require.NotContains(t, actual, keySecret)
			require.NotContains(t, actual, valueSecret)
		})
	}
}

func TestJSONEmbeddedKeepsWhitespaceWrappedAndNestedRegisteredDocumentsOpaque(t *testing.T) {
	const object = `{"opaque-nested-object":"registered-object-secret-only"}`
	const array = `["registered-array-secret-only"]`
	Register(object, array, ":true")
	cases := []struct{ name, document, expected string }{
		{"whitespace-object", " \t" + object + "\n", " \t" + Replacement + "\n"},
		{"whitespace-array", "\n" + array + "  ", "\n" + Replacement + "  "},
		{"object-in-array", "[" + object + `,{"safe":true}]`, `["[REDACTED]",{"safe":true}]`},
		{"array-in-object", `{"payload":` + array + `,"safe":true}`, `{"payload":"[REDACTED]","safe":true}`},
		{"duplicate-opaque", `{"x":` + object + `,"x":` + array + `}`, `{"x":"[REDACTED]","x":"[REDACTED]"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := embeddedJSONOutput(t, tc.document)
			require.Equal(t, tc.expected, actual)
			require.NotContains(t, actual, "registered-object-secret-only")
			require.NotContains(t, actual, "registered-array-secret-only")
			if tc.name != "whitespace-object" && tc.name != "whitespace-array" {
				require.True(t, json.Valid([]byte(actual)))
			}
		})
	}
}

func TestJSONEmbeddedPreservesUnchangedDuplicatesAndStructuralScalars(t *testing.T) {
	const number = "9876543210123456789"
	Register(":true", number)
	const document = " \n{ \"x\" : [9876543210123456789, true, false, null, 1.00e+03], \"x\" : {\"safe\":true} }\t"
	require.Equal(t, document, embeddedJSONOutput(t, document))
	// A registered numeric string remains redacted when used as string data.
	require.Equal(t, `["[REDACTED]",9876543210123456789]`, embeddedJSONOutput(t, `["9876543210123456789",9876543210123456789]`))
}

func TestJSONEmbeddedPreservesRegularTextRedaction(t *testing.T) {
	const secret = "embedded-regular-text-secret"
	Register(secret)
	for _, text := range []string{
		"prefix " + secret + " suffix",
		`prefix {"token":"` + secret + `"} suffix`,
		`{"incomplete":"` + secret,
	} {
		require.Equal(t, String(text), embeddedJSONOutput(t, text))
	}
}
