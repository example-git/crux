package providertransport

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

type endpointRedirectError struct{ reason string }

func (err *endpointRedirectError) Error() string {
	return "provider redirect refused: " + err.reason
}

// A refused destination cannot become permitted by replaying the request or
// refreshing its credential. Keep that classification through url.Error.
func (*endpointRedirectError) NonRetryable() bool { return true }

func isEndpointRedirectError(err error) bool {
	var refusal *endpointRedirectError
	return errors.As(err, &refusal)
}

// EndpointHTTPClient captures an endpoint's redirect policy without modifying
// the supplied client. Allowlisted-host overrides keep their declared broader
// destination scope; forbidden and same-origin endpoints keep the captured
// origin, including its port. FollowRedirects permits path/query changes: the
// endpoint contract does not declare a redirect path-prefix restriction.
func EndpointHTTPClient(base *http.Client, endpoint manifest.Endpoint) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	endpoint.AllowedSchemes = slices.Clone(endpoint.AllowedSchemes)
	endpoint.AllowedHosts = slices.Clone(endpoint.AllowedHosts)
	previous := client.CheckRedirect
	declared, declarationErr := url.Parse(endpoint.BaseURL)
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !endpoint.FollowRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return &endpointRedirectError{reason: "redirect limit exceeded"}
		}
		if previous != nil {
			if err := previous(request, via); err != nil {
				return err
			}
		}
		// A caller callback can edit request.URL. Validate the final URL after
		// that callback, immediately before net/http dispatches the redirect.
		if declarationErr != nil || !validRedirectURL(declared) || request == nil || !validRedirectURL(request.URL) {
			return &endpointRedirectError{reason: "invalid endpoint"}
		}
		target := request.URL
		if !containsFold(endpoint.AllowedSchemes, target.Scheme) || !containsFold(endpoint.AllowedHosts, target.Hostname()) {
			return &endpointRedirectError{reason: "destination is outside the endpoint allowlist"}
		}
		switch endpoint.Override {
		case "allowed-hosts":
		case "forbidden", "same-origin":
			if !strings.EqualFold(target.Scheme, declared.Scheme) || !strings.EqualFold(target.Hostname(), declared.Hostname()) || target.Port() != declared.Port() {
				return &endpointRedirectError{reason: "destination changes the captured origin"}
			}
		default:
			return &endpointRedirectError{reason: "endpoint override policy is unavailable"}
		}
		return nil
	}
	return &client
}

func validRedirectURL(target *url.URL) bool {
	return target != nil && (target.Scheme == "http" || target.Scheme == "https") && target.Hostname() != "" && target.User == nil && target.Fragment == ""
}
