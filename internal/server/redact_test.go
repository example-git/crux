package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestJSONResponsesScrubKnownSecrets(t *testing.T) {
	secret := "api-response-secret-value"
	redact.Register(secret)
	recorder := httptest.NewRecorder()
	jsonEncode(recorder, map[string]any{"nested": map[string]string{"ordinary": secret}})
	require.NotContains(t, recorder.Body.String(), secret)
	require.Contains(t, recorder.Body.String(), redact.Replacement)
}

func TestJSONErrorsScrubKnownSecrets(t *testing.T) {
	secret := "api-error-secret-value"
	redact.Register(secret)
	recorder := httptest.NewRecorder()
	jsonError(recorder, 500, "failed "+secret)
	require.NotContains(t, recorder.Body.String(), secret)
	require.Contains(t, recorder.Body.String(), redact.Replacement)
}

func TestJSONResponsesRemainValidWhenSecretMatchesJSONSyntax(t *testing.T) {
	escapedSecret := `quoted-"-secret`
	redact.Register(":true", escapedSecret)
	recorder := httptest.NewRecorder()
	jsonEncode(recorder, map[string]any{"flag": true, "message": "value :true", "escaped": escapedSecret})

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded))
	require.Equal(t, true, decoded["flag"])
	require.Equal(t, "value "+redact.Replacement, decoded["message"])
	require.Equal(t, redact.Replacement, decoded["escaped"])
}
