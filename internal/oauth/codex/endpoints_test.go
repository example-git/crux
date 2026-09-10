package codex

import (
	"net/url"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

func testEndpoint(raw string) manifest.Endpoint {
	u, _ := url.Parse(raw)
	return manifest.Endpoint{BaseURL: raw, AllowedSchemes: []string{u.Scheme}, AllowedHosts: []string{u.Hostname()}, Override: "forbidden"}
}

func testClient() Client {
	return Client{Authorization: testEndpoint("https://codex-auth.example.invalid/oauth/authorize"), Token: testEndpoint("https://codex-token.example.invalid/oauth/token"), Identity: testEndpoint("https://codex-identity.example.invalid/userinfo"), Scopes: []string{"fixture.read", "fixture.email"}}
}
