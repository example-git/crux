package providertransport

import (
	"net/http"
	"net/url"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestEndpointRedirectPolicyCapturesDeclarationAndComposesClient(t *testing.T) {
	callbackCalls := 0
	endpoint := manifest.Endpoint{BaseURL: "https://api.example.invalid/base", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"api.example.invalid", "cdn.example.invalid"}, Override: "allowed-hosts", FollowRedirects: true}
	base := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { callbackCalls++; return nil }}
	client := EndpointHTTPClient(base, endpoint)
	endpoint.BaseURL = "https://elsewhere.invalid"
	endpoint.AllowedHosts[0] = "elsewhere.invalid"
	endpoint.AllowedSchemes[0] = "http"
	request, err := http.NewRequest(http.MethodPost, "https://api.example.invalid/next?retained=yes", nil)
	require.NoError(t, err)
	require.NoError(t, client.CheckRedirect(request, []*http.Request{request}))
	request, err = http.NewRequest(http.MethodPost, "https://cdn.example.invalid:8443/other", nil)
	require.NoError(t, err)
	require.NoError(t, client.CheckRedirect(request, []*http.Request{request}), "allowed-hosts retains its explicitly wider origin scope")
	// Use an accepting callback for destination validation; a refusal from
	// the supplied callback remains authoritative without any dispatch.
	strict := EndpointHTTPClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}, manifest.Endpoint{BaseURL: "https://api.example.invalid", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"api.example.invalid"}, Override: "same-origin", FollowRedirects: true})
	for _, target := range []string{"http://api.example.invalid/next", "https://elsewhere.invalid/next", "https://user:pass@api.example.invalid/next", "https://api.example.invalid/next#fragment"} {
		request, err := http.NewRequest(http.MethodPost, target, nil)
		require.NoError(t, err)
		refused := strict.CheckRedirect(request, []*http.Request{request})
		require.True(t, isEndpointRedirectError(refused), "target must fail before dispatch")
		require.NotContains(t, refused.Error(), target, "private destinations must not be copied into policy errors")
	}
	require.Equal(t, 2, callbackCalls)
	require.NoError(t, base.CheckRedirect(nil, nil))
	require.Equal(t, 3, callbackCalls)
}

func TestEndpointRedirectRefusalStopsOuterOperationRetry(t *testing.T) {
	refusal := &endpointRedirectError{reason: "destination is outside the endpoint allowlist"}
	require.False(t, RetryOperationError(manifest.RetryPolicy{MaxAttempts: 3, TransportErrors: true, ReplayRequirement: "before-first-event"}, nil, fantasy.WrapTransportError(&url.Error{Op: "Post", Err: refusal}), false))
}

func TestEndpointRedirectPolicyChecksCallerURLMutation(t *testing.T) {
	endpoint := manifest.Endpoint{BaseURL: "https://api.example.invalid", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"api.example.invalid"}, Override: "same-origin", FollowRedirects: true}
	client := EndpointHTTPClient(&http.Client{CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		request.URL.Host = "outside.example.invalid"
		return nil
	}}, endpoint)
	request, err := http.NewRequest(http.MethodPost, "https://api.example.invalid/next", nil)
	require.NoError(t, err)
	require.True(t, isEndpointRedirectError(client.CheckRedirect(request, []*http.Request{request})))
}
