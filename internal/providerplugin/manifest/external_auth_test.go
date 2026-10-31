package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExternalAuthenticationPolicyValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Manifest)
		valid  bool
	}{
		{name: "valid", valid: true},
		{name: "never export", valid: true, change: func(m *Manifest) { m.Capabilities.ExternalAuth.TokenExport = "never" }},
		{name: "unknown credential", change: func(m *Manifest) { m.Capabilities.ExternalAuth.Credential = "missing" }},
		{name: "invalid service", change: func(m *Manifest) { m.Capabilities.ExternalAuth.Service = "Invalid" }},
		{name: "invalid method", change: func(m *Manifest) { m.Capabilities.ExternalAuth.Method = "not/a/method" }},
		{name: "long method", change: func(m *Manifest) { m.Capabilities.ExternalAuth.Method = strings.Repeat("a", 65) }},
		{name: "invalid export", change: func(m *Manifest) { m.Capabilities.ExternalAuth.TokenExport = "refresh-token" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := DecodeStrict(readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json"))
			require.NoError(t, err)
			value.Capabilities.ExternalAuth = &ExternalAuthPolicy{Service: "fixture", Credential: value.Capabilities.OAuth[0].Credential, Method: "oauth", TokenExport: "access-token"}
			if test.change != nil {
				test.change(&value)
			}
			data, err := json.Marshal(value)
			require.NoError(t, err)
			_, decodedErr := DecodeStrict(data)
			if test.valid {
				require.NoError(t, Validate(value))
				require.NoError(t, decodedErr)
			} else {
				require.Error(t, Validate(value))
				require.Error(t, decodedErr)
			}
		})
	}
}
