package providertransport

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestImageConditionalOmissionAndOptionalFidelity(t *testing.T) {
	literal := func(value any) manifest.ImageValue {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		return manifest.ImageValue{Literal: data}
	}
	op := func(name string, args ...manifest.ImageValue) manifest.ImageValue {
		return manifest.ImageValue{Op: name, Args: args}
	}
	for _, document := range []string{`{"synthetic":"value"}`, `{"synthetic":"value"} trailing`} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(document))
		_, err := EvaluateImageValue(op("parse-json", op("base64url-decode", literal(encoded))), nil)
		if document == `{"synthetic":"value"}` {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	_, decodeErr := EvaluateImageValue(op("base64url-decode", literal("!invalid")), nil)
	require.Error(t, decodeErr)
	missing := manifest.ImageValue{Ref: "/missing"}
	result, err := EvaluateImageValue(manifest.ImageValue{Object: map[string]manifest.ImageValue{
		"kept":    op("if", literal(true), literal("value"), missing),
		"omitted": op("if", literal(false), missing, op("omit")),
		"null":    op("optional", missing),
		"array":   {Array: []manifest.ImageValue{literal(1), op("omit"), literal(nil), literal(2)}},
	}}, map[string]any{})
	require.NoError(t, err)
	data, err := json.Marshal(ImageWorkflowValue(result))
	require.NoError(t, err)
	require.JSONEq(t, `{"kept":"value","null":null,"array":[1,null,2]}`, string(data))
	_, err = EvaluateImageValue(op("optional", op("parse-json", literal("malformed"))), nil)
	require.Error(t, err)
	_, err = EvaluateImageValue(op("if", literal("true"), literal(1), literal(2)), nil)
	require.Error(t, err)
	_, err = EvaluateImageValue(op("add", literal(json.Number("9223372036854775807")), literal(1)), nil)
	require.ErrorContains(t, err, "overflow")
	result, err = EvaluateImageValue(op("coalesce", op("regexp", literal("text"), literal("(absent)"), literal(1)), literal("fallback")), nil)
	require.NoError(t, err)
	require.Equal(t, "fallback", ImageWorkflowValue(result))
	_, err = EvaluateImageValue(op("coalesce", op("regexp", literal("text"), literal("(absent)"), literal(2)), literal("fallback")), nil)
	require.ErrorContains(t, err, "group")
}
