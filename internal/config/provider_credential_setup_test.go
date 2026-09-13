package config

import (
	"testing"

	kjsonschema "github.com/kaptinlin/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestCredentialRequirementsSchemaBranches(t *testing.T) {
	for _, test := range []struct {
		name   string
		schema string
		values map[string]any
		want   bool
	}{
		{name: "required", schema: `{"required":["credential"]}`, want: true},
		{name: "allOf", schema: `{"allOf":[{"required":["credential"]}]}`, want: true},
		{name: "anyOf", schema: `{"anyOf":[{"required":["credential"]}]}`, want: true},
		{name: "oneOf", schema: `{"oneOf":[{"required":["credential"]}]}`, want: true},
		{name: "reference", schema: `{"$defs":{"auth":{"required":["credential"]}},"$ref":"#/$defs/auth"}`, want: true},
		{name: "then", schema: `{"if":{"type":"object"},"then":{"required":["credential"]}}`, want: true},
		{name: "else", schema: `{"if":{"required":["mode"]},"else":{"required":["credential"]}}`, want: true},
		{name: "dependent", schema: `{"dependentSchemas":{"mode":{"required":["credential"]}}}`, values: map[string]any{"mode": true}, want: true},
		{name: "nested composition", schema: `{"allOf":[{"anyOf":[{"required":["credential"]}]}]}`, want: true},
		{name: "unrelated required", schema: `{"allOf":[{"required":["credential"]},{"required":["region"]}]}`},
		{name: "invalid value", schema: `{"required":["credential"],"properties":{"region":{"type":"string"}}}`, values: map[string]any{"region": 1}},
		{name: "nested object", schema: `{"properties":{"nested":{"required":["credential"]}}}`, values: map[string]any{"nested": map[string]any{}}},
		{name: "multiple oneOf matches", schema: `{"oneOf":[{"type":"object"},{"type":"object"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			schema, err := kjsonschema.NewCompiler().Compile([]byte(test.schema))
			require.NoError(t, err)
			values := test.values
			if values == nil {
				values = map[string]any{}
			}
			result := schema.ValidateMap(values)
			require.False(t, result.IsValid())
			require.Equal(t, test.want, onlyMissingCredentialRequirements(result, schema, values, map[string]bool{"credential": true}))
		})
	}
}

func TestCredentialRequirementsRejectAmbiguousNames(t *testing.T) {
	schema, err := kjsonschema.NewCompiler().Compile([]byte(`{"type":"object","required":["credential", "region"]}`))
	require.NoError(t, err)
	values := map[string]any{}
	result := schema.ValidateMap(values)
	require.False(t, onlyMissingCredentialRequirements(result, schema, values, map[string]bool{"credential', 'region": true}))
	require.True(t, onlyMissingCredentialRequirements(result, schema, values, map[string]bool{"credential": true, "region": true}))
}
