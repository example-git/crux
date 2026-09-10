package providerregistry

import (
	"fmt"
	"net/url"
	"reflect"
	"slices"

	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

func bindCompatibilityEndpoints(registration *Registration, declaration manifest.CompatibilityAdapter) error {
	if registration.Manifest == nil || declaration.Endpoints == nil {
		return fmt.Errorf("compatibility construction %q requires manifest endpoint bindings; update the provider bundle", declaration.ID)
	}
	value := registration.Manifest
	if len(value.Capabilities.OAuth) != 1 {
		return fmt.Errorf("compatibility construction %q requires exactly one OAuth flow", declaration.ID)
	}
	flow := value.Capabilities.OAuth[0]
	if len(flow.Scopes) == 0 || slices.Contains(flow.Scopes, "") {
		return fmt.Errorf("compatibility OAuth scopes are required")
	}
	endpoint := func(role, ref, scheme string, authenticated bool) (manifest.Endpoint, error) {
		for _, e := range value.Capabilities.Endpoints {
			if ref == "" || e.ID != ref {
				continue
			}
			u, err := url.Parse(e.BaseURL)
			if err != nil || u.Scheme != scheme || u.Host == "" || u.User != nil || u.Fragment != "" || !slices.Contains(e.AllowedSchemes, u.Scheme) || !slices.Contains(e.AllowedHosts, u.Hostname()) {
				return manifest.Endpoint{}, fmt.Errorf("compatibility %s endpoint %q is invalid or outside its allowlist", role, ref)
			}
			if authenticated && e.Credential != flow.Credential {
				return manifest.Endpoint{}, fmt.Errorf("compatibility %s endpoint must bind the OAuth credential", role)
			}
			return cloneJSON(e), nil
		}
		return manifest.Endpoint{}, fmt.Errorf("compatibility %s endpoint binding %q is missing", role, ref)
	}
	authorize, err := endpoint("authorization", flow.AuthorizationEndpoint, "https", false)
	if err != nil {
		return err
	}
	token, err := endpoint("token", flow.TokenEndpoint, "https", false)
	if err != nil {
		return err
	}
	identity, err := endpoint("identity", declaration.Endpoints.Identity, "https", true)
	if err != nil {
		return err
	}
	if registration.Operation == nil {
		return fmt.Errorf("compatibility inference operation is required")
	}
	scheme := "https"
	if Construction(declaration.ID) == ConstructionCodex {
		scheme = "wss"
	}
	inference, err := endpoint("inference", registration.Operation.Endpoint.ID, scheme, true)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(inference, registration.Operation.Endpoint) {
		return fmt.Errorf("compatibility inference endpoint does not match manifest")
	}
	switch Construction(declaration.ID) {
	case ConstructionCodex:
		if declaration.Endpoints.Project != "" {
			return fmt.Errorf("Codex compatibility cannot bind a project endpoint")
		}
		images, err := endpoint("images", declaration.Endpoints.Images, "https", true)
		if err != nil {
			return err
		}
		registration.ImageEndpoint = &images
		registration.Codex = &codex.Client{Authorization: authorize, Token: token, Identity: identity, Scopes: slices.Clone(flow.Scopes)}
		if registration.Usage == nil || registration.Usage.Source != "operation" || registration.Operations[registration.Usage.Operation] == nil {
			return fmt.Errorf("Codex compatibility requires a manifest usage operation")
		}
	case ConstructionGeminiAntigravity:
		if declaration.Endpoints.Images != "" {
			return fmt.Errorf("Gemini compatibility cannot bind an images endpoint")
		}
		project, err := endpoint("project", declaration.Endpoints.Project, "https", true)
		if err != nil {
			return err
		}
		registration.Gemini = &gemini.Client{Authorization: authorize, Token: token, Identity: identity, ProjectEndpoint: project, Scopes: slices.Clone(flow.Scopes), RedirectURI: flow.Redirect.URI}
	default:
		return fmt.Errorf("unknown compatibility construction %q", declaration.ID)
	}
	return nil
}

func validateCompatibilityEndpoints(registration Registration) error {
	if registration.Manifest == nil || registration.Manifest.Capabilities.Compatibility == nil {
		return fmt.Errorf("compatibility endpoint manifest is required")
	}
	expected := registration.Clone()
	if err := bindCompatibilityEndpoints(&expected, *registration.Manifest.Capabilities.Compatibility); err != nil {
		return err
	}
	if !reflect.DeepEqual(registration.Codex, expected.Codex) || !reflect.DeepEqual(registration.Gemini, expected.Gemini) || !reflect.DeepEqual(registration.ImageEndpoint, expected.ImageEndpoint) {
		return fmt.Errorf("registered compatibility endpoints do not match manifest")
	}
	return nil
}
