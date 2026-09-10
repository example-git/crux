package providerregistry

import (
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerplugin/manifest/manifesttest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func testDelegatedRegistration(t *testing.T, providerID string) Registration {
	t.Helper()
	registration, err := FromManifest(manifesttest.Delegated(providerID), manifesttest.StaticText())
	require.NoError(t, err)
	require.NoError(t, ValidateActivation(registration))
	return registration
}

func TestDelegatedRequiredManifestInputsFailVisibly(t *testing.T) {
	for _, id := range []string{"codex", "gemini-ag"} {
		for _, test := range []struct {
			name   string
			change func(*manifest.Manifest)
		}{
			{"authorization", func(m *manifest.Manifest) { m.Capabilities.OAuth[0].AuthorizationEndpoint = "missing" }},
			{"token", func(m *manifest.Manifest) { m.Capabilities.OAuth[0].TokenEndpoint = "missing" }},
			{"scopes", func(m *manifest.Manifest) { m.Capabilities.OAuth[0].Scopes = nil }},
			{"inference URL", func(m *manifest.Manifest) {
				for i := range m.Capabilities.Endpoints {
					if m.Capabilities.Endpoints[i].ID == m.Capabilities.Operations[0].Endpoint {
						m.Capabilities.Endpoints[i].BaseURL = "https://undeclared.invalid"
					}
				}
			}},
		} {
			t.Run(id+"/"+test.name, func(t *testing.T) {
				value := manifesttest.Delegated(id)
				test.change(&value)
				_, err := FromManifest(value, manifesttest.StaticText())
				require.Error(t, err)
			})
		}
	}
	value := manifesttest.Delegated("codex")
	value.Capabilities.Usage = nil
	_, err := FromManifest(value, manifesttest.StaticText())
	require.Error(t, err)
}

// Supply real synthetic endpoint bindings to the small policy-specific fixtures
// without replacing the inference/compaction policy each test is exercising.
func addTestEndpointBindings(t *testing.T, registration *Registration) {
	t.Helper()
	providerID := "codex"
	if registration.Construction == ConstructionGeminiAntigravity {
		providerID = "gemini-ag"
	}
	source := testDelegatedRegistration(t, providerID)
	registration.Manifest.Capabilities.Endpoints = source.Manifest.Capabilities.Endpoints
	registration.Manifest.Capabilities.OAuth = source.Manifest.Capabilities.OAuth
	registration.Manifest.Capabilities.Compatibility.Endpoints = source.Manifest.Capabilities.Compatibility.Endpoints
	registration.Operation.Endpoint = source.Operation.Endpoint
	for _, operation := range registration.Operations {
		if operation.Kind == "compaction" {
			operation.Endpoint = source.Operation.Endpoint
		}
	}
	if registration.Operations == nil {
		registration.Operations = map[string]*providertransport.Operation{}
	}
	if providerID == "codex" {
		registration.Usage = source.Usage
		registration.Manifest.Capabilities.Usage = source.Manifest.Capabilities.Usage
		registration.Quota = source.Quota
		registration.Operations[source.Usage.Operation] = source.Operations[source.Usage.Operation]
	}
	require.NoError(t, bindCompatibilityEndpoints(registration, *registration.Manifest.Capabilities.Compatibility))
}

func TestDelegatedEndpointBindingsAreRequiredAndDetached(t *testing.T) {
	for _, providerID := range []string{"codex", "gemini-ag"} {
		t.Run(providerID, func(t *testing.T) {
			original := manifesttest.Delegated(providerID)
			registration, err := FromManifest(original, manifesttest.StaticText())
			require.NoError(t, err)
			require.NoError(t, ValidateActivation(registration))
			original.Capabilities.Compatibility.Endpoints = nil
			_, err = FromManifest(original, manifesttest.StaticText())
			require.ErrorContains(t, err, "requires manifest endpoint bindings")
			for _, role := range []string{"identity", "secondary"} {
				broken := manifesttest.Delegated(providerID)
				if role == "identity" {
					broken.Capabilities.Compatibility.Endpoints.Identity = "missing"
				} else if providerID == "codex" {
					broken.Capabilities.Compatibility.Endpoints.Images = "missing"
				} else {
					broken.Capabilities.Compatibility.Endpoints.Project = "missing"
				}
				_, err = FromManifest(broken, manifesttest.StaticText())
				require.ErrorContains(t, err, "is missing")
			}
			clone := registration.Clone()
			if providerID == "codex" {
				clone.Codex.Identity.BaseURL = "https://changed.invalid"
			} else {
				clone.Gemini.ProjectEndpoint.BaseURL = "https://changed.invalid"
			}
			require.ErrorContains(t, ValidateActivation(clone), "do not match manifest")
			require.NoError(t, ValidateActivation(registration))
		})
	}
}
