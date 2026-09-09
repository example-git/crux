package providertransport

import (
	"net/http"
	"net/url"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

// CapturedOriginHTTPClient permits redirects within the selected endpoint's
// origin, including its literal port, without giving native/custom credentials
// an implicit wider destination allowlist. Path and query changes remain valid.
// Manifest-backed callers must use their declared EndpointHTTPClient instead.
func CapturedOriginHTTPClient(base *http.Client, endpoint string) *http.Client {
	policy := manifest.Endpoint{BaseURL: endpoint, Override: "same-origin", FollowRedirects: true}
	if target, err := url.Parse(endpoint); err == nil {
		policy.AllowedSchemes = []string{target.Scheme}
		policy.AllowedHosts = []string{target.Hostname()}
	}
	return EndpointHTTPClient(base, policy)
}
