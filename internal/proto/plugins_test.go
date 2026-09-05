package proto_test

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestPluginStatusPluginTypeJSONCompatibility(t *testing.T) {
	t.Parallel()

	legacy := proto.PluginStatus{BundleName: "legacy.plugin"}
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "plugin_type")

	var decoded proto.PluginStatus
	require.NoError(t, json.Unmarshal([]byte(`{"bundle_name":"legacy.plugin","state":"registered","trust":"trusted","compatibility":"compatible"}`), &decoded))
	require.Empty(t, decoded.PluginType)

	decoded.PluginType = "provider-preset"
	encoded, err = json.Marshal(decoded)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"plugin_type":"provider-preset"`)
}
