package providertransport

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestLookupJSONPointerPreservesPresenceAndArrayErrors(t *testing.T) {
	document := map[string]any{
		"object": map[string]any{"null": nil, "a/b": "escaped"},
		"array":  []any{"zero", "one"},
		"scalar": "value",
	}
	tests := []struct {
		name    string
		path    string
		want    any
		present bool
		wantErr string
	}{
		{name: "missing object property", path: "/object/missing"},
		{name: "explicit null", path: "/object/null", present: true},
		{name: "array element", path: "/array/1", want: "one", present: true},
		{name: "array out of range", path: "/array/2"},
		{name: "escaped property", path: "/object/a~1b", want: "escaped", present: true},
		{name: "leading zero index", path: "/array/01", wantErr: "invalid array index"},
		{name: "negative index", path: "/array/-1", wantErr: "invalid array index"},
		{name: "overflowing index", path: "/array/18446744073709551616", wantErr: "invalid array index"},
		{name: "non-container traversal", path: "/scalar/nested", wantErr: "traverses a non-container"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, present, err := LookupJSONPointer(document, test.path)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				require.False(t, present)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.present, present)
			require.Equal(t, test.want, value)
		})
	}
}

func TestEvaluatePredicateComparesJSONNumbersExactly(t *testing.T) {
	tests := []struct {
		name     string
		actual   json.Number
		expected any
		want     bool
	}{
		{name: "equivalent decimal", actual: json.Number("1.0"), expected: float64(1), want: true},
		{name: "exact large integer", actual: json.Number("9007199254740993"), expected: int64(9007199254740993), want: true},
		{name: "distinct large integer", actual: json.Number("9007199254740993"), expected: int64(9007199254740992)},
		{name: "maximum integer", actual: json.Number("9223372036854775807"), expected: int64(9223372036854775807), want: true},
		{name: "equivalent huge exponent", actual: json.Number("1e1000000000"), expected: json.Number("10e999999999"), want: true},
		{name: "distinct huge exponent", actual: json.Number("1e1000000000"), expected: json.Number("2e1000000000")},
		{name: "signed zero", actual: json.Number("-0e1000000000"), expected: int64(0), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			matched, err := EvaluatePredicate(
				map[string]any{"value": test.actual},
				manifest.Predicate{Operation: "equals", Path: "/value", Value: &manifest.Template{Kind: "literal", Value: test.expected}},
				TemplateValues{},
			)
			require.NoError(t, err)
			require.Equal(t, test.want, matched)
		})
	}
}
