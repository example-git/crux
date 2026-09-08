package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeBrandingStrict(t *testing.T) {
	for _, input := range []string{`{}`, `{"short_name":"ABCDE"}`, `{"label":"Example","short_name":"EXAMP","color":"#123456","gradient_a":"#234567","gradient_b":"#ABCDEF"}`} {
		_, err := DecodeBrandingStrict([]byte(input))
		require.NoError(t, err, input)
	}
	for _, input := range []string{`null`, `[]`, `{"unknown":true}`, `{"color":"red"}`, `{"gradient_a":"#123"}`, `{"short_name":"bad\nlabel"}`, `{"short_name":"` + strings.Repeat("a", 25) + `"}`, `{} {}`, `{"label":null}`, strings.Repeat(" ", MaxBrandingBytes+1), string([]byte{0xff})} {
		_, err := DecodeBrandingStrict([]byte(input))
		require.Error(t, err, input)
	}
}

func TestBrandingSchemaIsCurrent(t *testing.T) {
	data, err := BrandingSchemaJSON()
	require.NoError(t, err)
	stored, err := os.ReadFile(filepath.Join("..", "..", "..", "provider-branding.schema.json"))
	require.NoError(t, err)
	require.JSONEq(t, string(data), string(stored))
}
