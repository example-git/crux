package csync

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMapJSONPreservesNestedNumbersAndTypedFields(t *testing.T) {
	type value struct {
		Count   int            `json:"count"`
		Options map[string]any `json:"options"`
	}
	var values Map[string, value]
	require.NoError(t, json.Unmarshal([]byte(`{"provider":{"count":3,"options":{"exact":9007199254740993,"zero":0.00}}}`), &values))
	got, ok := values.Get("provider")
	require.True(t, ok)
	require.Equal(t, 3, got.Count)
	require.Equal(t, json.Number("9007199254740993"), got.Options["exact"])
	require.Equal(t, json.Number("0.00"), got.Options["zero"])
	require.Error(t, values.UnmarshalJSON([]byte(`{} {}`)))
}
