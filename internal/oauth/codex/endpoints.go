package codex

import "github.com/example-git/crux/internal/providerplugin/manifest"

// Client captures one registration's declared OAuth and metadata endpoints.
// There are no provider URL or scope defaults. Values are detached by the registry.
type Client struct {
	Authorization, Token, Identity manifest.Endpoint
	Scopes                         []string
}
