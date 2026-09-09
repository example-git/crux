package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeControlManifestDefaultsPreserveExactNumbers(t *testing.T) {
	t.Parallel()
	for _, literal := range []string{"9007199254740993", "1e3", "0", "18446744073709551616"} {
		for _, caseFold := range []bool{false, true} {
			t.Run(literal+"/"+map[bool]string{false: "lower", true: "upper"}[caseFold], func(t *testing.T) {
				var document map[string]any
				require.NoError(t, json.Unmarshal(readRepoFile(t, "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json"), &document))
				document["capabilities"].(map[string]any)["runtime_controls"] = []RuntimeControl{{ID: "vendor.count", Label: "Count", Type: "integer", Scope: "model", RequestPath: "/count", Default: json.Number(literal)}}
				body, err := json.Marshal(document)
				require.NoError(t, err)
				if caseFold {
					// Exercise the typed restoration helper separately: strict
					// manifest schema validation requires canonical field names.
					body = []byte(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(string(body), `"capabilities"`, `"Capabilities"`), `"runtime_controls"`, `"Runtime_Controls"`), `"default"`, `"Default"`))
					var value Manifest
					require.NoError(t, json.Unmarshal(body, &value))
					require.NoError(t, decodeRuntimeControlDefaults(body, &value))
					require.Equal(t, json.Number(literal), value.Capabilities.RuntimeControls[0].Default)
					return
				}
				value, err := DecodeStrict(body)
				require.NoError(t, err)
				require.Equal(t, json.Number(literal), value.Capabilities.RuntimeControls[0].Default)
			})
		}
	}
}

func TestRuntimeControlManifestRejectsInvalidNumericDefaults(t *testing.T) {
	t.Parallel()
	for _, literal := range []string{"1.25", "1e-1000", "1e1000", "0e1000000000", "1e-1000000000", "0." + strings.Repeat("0", 16<<10)} {
		var document map[string]any
		require.NoError(t, json.Unmarshal(readRepoFile(t, "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json"), &document))
		document["capabilities"].(map[string]any)["runtime_controls"] = []RuntimeControl{{ID: "vendor.count", Label: "Count", Type: "integer", Scope: "model", RequestPath: "/count", Default: json.Number(literal)}}
		body, err := json.Marshal(document)
		require.NoError(t, err)
		_, err = DecodeStrict(body)
		require.Error(t, err, literal)
	}
	var value Manifest
	err := json.Unmarshal([]byte(`{"capabilities":{"runtime_controls":[{"id":"x","default":false,"unknown":true}]}}`), &value)
	require.NoError(t, err)
	_, err = DecodeStrict([]byte(`{"capabilities":{"runtime_controls":[{"id":"x","default":false,"unknown":true}]}}`))
	require.ErrorContains(t, err, "unknown field")
}
