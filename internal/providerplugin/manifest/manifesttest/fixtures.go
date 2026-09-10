// Package manifesttest provides synthetic bundle declarations for tests.
package manifesttest

import (
	"embed"
	"encoding/json"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

//go:embed *.json
var files embed.FS

// Delegated returns a detached declaration with synthetic endpoints and models.
func Delegated(providerID string) manifest.Manifest {
	data, err := files.ReadFile(providerID + ".json")
	if err != nil {
		panic(err)
	}
	var value manifest.Manifest
	if err := json.Unmarshal(data, &value); err != nil {
		panic(err)
	}
	return value
}

// StaticText is synthetic content for the declared native tooling profile.
func StaticText() map[string]string {
	return map[string]string{"instructions/native.md": "Synthetic native tooling instructions for the endpoint fixture.\n"}
}
