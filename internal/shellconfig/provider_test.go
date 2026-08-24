package shellconfig

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderToolingInstructions(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `provider add codex --tooling-instructions native`)
	provider := result["providers"].(map[string]any)["codex"].(map[string]any)
	require.Equal(t, "native", provider["tooling_instructions"])
}

func TestProviderDeclarativeConfiguration(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `provider add synthetic --configuration '{"oauth_client_id":"local-client","api_base":"https://api.example.invalid"}'`)
	provider := result["providers"].(map[string]any)["synthetic"].(map[string]any)
	require.Equal(t, map[string]any{
		"oauth_client_id": "local-client",
		"api_base":        "https://api.example.invalid",
	}, provider["configuration"])
}

func TestProviderToolingInstructionsRejectsInvalidValue(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cruxrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`provider add codex --tooling-instructions official`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects crux or native")
}
