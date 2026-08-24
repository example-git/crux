package providerplugin

import (
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestCompatibilityDiagnosticsAdvertisesOnlyImplementedTransports(t *testing.T) {
	t.Parallel()

	implemented := []string{
		"continuation.previous-response",
		"transport.anthropic-messages-http",
		"transport.gemini-generate-content",
		"transport.openai-responses-http",
	}
	for _, feature := range implemented {
		value := manifest.Compatibility{
			HostAPI:          manifest.VersionBounds{Min: manifest.HostAPIVersion, Max: manifest.HostAPIVersion},
			RequiredFeatures: []string{feature},
		}
		require.Empty(t, compatibilityDiagnostics(value), feature)
	}

	unimplemented := []string{
		"transport.gemini-interactions",
		"transport.openai-responses-websocket",
	}
	for _, feature := range unimplemented {
		value := manifest.Compatibility{
			HostAPI:          manifest.VersionBounds{Min: manifest.HostAPIVersion, Max: manifest.HostAPIVersion},
			RequiredFeatures: []string{feature},
		}
		diagnostics := compatibilityDiagnostics(value)
		require.Len(t, diagnostics, 1, feature)
		require.Equal(t, "host-feature-unsupported", diagnostics[0].Code)
	}
}
