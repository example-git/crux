package codex_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type identityNoHTTP func(*http.Request) (*http.Response, error)

func (f identityNoHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLegacyCodexIdentityKeepsExistingHeaderValues(t *testing.T) {
	t.Setenv("CODEX_VERSION", strings.Repeat("1", 129))
	t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", strings.Repeat("o", 257))
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	validate := func() error { return nil }
	provider, err := codex.NewProvider("wss://fixture.invalid/responses", func() string { return "synthetic-token" }, nil, nil, nil, registration.Operation, nil, registration.Images, validate)
	require.NoError(t, err, "captured declaration limits must not alter legacy construction")
	require.NotNil(t, provider)
	identity := useragent.NativeIdentity{UserAgent: useragent.Codex(), Originator: useragent.CodexOriginator(), Version: useragent.CodexVersion()}
	_, err = codex.NewProviderWithIdentity("wss://fixture.invalid/responses", func() string { return "synthetic-token" }, nil, nil, nil, registration.Operation, nil, registration.Images, validate, identity)
	require.ErrorContains(t, err, "native identity is invalid")
}

func TestLegacyCodexPreflightDoesNotDiscoverIdentity(t *testing.T) {
	t.Setenv("CODEX_VERSION", "")
	var requests atomic.Int32
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: identityNoHTTP(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected identity discovery")
	})}
	t.Cleanup(func() { http.DefaultClient = original })
	_, err := codex.NewProvider("", nil, nil, nil, nil, nil, nil, nil, nil)
	require.ErrorContains(t, err, "owner validator")
	require.Zero(t, requests.Load())
	// General OAuth context helpers preserve existing printable overrides too;
	// finite remote declaration validation is applied at the capture boundary.
	t.Setenv("CODEX_VERSION", strings.Repeat("1", 129))
	actual, err := useragent.CodexForContext(context.Background())
	require.NoError(t, err)
	require.Contains(t, actual, strings.Repeat("1", 129))
}
