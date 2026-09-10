package redact

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShortSecretsPreserveProtocolTokens(t *testing.T) {
	// Secret fingerprints intentionally survive for the process lifetime.
	// Isolate low-entropy fixtures from other tests' registered values.
	if os.Getenv("CRUX_TEST_SHORT_SECRETS") != "1" {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestShortSecretsPreserveProtocolTokens$")
		command.Env = append(os.Environ(), "CRUX_TEST_SHORT_SECRETS=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	Register("1", "0", "abc", "ordinary-long-credential")
	for _, value := range []string{
		strings.Repeat("abcdef0123456789", 4),
		"018abc10-0000-1111-aaaa-bbbbbbbbbbbb",
		"1.0.0", "gpt-1", "client_1", "abc-provider",
	} {
		require.Equal(t, value, String(value))
		encoded, err := json.Marshal(map[string]string{"identity": value})
		require.NoError(t, err)
		redacted, err := JSON(encoded)
		require.NoError(t, err)
		var decoded map[string]string
		require.NoError(t, json.Unmarshal(redacted, &decoded))
		require.Equal(t, value, decoded["identity"])
	}
	for _, value := range []string{"1", "0", "abc"} {
		require.Equal(t, Replacement, String(value))
		require.Equal(t, "cookie="+Replacement+"; next=value", String("cookie="+value+"; next=value"))
		require.Equal(t, "Bearer "+Replacement, String("Bearer "+value))
		encoded, err := json.Marshal(map[string]string{"cookie": value})
		require.NoError(t, err)
		redacted, err := JSON(encoded)
		require.NoError(t, err)
		require.JSONEq(t, `{"cookie":"[REDACTED]"}`, string(redacted))
	}
	// Longer credentials continue to redact embedded occurrences.
	Register("longcredential")
	require.Equal(t, "prefix"+Replacement+"suffix", String("prefixlongcredentialsuffix"))
}
