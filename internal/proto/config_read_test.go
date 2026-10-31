package proto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestConfigReadResponseValidation(t *testing.T) {
	response := ConfigReadResponse{WorkspaceID: "workspace", View: config.ConfigReadView{Values: map[string]string{"models.large.model": "main"}, Origins: map[string]config.ConfigReadOrigin{}}}
	body, err := json.Marshal(response)
	require.NoError(t, err)
	view, err := DecodeConfigReadResponse(body, "workspace")
	require.NoError(t, err)
	require.Equal(t, response.View, view)
	_, err = DecodeConfigReadResponse(body, "different")
	require.Error(t, err)
	for _, body := range []string{
		`null`, `{}`, `{"workspace_id":"workspace","view":{}}`,
		`{"workspace_id":"workspace","view":{"values":null,"origins":{}}}`,
		`{"workspace_id":"workspace","view":{"values":{"models.large.model":null},"origins":{}}}`,
		`{"workspace_id":"workspace","view":{"values":{},"origins":{}},"View":{}}`,
		`{"workspace_id":"workspace","view":{"values":{},"values":{},"origins":{}}}`,
		`{"workspace_id":"workspace","view":{"values":{"secret":"value"},"origins":{}}}`,
		string(body) + `{}`, strings.Repeat(" ", MaxConfigReadResponseBytes+1),
	} {
		_, err := DecodeConfigReadResponse([]byte(body), "workspace")
		require.Error(t, err)
	}
}
