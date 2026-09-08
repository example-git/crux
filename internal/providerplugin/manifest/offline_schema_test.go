package manifest

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type offlineSchemaTransport func(*http.Request) (*http.Response, error)

func (f offlineSchemaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderMetadataSchemasValidateWithoutNetwork(t *testing.T) {
	var attempts atomic.Int32
	previous := http.DefaultTransport
	http.DefaultTransport = offlineSchemaTransport(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("network forbidden by fixture")
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json"))
	require.NoError(t, err)
	_, err = DecodeStrict(data)
	require.NoError(t, err)
	contract := MetadataContract{Namespace: "offline.fixture", Schema: map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object",
		"$defs":      map[string]any{"value": map[string]any{"type": "string"}},
		"properties": map[string]any{"value": map[string]any{"$ref": "#/$defs/value"}},
	}}
	compiled, err := CompileMetadataContracts([]MetadataContract{contract})
	require.NoError(t, err)
	require.NoError(t, ValidateMetadataValue(contract.Namespace, compiled[contract.Namespace], map[string]any{"value": "valid"}))
	require.Error(t, ValidateMetadataValue(contract.Namespace, compiled[contract.Namespace], map[string]any{"value": 123}))
	contract.Schema["type"] = "invalid-type"
	_, err = CompileMetadataContracts([]MetadataContract{contract})
	require.Error(t, err, "offline validation must still reject invalid schemas")
	delete(contract.Schema, "type")
	contract.Schema["$ref"] = "https://outside.invalid/schema.json"
	_, err = CompileMetadataContracts([]MetadataContract{contract})
	require.Error(t, err)
	require.Zero(t, attempts.Load())
}
