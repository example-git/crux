package providerplugin

import "path/filepath"

const (
	bundleSuffix       = ".plugin"
	manifestFilename   = "manifest.json"
	trustFilename      = "trust.json"
	provenanceFilename = "provenance.json"
	managerLockName    = "plugins.lock"
	pluginStateDir     = "plugin-state"
	pluginCacheSubdir  = "plugins"
)

// Paths contains the host-owned locations used by provider plugins. Installed
// bundles and trust are global to the execution host, never project-scoped.
type Paths struct {
	Root           string
	Bundles        string
	Cache          string
	State          string
	TrustFile      string
	ProvenanceFile string
	ManagerLock    string
}

// DefaultPaths derives provider-plugin storage from host-owned global data and
// cache roots supplied by the application. Keeping path policy outside this
// package lets the configuration layer consume registry projections without an
// import cycle.
func DefaultPaths(root, cacheRoot string) Paths {
	state := filepath.Join(root, pluginStateDir)
	return Paths{
		Root:           root,
		Bundles:        filepath.Join(root, "plugins"),
		Cache:          filepath.Join(cacheRoot, pluginCacheSubdir),
		State:          state,
		TrustFile:      filepath.Join(state, trustFilename),
		ProvenanceFile: filepath.Join(state, provenanceFilename),
		ManagerLock:    filepath.Join(state, managerLockName),
	}
}
