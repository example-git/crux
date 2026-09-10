package config

import "github.com/example-git/crux/foundation/catalog"

// BindPreviewProviders supplies an isolated catalog to ordinary provider/model
// UI constructors without launching provider discovery or reading credentials.
func (c *Config) BindPreviewProviders(providers []catalog.Provider) {
	c.bindProviderScan(ProviderScan{Providers: providers})
}

// LoadPreview resolves the normal launch configuration/catalog, stopping before
// configuration persistence, ownership migrations, and process-state publication.
func LoadPreview(workingDir, dataDir string) (*ConfigStore, error) {
	return loadWithEnvironment(workingDir, dataDir, false, snapshotEnvironment(), false, configLoadOptions{previewReadOnly: true})
}
