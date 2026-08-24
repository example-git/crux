package log

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestKnownSecretsAreScrubbedFromAllTrafficDiagnosticFields(t *testing.T) {
	secret := "traffic-secret-value"
	redact.Register(secret)
	headers := http.Header(formatHeaders(http.Header{"X-Trace": []string{"prefix " + secret}, "Authorization": []string{"Bearer anything"}}))
	require.Equal(t, "prefix "+redact.Replacement, headers.Get("X-Trace"))
	require.Equal(t, redact.Replacement, headers.Get("Authorization"))

	parsed, err := url.Parse("https://example.test/path/" + secret + "?ordinary=" + url.QueryEscape(secret))
	require.NoError(t, err)
	require.NotContains(t, sanitizeURL(parsed), secret)

	for _, contentType := range []string{"text/plain", "application/json", "application/x-www-form-urlencoded"} {
		body, _ := encodeNetworkBody([]byte("prefix "+secret+" suffix"), contentType)
		require.NotContains(t, body, secret)
		require.Contains(t, body, redact.Replacement)
	}

	binary := append([]byte{0xff, 0x00}, []byte(secret)...)
	body, encoding := encodeNetworkBody(binary, "application/octet-stream")
	require.Equal(t, "base64", encoding)
	decoded, err := base64.StdEncoding.DecodeString(body)
	require.NoError(t, err)
	require.NotContains(t, string(decoded), secret)

	truncated := []byte(secret + strings.Repeat("x", trafficMaxPayloadBytes))
	body, encoding = encodeNetworkBody(truncated, "text/plain")
	require.Contains(t, encoding, "truncated")
	require.NotContains(t, body, secret)
}
