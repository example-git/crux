package config

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	exitVal := m.Run()
	os.Exit(exitVal)
}

func TestConfig_LoadFromBytes(t *testing.T) {
	data1 := []byte(`{"providers": {"openai": {"api_key": "key1", "base_url": "https://api.openai.com/v1"}}}`)
	data2 := []byte(`{"providers": {"openai": {"api_key": "key2", "base_url": "https://api.openai.com/v2"}}}`)
	data3 := []byte(`{"providers": {"openai": {}}}`)

	loadedConfig, err := loadFromBytes([][]byte{data1, data2, data3})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig)
	require.Equal(t, 1, loadedConfig.Providers.Len())
	pc, _ := loadedConfig.Providers.Get("openai")
	require.Equal(t, "key2", pc.APIKey)
	require.Equal(t, "https://api.openai.com/v2", pc.BaseURL)
}

func TestConfig_LoadCodebaseSearchPaths(t *testing.T) {
	loadedConfig, err := loadFromBytes([][]byte{[]byte(`{"tools":{"codebase_search":{"database_path":"/var/lib/crux/index-db","store_directory":"/var/lib/crux/index-store","enabled":false,"include_paths":["src","internal"],"exclude_paths":["src/generated"]}}}`)})
	require.NoError(t, err)
	require.Equal(t, "/var/lib/crux/index-db", loadedConfig.Tools.CodebaseSearch.DatabasePath)
	require.Equal(t, "/var/lib/crux/index-store", loadedConfig.Tools.CodebaseSearch.GetStoreDirectory())
	require.False(t, loadedConfig.Tools.CodebaseSearch.IsEnabled())
	require.Equal(t, []string{"src", "internal"}, loadedConfig.Tools.CodebaseSearch.IncludePaths)
	require.Equal(t, []string{"src/generated"}, loadedConfig.Tools.CodebaseSearch.ExcludePaths)

	legacyConfig, err := loadFromBytes([][]byte{[]byte(`{"tools":{"codebase_search":{"ann_directory":"/var/lib/crux/index-ann"}}}`)})
	require.NoError(t, err)
	require.Equal(t, "/var/lib/crux/index-ann", legacyConfig.Tools.CodebaseSearch.GetStoreDirectory())
	require.False(t, legacyConfig.Tools.CodebaseSearch.IsEnabled())

	enabledConfig, err := loadFromBytes([][]byte{[]byte(`{"tools":{"codebase_search":{"enabled":true}}}`)})
	require.NoError(t, err)
	require.True(t, enabledConfig.Tools.CodebaseSearch.IsEnabled())

	_, err = loadFromBytes([][]byte{[]byte(`{"tools":{"codebase_search":{"enabled":"yes"}}}`)})
	require.Error(t, err)
}

func TestLookupConfigs_BoundedByProject(t *testing.T) {
	// Force GlobalConfig and GlobalConfigData to point at locations we
	// control so they can be present in the result without polluting
	// the developer's real config.
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", globalDir)
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)

	t.Run("does not pick up crux.json above non-git project", func(t *testing.T) {
		parent := t.TempDir()

		// crux.json above the project must not be adopted.
		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "crux.json"),
			[]byte(`{}`),
			0o644,
		))

		project := filepath.Join(parent, "project")
		require.NoError(t, os.Mkdir(project, 0o755))

		got := lookupConfigs(project)
		for _, p := range got {
			require.NotEqual(t, filepath.Join(parent, "crux.json"), p)
		}
	})

	t.Run("does not climb out of git worktree to find crux.json", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		parent := t.TempDir()

		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "crux.json"),
			[]byte(`{}`),
			0o644,
		))

		worktree := filepath.Join(parent, "worktree")
		require.NoError(t, os.Mkdir(worktree, 0o755))
		gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
		gitInit.Dir = worktree
		require.NoError(t, gitInit.Run())

		got := lookupConfigs(worktree)
		strayEval, err := filepath.EvalSymlinks(filepath.Join(parent, "crux.json"))
		require.NoError(t, err)
		for _, p := range got {
			pEval, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			require.NotEqual(t, strayEval, pEval, "must not adopt parent crux.json")
		}
	})

	t.Run("picks up crux.json inside the project", func(t *testing.T) {
		project := t.TempDir()
		local := filepath.Join(project, "crux.json")
		require.NoError(t, os.WriteFile(local, []byte(`{}`), 0o644))

		got := lookupConfigs(project)

		localEval, err := filepath.EvalSymlinks(local)
		require.NoError(t, err)
		var foundLocal bool
		for _, p := range got {
			pEval, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			if pEval == localEval {
				foundLocal = true
				break
			}
		}
		require.True(t, foundLocal, "expected project crux.json to be in lookup result: %v", got)
	})

	t.Run("global config is always included regardless of boundary", func(t *testing.T) {
		project := t.TempDir()

		got := lookupConfigs(project)
		// Global config and global data path are always prepended,
		// even when no project file exists.
		require.Contains(t, got, GlobalConfig())
		require.Contains(t, got, GlobalConfigData())
	})

	t.Run("global shell config (cruxrc) is included", func(t *testing.T) {
		project := t.TempDir()

		got := lookupConfigs(project)
		// A global cruxrc is discovered only beside the user config. The data
		// directory is machine-owned state and must never execute a cruxrc.
		require.Contains(t, got, shellConfigSibling(GlobalConfig()))
		require.NotContains(t, got, shellConfigSibling(GlobalConfigData()))
	})

	t.Run("project cruxrc and .cruxrc are discovered", func(t *testing.T) {
		project := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(project, "cruxrc"), []byte(""), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".cruxrc"), []byte(""), 0o644))

		got := lookupConfigs(project)
		require.Contains(t, got, filepath.Join(project, "cruxrc"))
		require.Contains(t, got, filepath.Join(project, ".cruxrc"))
	})

	t.Run("system config is loaded first", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("system config not supported on Windows")
		}

		got := lookupConfigs(t.TempDir())
		require.NotEmpty(t, got)
		// The system-wide config must be first so it has the lowest
		// priority when configs are merged.
		require.Equal(t, "/etc/crux/crux.json", got[0])
	})
}

func TestLegacyCrushIdentityIsIgnored(t *testing.T) {
	legacyRoot := t.TempDir()
	configRoot := t.TempDir()
	dataRoot := t.TempDir()
	cacheRoot := t.TempDir()

	t.Setenv("CRUX_GLOBAL_CONFIG", "")
	t.Setenv("CRUX_GLOBAL_DATA", "")
	t.Setenv("CRUX_CACHE_DIR", "")
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(legacyRoot, "config"))
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(legacyRoot, "data"))
	t.Setenv("CRUSH_CACHE_DIR", filepath.Join(legacyRoot, "cache"))
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)

	require.Equal(t, filepath.Join(configRoot, "crux", "crux.json"), GlobalConfig())
	require.Equal(t, filepath.Join(dataRoot, "crux", "crux.json"), GlobalConfigData())
	require.Equal(t, filepath.Join(cacheRoot, "crux"), GlobalCacheDir())

	project := t.TempDir()
	legacyConfigs := []string{"crush.json", ".crush.json", "crushrc", ".crushrc"}
	for _, name := range legacyConfigs {
		path := filepath.Join(project, name)
		require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o644))
	}

	configs := lookupConfigs(project)
	for _, name := range legacyConfigs {
		require.NotContains(t, configs, filepath.Join(project, name))
	}
}

func TestLoadFromConfigPaths_InvalidJSON(t *testing.T) {
	t.Parallel()

	t.Run("identifies the offending file", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		good := filepath.Join(tmpDir, "good.json")
		bad := filepath.Join(tmpDir, "bad.json")
		require.NoError(t, os.WriteFile(good, []byte(`{"providers":{}}`), 0o644))
		require.NoError(t, os.WriteFile(bad, []byte(`{not valid json}`), 0o644))

		_, _, err := loadFromConfigPaths(context.Background(), []string{good, bad})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid JSON in config file")
		require.Contains(t, err.Error(), "bad.json")
	})

	t.Run("skips missing and empty files", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		empty := filepath.Join(tmpDir, "empty.json")
		require.NoError(t, os.WriteFile(empty, []byte(""), 0o644))

		cfg, _, err := loadFromConfigPaths(context.Background(), []string{
			filepath.Join(tmpDir, "nonexistent.json"),
			empty,
		})
		require.NoError(t, err)
		require.NotNil(t, cfg)
	})
}

// TestLoadFromConfigPaths_ConflictWarningNamesKeys verifies that when a JSON
// config and a cruxrc coexist in the same directory, the merge warning names
// the overlapping top-level keys so incremental migrations can spot stale
// duplicates.
func TestLoadFromConfigPaths_ConflictWarningNamesKeys(t *testing.T) {
	capture := func(t *testing.T) *strings.Builder {
		t.Helper()
		var buf strings.Builder
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })
		return &buf
	}

	t.Run("names overlapping keys", func(t *testing.T) {
		buf := capture(t)
		tmpDir := t.TempDir()
		jsonPath := filepath.Join(tmpDir, "crux.json")
		rcPath := filepath.Join(tmpDir, "cruxrc")
		require.NoError(t, os.WriteFile(jsonPath, []byte(`{"options":{"debug":true},"providers":{}}`), 0o644))
		require.NoError(t, os.WriteFile(rcPath, []byte("option debug true\n"), 0o644))

		_, _, err := loadFromConfigPaths(context.Background(), []string{jsonPath, rcPath})
		require.NoError(t, err)
		require.Contains(t, buf.String(), "cruxrc taking precedence")
		require.Contains(t, buf.String(), `"conflicting_keys":"options"`)
	})

	t.Run("no warning when nothing overlaps", func(t *testing.T) {
		buf := capture(t)
		tmpDir := t.TempDir()
		jsonPath := filepath.Join(tmpDir, "crux.json")
		rcPath := filepath.Join(tmpDir, "cruxrc")
		require.NoError(t, os.WriteFile(jsonPath, []byte(`{"providers":{}}`), 0o644))
		require.NoError(t, os.WriteFile(rcPath, []byte("option debug true\n"), 0o644))

		_, _, err := loadFromConfigPaths(context.Background(), []string{jsonPath, rcPath})
		require.NoError(t, err)
		require.NotContains(t, buf.String(), "cruxrc taking precedence",
			"disjoint coexistence should not warn")
	})
}

// testStore wraps a Config in a minimal ConfigStore for testing.
func testStore(cfg *Config) *ConfigStore {
	return &ConfigStore{config: cfg}
}

func TestOptions_validatePromptOptions(t *testing.T) {
	require.NoError(t, (&Options{}).validatePromptOptions())
	require.NoError(t, (&Options{ResponseVerbosity: "high", AnalysisEffort: "max"}).validatePromptOptions())
	require.ErrorContains(t, (&Options{ResponseVerbosity: "verbose"}).validatePromptOptions(), "response_verbosity")
	require.ErrorContains(t, (&Options{AnalysisEffort: "unlimited"}).validatePromptOptions(), "analysis_effort")
}

func TestConfig_setDefaults(t *testing.T) {
	t.Run("sets default data directory", func(t *testing.T) {
		cfg := &Config{}
		workingDir := t.TempDir()

		cfg.setDefaults(workingDir, "")

		require.NotNil(t, cfg.Options)
		require.NotNil(t, cfg.Options.TUI)
		require.NotNil(t, cfg.Options.ContextPaths)
		require.NotNil(t, cfg.Providers)
		require.NotNil(t, cfg.Models)
		require.NotNil(t, cfg.LSP)
		require.NotNil(t, cfg.MCP)
		require.Equal(t, filepath.Join(workingDir, ".crux"), cfg.Options.DataDirectory)
		require.Equal(t, AiCliProjectInstructionsPath(workingDir), cfg.Options.InitializeAs)
		for _, path := range defaultContextPaths {
			require.Contains(t, cfg.Options.ContextPaths, path)
		}
	})

	t.Run("prunes orphaned OAuth token MCP entries but keeps real ones", func(t *testing.T) {
		cfg := &Config{
			MCP: map[string]MCPConfig{
				"orphan":     {OAuthToken: &oauth.Token{AccessToken: "stale"}},
				"real-http":  {Type: MCPHttp, URL: "https://example.com/mcp", OAuthToken: &oauth.Token{AccessToken: "live"}},
				"real-stdio": {Type: MCPStdio, Command: "npx"},
				"malformed":  {Command: "npx"}, // missing type but has a command: surface the error, don't prune
			},
		}

		cfg.setDefaults(t.TempDir(), "")

		require.NotContains(t, cfg.MCP, "orphan", "orphaned token entry should be pruned")
		require.Contains(t, cfg.MCP, "real-http")
		require.Contains(t, cfg.MCP, "real-stdio")
		require.Contains(t, cfg.MCP, "malformed", "malformed entry should survive so its error surfaces")
	})

	t.Run("resolves relative configured data directory from working directory", func(t *testing.T) {
		cfg := &Config{Options: &Options{DataDirectory: "."}}
		workingDir := filepath.Join(t.TempDir(), "worktree")

		cfg.setDefaults(workingDir, "")

		require.Equal(t, workingDir, cfg.Options.DataDirectory)
	})

	t.Run("resolves relative flag data directory from working directory", func(t *testing.T) {
		cfg := &Config{}
		workingDir := filepath.Join(t.TempDir(), "worktree")

		cfg.setDefaults(workingDir, "./state")

		require.Equal(t, filepath.Join(workingDir, "state"), cfg.Options.DataDirectory)
	})

	t.Run("preserves absolute configured data directory", func(t *testing.T) {
		// Use a platform-appropriate absolute path so the test runs
		// the same way on POSIX and Windows.
		absDir := filepath.Join(t.TempDir(), "data")
		cfg := &Config{Options: &Options{DataDirectory: absDir}}

		cfg.setDefaults(filepath.Join(t.TempDir(), "worktree"), "")

		require.Equal(t, absDir, cfg.Options.DataDirectory)
	})

	t.Run("workspace merge re-entry keeps an absolute data directory", func(t *testing.T) {
		// Simulate the load and reload paths: defaults are applied
		// twice with the data directory potentially carried through
		// from an earlier merge as a relative string.
		workingDir := filepath.Join(t.TempDir(), "worktree")
		cfg := &Config{}
		cfg.setDefaults(workingDir, "")

		// Workspace JSON sets data_directory to a relative value; the
		// merge replaces the struct, then setDefaults runs again.
		cfg.Options.DataDirectory = "./state"
		cfg.setDefaults(workingDir, "")

		require.True(t, filepath.IsAbs(cfg.Options.DataDirectory),
			"data directory must remain absolute after re-merge, got %q",
			cfg.Options.DataDirectory)
		require.Equal(t, filepath.Join(workingDir, "state"), cfg.Options.DataDirectory)
	})

	t.Run("ignores a legacy .crush directory", func(t *testing.T) {
		workingDir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(workingDir, ".crush"), 0o755))

		cfg := &Config{}
		cfg.setDefaults(workingDir, "")

		require.Equal(t, filepath.Join(workingDir, ".crux"), cfg.Options.DataDirectory)
	})

	t.Run("does not adopt .crux from a parent project", func(t *testing.T) {
		parent := t.TempDir()

		// .crux in the parent: it should not be reused by the child
		// because there is no git context joining them.
		require.NoError(t, os.Mkdir(filepath.Join(parent, defaultDataDirectory), 0o755))

		child := filepath.Join(parent, "child")
		require.NoError(t, os.Mkdir(child, 0o755))

		cfg := &Config{}
		cfg.setDefaults(child, "")

		require.Equal(
			t,
			filepath.Clean(filepath.Join(child, defaultDataDirectory)),
			filepath.Clean(cfg.Options.DataDirectory),
		)
	})

	t.Run("does not climb out of git worktree to find .crux", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		parent := t.TempDir()

		// Stray .crux above the worktree root.
		require.NoError(t, os.Mkdir(filepath.Join(parent, defaultDataDirectory), 0o755))

		worktree := filepath.Join(parent, "worktree")
		require.NoError(t, os.Mkdir(worktree, 0o755))

		sub := filepath.Join(worktree, "pkg")
		require.NoError(t, os.Mkdir(sub, 0o755))

		// Make worktree a real git repo so the boundary detection
		// resolves to it, mirroring what happens with linked worktrees
		// in real usage.
		gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
		gitInit.Dir = worktree
		require.NoError(t, gitInit.Run())

		cfg := &Config{}
		cfg.setDefaults(sub, "")

		// Resolve symlinks because TempDir on macOS sits under /var
		// which is a symlink to /private/var. The data directory has
		// not been created yet, so resolve its parent and join.
		gotDir, gotName := filepath.Split(cfg.Options.DataDirectory)
		gotEvalDir, err := filepath.EvalSymlinks(filepath.Clean(gotDir))
		require.NoError(t, err)
		gotEval := filepath.Join(gotEvalDir, gotName)

		strayEval, err := filepath.EvalSymlinks(filepath.Join(parent, defaultDataDirectory))
		require.NoError(t, err)
		require.NotEqual(t, strayEval, gotEval, "must not adopt parent .crux")

		subEval, err := filepath.EvalSymlinks(sub)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(subEval, defaultDataDirectory), gotEval)
	})
}

func TestConfig_configureProviders(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Providers.Len())

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "$OPENAI_API_KEY", pc.APIKey)
}

func TestConfig_configureProvidersWithOverride(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMap[string, ProviderConfig](),
	}
	cfg.Providers.Set("openai", ProviderConfig{
		APIKey:  "xyz",
		BaseURL: "https://api.openai.com/v2",
		Models: []catwalk.Model{
			{
				ID:   "test-model",
				Name: "Updated",
			},
			{
				ID: "another-model",
			},
		},
	})
	cfg.setDefaults("/tmp", "")

	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Providers.Len())

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "xyz", pc.APIKey)
	require.Equal(t, "https://api.openai.com/v2", pc.BaseURL)
	require.Len(t, pc.Models, 2)
	require.Equal(t, "Updated", pc.Models[0].Name)
}

func TestConfig_configureProvidersWithNewProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				APIKey:  "xyz",
				BaseURL: "https://api.someendpoint.com/v2",
				Models: []catwalk.Model{
					{
						ID: "test-model",
					},
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Should be to because of the env variable
	require.Equal(t, cfg.Providers.Len(), 2)

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("custom")
	require.Equal(t, "xyz", pc.APIKey)
	// Make sure we set the ID correctly
	require.Equal(t, "custom", pc.ID)
	require.Equal(t, "https://api.someendpoint.com/v2", pc.BaseURL)
	require.Len(t, pc.Models, 1)

	_, ok := cfg.Providers.Get("openai")
	require.True(t, ok, "OpenAI provider should still be present")
}

func TestConfig_configureProvidersSetProviderID(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, cfg.Providers.Len(), 1)

	// Provider ID should be set
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "openai", pc.ID)
}

func TestConfig_EnabledProviders(t *testing.T) {
	t.Run("all providers enabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: false,
				},
			}),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 2)
	})

	t.Run("some providers disabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: true,
				},
			}),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 1)
		require.Equal(t, "openai", enabled[0].ID)
	})

	t.Run("empty providers map", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap[string, ProviderConfig](),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 0)
	})
}

func TestConfig_IsConfigured(t *testing.T) {
	t.Run("returns true when at least one provider is enabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
			}),
		}

		require.True(t, cfg.IsConfigured())
	})

	t.Run("returns false when no providers are configured", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap[string, ProviderConfig](),
		}

		require.False(t, cfg.IsConfigured())
	})

	t.Run("returns false when all providers are disabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: true,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: true,
				},
			}),
		}

		require.False(t, cfg.IsConfigured())
	})
}

func TestConfig_CanInitializeAgent(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfileCoreOnly))

	available := ProviderConfig{
		ID:     "available",
		APIKey: "key",
		Type:   catwalk.TypeOpenAICompat,
		Models: []catwalk.Model{{ID: "available-model"}},
	}
	codex := ProviderConfig{
		ID:         "codex",
		OAuthToken: &oauth.Token{AccessToken: "persisted-token"},
		Plugin:     &ProviderPluginReference{ID: "private.plugin", Version: "1"},
		Models:     []catwalk.Model{{ID: "codex-model"}},
	}
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			available.ID: available,
			codex.ID:     codex,
		}),
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: codex.ID, Model: "codex-model"},
			SelectedModelTypeSmall: {Provider: codex.ID, Model: "codex-model"},
		},
	}

	require.True(t, cfg.IsConfigured(), "the available provider reproduces the broad startup gate")
	require.False(t, cfg.IsProviderIntegrationAvailable(codex.ID))

	// Legacy target configurations may predate durable plugin ownership
	// markers. Their OAuth credential still distinguishes them from custom
	// OpenAI-compatible providers, so an inactive native profile must hide them.
	for _, id := range []string{"legacy-codex", "legacy-gemini"} {
		cfg.Providers.Set(id, ProviderConfig{
			ID: id, OAuthToken: &oauth.Token{AccessToken: "persisted-token"},
			Models: []catwalk.Model{{ID: "legacy-model"}},
		})
		require.False(t, cfg.IsProviderIntegrationAvailable(id))
	}
	require.False(t, cfg.CanInitializeAgent(), "the retained Codex selection cannot initialize without its active integration")

	cfg.Models[SelectedModelTypeLarge] = SelectedModel{Provider: available.ID, Model: "available-model"}
	cfg.Models[SelectedModelTypeSmall] = SelectedModel{Provider: available.ID, Model: "available-model"}
	require.True(t, cfg.CanInitializeAgent())
}

func TestConfig_setupAgentsWithNoDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.Equal(t, allToolNames(), coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Equal(t, resolveReadOnlyTools(allToolNames()), taskAgent.AllowedTools)
}

func TestConfig_setupAgentsWithDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"edit",
				"download",
				"grep",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)

	allowedTools := resolveAllowedTools(allToolNames(), cfg.Options.DisabledTools)
	assert.Equal(t, allowedTools, coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Equal(t, resolveReadOnlyTools(allowedTools), taskAgent.AllowedTools)
}

func TestConfig_setupAgentsWithEveryReadOnlyToolDisabled(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"codebase_search",
				"git_inspect",
				"glob",
				"grep",
				"job_list",
				"job_output",
				"task_list",
				"task_output",
				"ls",
				"lsp_call_hierarchy",
				"lsp_definition",
				"lsp_symbols",
				"memory_list",
				"project_status",
				"skill_list",
				"skill_load",
				"sourcegraph",
				"view",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.Equal(t, resolveAllowedTools(allToolNames(), cfg.Options.DisabledTools), coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Len(t, taskAgent.AllowedTools, 0)
}

func TestConfig_configureProvidersWithDisabledProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {
				Disable: true,
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)

	require.Equal(t, cfg.Providers.Len(), 1)
	prov, exists := cfg.Providers.Get("openai")
	require.True(t, exists)
	require.True(t, prov.Disable)
}

func TestConfig_configureProvidersCustomProviderValidation(t *testing.T) {
	t.Run("custom provider with missing API key is allowed, but not known providers", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
				"openai": {
					APIKey: "$MISSING",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		_, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
	})

	t.Run("custom provider with missing BaseURL is removed", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey: "test-key",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("custom provider with no models attempts discovery and is removed on failure", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models:  []catwalk.Model{},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		// Discovery fails (unreachable URL) so provider is removed.
		require.Equal(t, 0, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("custom provider with no models and discover_models:false is removed", func(t *testing.T) {
		discoverFalse := false
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:             "test-key",
					BaseURL:            "https://api.custom.com/v1",
					Models:             []catwalk.Model{},
					AutoDiscoverModels: &discoverFalse,
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 0, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("custom provider with models and discover_models:true merges discovered models", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [
				{"id": "existing-model", "object": "model"},
				{"id": "discovered-model", "object": "model"}
			]}`))
		}))
		defer server.Close()

		discoverTrue := true
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: server.URL + "/v1",
					Models: []catwalk.Model{
						{ID: "existing-model", Name: "My Custom Name", ContextWindow: 200000},
					},
					AutoDiscoverModels: &discoverTrue,
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 1, cfg.Providers.Len())
		p, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Len(t, p.Models, 2)

		// User-specified model keeps its custom fields.
		require.Equal(t, "existing-model", p.Models[0].ID)
		require.Equal(t, "My Custom Name", p.Models[0].Name)
		require.Equal(t, int64(200000), p.Models[0].ContextWindow)

		// Discovered model is appended.
		require.Equal(t, "discovered-model", p.Models[1].ID)
	})

	t.Run("custom provider with models and no discover_models uses only listed models", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{
						{ID: "my-model", Name: "My Model"},
					},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 1, cfg.Providers.Len())
		p, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Len(t, p.Models, 1)
		require.Equal(t, "my-model", p.Models[0].ID)
	})

	t.Run("custom provider with no models auto-discovers successfully", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [
				{"id": "auto-model-a", "object": "model"},
				{"id": "auto-model-b", "object": "model"}
			]}`))
		}))
		defer server.Close()

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: server.URL + "/v1",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 1, cfg.Providers.Len())
		p, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Len(t, p.Models, 2)
		require.Equal(t, "auto-model-a", p.Models[0].ID)
		require.Equal(t, "auto-model-b", p.Models[1].ID)
	})

	t.Run("custom provider with unsupported type is removed", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    "unsupported",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("valid custom provider is kept and ID is set", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    catwalk.TypeOpenAICompat,
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		customProvider, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Equal(t, "custom", customProvider.ID)
		require.Equal(t, "test-key", customProvider.APIKey)
		require.Equal(t, "https://api.custom.com/v1", customProvider.BaseURL)
	})

	t.Run("removed custom provider type is rejected", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom-anthropic": {
					APIKey:  "test-key",
					BaseURL: "https://api.anthropic.com/v1",
					Type:    catwalk.TypeAnthropic,
					Models: []catwalk.Model{{
						ID: "claude-3-sonnet",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 0, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("custom-anthropic")
		require.False(t, exists)
	})

	t.Run("disabled custom provider is preserved", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    catwalk.TypeOpenAICompat,
					Disable: true,
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 1, cfg.Providers.Len())
		pc, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.True(t, pc.Disable)
	})
}

func TestConfig_defaultModelSelection(t *testing.T) {
	t.Run("default behavior uses the default models for given provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		large, small, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "large-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(1000), large.MaxTokens)
		require.Equal(t, "small-model", small.Model)
		require.Equal(t, "openai", small.Provider)
		require.Equal(t, int64(500), small.MaxTokens)
	})
	t.Run("should error if no providers configured", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING_KEY",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		_, _, err = cfg.defaultModelSelection(knownProviders)
		require.Error(t, err)
	})
	t.Run("should not error if model is missing", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		_, _, err = cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
	})

	t.Run("should configure the default models with a custom provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING", // will not be included in the config
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{
						{
							ID:               "model",
							DefaultMaxTokens: 600,
						},
					},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		large, small, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "model", large.Model)
		require.Equal(t, "custom", large.Provider)
		require.Equal(t, int64(600), large.MaxTokens)
		require.Equal(t, "model", small.Model)
		require.Equal(t, "custom", small.Provider)
		require.Equal(t, int64(600), small.MaxTokens)
	})

	t.Run("should fail if no model configured", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING", // will not be included in the config
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models:  []catwalk.Model{},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		_, _, err = cfg.defaultModelSelection(knownProviders)
		require.Error(t, err)
	})
	t.Run("should use the default provider first", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "set",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{
						{
							ID:               "large-model",
							DefaultMaxTokens: 1000,
						},
					},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		large, small, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "large-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(1000), large.MaxTokens)
		require.Equal(t, "small-model", small.Model)
		require.Equal(t, "openai", small.Provider)
		require.Equal(t, int64(500), small.MaxTokens)
	})
}

func TestConfig_configureProvidersDisableDefaultProviders(t *testing.T) {
	t.Run("when enabled, ignores all default providers and requires full specification", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
		}

		// User references openai but doesn't fully specify it (no base_url, no
		// models). This should be rejected because disable_default_providers
		// treats all providers as custom.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					APIKey: "$OPENAI_API_KEY",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{
			"OPENAI_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.ErrorContains(t, err, "no custom providers")

		// openai should NOT be present because it lacks base_url and models.
		require.Equal(t, 0, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("openai")
		require.False(t, exists, "openai should not be present without full specification")
	})

	t.Run("when enabled, fully specified providers work", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
		}

		// User fully specifies their provider.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"my-llm": {
					APIKey:  "$MY_API_KEY",
					BaseURL: "https://my-llm.example.com/v1",
					Models: []catwalk.Model{{
						ID: "my-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{
			"MY_API_KEY":     "test-key",
			"OPENAI_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		// Only fully specified provider should be present.
		require.Equal(t, 1, cfg.Providers.Len())
		provider, exists := cfg.Providers.Get("my-llm")
		require.True(t, exists, "my-llm should be present")
		require.Equal(t, "https://my-llm.example.com/v1", provider.BaseURL)
		require.Len(t, provider.Models, 1)

		// Default openai should NOT be present.
		_, exists = cfg.Providers.Get("openai")
		require.False(t, exists, "openai should not be present")
	})

	t.Run("when disabled, includes all known providers with valid credentials", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
			{
				ID:          "anthropic",
				APIKey:      "$ANTHROPIC_API_KEY",
				APIEndpoint: "https://api.anthropic.com/v1",
				Models: []catwalk.Model{{
					ID: "claude-3",
				}},
			},
		}

		// User only configures openai, both API keys are available, but option
		// is disabled.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: false,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					APIKey: "$OPENAI_API_KEY",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{
			"OPENAI_API_KEY":    "test-key",
			"ANTHROPIC_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		// Both providers should be present.
		require.Equal(t, 2, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("openai")
		require.True(t, exists, "openai should be present")
		_, exists = cfg.Providers.Get("anthropic")
		require.True(t, exists, "anthropic should be present")
	})

	t.Run("when enabled, provider missing models attempts discovery but still triggers no-custom-providers error", func(t *testing.T) {
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"my-llm": {
					APIKey:  "test-key",
					BaseURL: "https://my-llm.example.com/v1",
					Models:  []catwalk.Model{}, // No models.
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.ErrorContains(t, err, "no custom providers")

		// Discovery fails (unreachable URL) so provider is removed.
		require.Equal(t, 0, cfg.Providers.Len())
	})

	t.Run("when enabled, provider missing base_url is rejected", func(t *testing.T) {
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"my-llm": {
					APIKey: "test-key",
					Models: []catwalk.Model{{ID: "model"}},
					// No BaseURL.
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.ErrorContains(t, err, "no custom providers")

		// Provider should be rejected for missing base_url.
		require.Equal(t, 0, cfg.Providers.Len())
	})
}

func TestConfig_setDefaultsDisableDefaultProvidersEnvVar(t *testing.T) {
	t.Run("sets option from environment variable", func(t *testing.T) {
		t.Setenv("CRUX_DISABLE_DEFAULT_PROVIDERS", "true")

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")

		require.True(t, cfg.Options.DisableDefaultProviders)
	})

	t.Run("does not override when env var is not set", func(t *testing.T) {
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
		}
		cfg.setDefaults("/tmp", "")

		require.True(t, cfg.Options.DisableDefaultProviders)
	})
}

func TestConfig_configureSelectedModels(t *testing.T) {
	t.Run("reload mode should not persist fallback defaults", func(t *testing.T) {
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "crux.json")
		require.NoError(t, os.WriteFile(globalPath, []byte(`{"models":{"large":{"provider":"ghost","model":"missing"}}}`), 0o600))

		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{ID: "large-model", DefaultMaxTokens: 1000},
					{ID: "small-model", DefaultMaxTokens: 500},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				SelectedModelTypeLarge: {Provider: "ghost", Model: "missing"},
			},
		}
		cfg.setDefaults(dir, "")
		store := &ConfigStore{config: cfg, globalDataPath: globalPath}
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), store, env, resolver, knownProviders)
		require.NoError(t, err)

		resolved, resolveErr := resolveSelectedModels(cfg, knownProviders)
		require.NoError(t, resolveErr)
		cfg.Models[SelectedModelTypeLarge] = resolved.Large
		cfg.Models[SelectedModelTypeSmall] = resolved.Small

		// In-memory falls back to default.
		require.True(t, resolved.LargeFallback)
		require.Equal(t, "openai", cfg.Models[SelectedModelTypeLarge].Provider)
		require.Equal(t, "large-model", cfg.Models[SelectedModelTypeLarge].Model)

		// Disk remains unchanged (resolveSelectedModels never persists).
		data, readErr := os.ReadFile(globalPath)
		require.NoError(t, readErr)
		require.Contains(t, string(data), `"provider":"ghost"`)
		require.Contains(t, string(data), `"model":"missing"`)
	})
	t.Run("should override defaults", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "larger-model",
						DefaultMaxTokens: 2000,
					},
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"large": {
					Model: "larger-model",
				},
			},
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		resolved, resolveErr := resolveSelectedModels(cfg, knownProviders)
		require.NoError(t, resolveErr)
		cfg.Models[SelectedModelTypeLarge] = resolved.Large
		cfg.Models[SelectedModelTypeSmall] = resolved.Small
		large := cfg.Models[SelectedModelTypeLarge]
		small := cfg.Models[SelectedModelTypeSmall]
		require.Equal(t, "larger-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(2000), large.MaxTokens)
		require.Equal(t, "small-model", small.Model)
		require.Equal(t, "openai", small.Provider)
		require.Equal(t, int64(500), small.MaxTokens)
	})
	t.Run("should be possible to use multiple providers", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
			{
				ID:                  "anthropic",
				APIKey:              "abc",
				DefaultLargeModelID: "a-large-model",
				DefaultSmallModelID: "a-small-model",
				Models: []catwalk.Model{
					{
						ID:               "a-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "a-small-model",
						DefaultMaxTokens: 200,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"small": {
					Model:     "a-small-model",
					Provider:  "anthropic",
					MaxTokens: 300,
				},
			},
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		resolved, resolveErr := resolveSelectedModels(cfg, knownProviders)
		require.NoError(t, resolveErr)
		cfg.Models[SelectedModelTypeLarge] = resolved.Large
		cfg.Models[SelectedModelTypeSmall] = resolved.Small
		large := cfg.Models[SelectedModelTypeLarge]
		small := cfg.Models[SelectedModelTypeSmall]
		require.Equal(t, "large-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(1000), large.MaxTokens)
		require.Equal(t, "a-small-model", small.Model)
		require.Equal(t, "anthropic", small.Provider)
		require.Equal(t, int64(300), small.MaxTokens)
	})

	t.Run("should override the max tokens only", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"large": {
					MaxTokens: 100,
				},
			},
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		resolved, resolveErr := resolveSelectedModels(cfg, knownProviders)
		require.NoError(t, resolveErr)
		cfg.Models[SelectedModelTypeLarge] = resolved.Large
		cfg.Models[SelectedModelTypeSmall] = resolved.Small
		large := cfg.Models[SelectedModelTypeLarge]
		require.Equal(t, "large-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(100), large.MaxTokens)
	})
	t.Run("resolve and persist fallback under writeMu does not deadlock", func(t *testing.T) {
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "crux.json")
		require.NoError(t, os.WriteFile(globalPath, []byte(`{}`), 0o600))

		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{ID: "large-model", DefaultMaxTokens: 1000},
					{ID: "small-model", DefaultMaxTokens: 500},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				SelectedModelTypeLarge: {Provider: "openai", Model: "this-model-does-not-exist"},
				SelectedModelTypeSmall: {Provider: "openai", Model: "also-does-not-exist"},
			},
		}
		cfg.setDefaults(dir, "")
		store := &ConfigStore{config: cfg, globalDataPath: globalPath}
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), store, env, resolver, knownProviders)
		require.NoError(t, err)

		// Simulate the Load path: resolve (pure), then persist fallbacks
		// under writeMu using updateLocked. Before the refactor, the
		// combined configureSelectedModels(persist=true) self-deadlocked
		// because UpdatePreferredModel re-acquired writeMu.
		done := make(chan error, 1)
		go func() {
			resolved, resolveErr := resolveSelectedModels(cfg, knownProviders)
			if resolveErr != nil {
				done <- resolveErr
				return
			}
			cfg.Models[SelectedModelTypeLarge] = resolved.Large
			cfg.Models[SelectedModelTypeSmall] = resolved.Small

			store.writeMu.Lock()
			defer store.writeMu.Unlock()
			if resolved.LargeFallback {
				if err := store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
					return store.updatePreferredModelFields(c, SelectedModelTypeLarge, resolved.Large)
				}); err != nil {
					done <- err
					return
				}
			}
			if resolved.SmallFallback {
				if err := store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
					return store.updatePreferredModelFields(c, SelectedModelTypeSmall, resolved.Small)
				}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()

		select {
		case err := <-done:
			require.NoError(t, err)
			// Should have fallen back to defaults.
			require.Equal(t, "large-model", cfg.Models[SelectedModelTypeLarge].Model)
			require.Equal(t, "small-model", cfg.Models[SelectedModelTypeSmall].Model)
		case <-time.After(5 * time.Second):
			t.Fatal("resolve + persist deadlocked under writeMu")
		}
	})
}

// TestConfig_configureProviders_ProviderHeaderResolveError verifies
// that a failing $(cmd) in a provider header fails the provider load
// with a clear message that names the offending header. Provider
// headers share the MCP error contract.
func TestConfig_configureProviders_ProviderHeaderResolveError(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {
				ExtraHeaders: map[string]string{
					// Failing $(...) — inner command exits 1. Must
					// propagate as an error, not a silent truncation.
					"X-Broken": "$(false)",
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.Error(t, err, "failing $(cmd) in a header must fail the provider load")
	require.Contains(t, err.Error(), "X-Broken", "error must name the offending header")
}

// TestConfig_configureProviders_CatwalkDefaultWithUnsetVarLoads
// verifies that a Catwalk-style default header like
// "OpenAI-Organization": "$OPENAI_ORG_ID" loads cleanly under lenient
// nounset (unset → "" → header dropped), and does not fail the load
// or leave the literal template on the wire.
func TestConfig_configureProviders_CatwalkDefaultWithUnsetVarLoads(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
			DefaultHeaders: map[string]string{
				"OpenAI-Organization": "$OPENAI_ORG_ID",
			},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "optional env-gated header must not fail the load")

	pc, ok := cfg.Providers.Get("openai")
	require.True(t, ok, "openai provider must still be configured")
	_, present := pc.ExtraHeaders["OpenAI-Organization"]
	require.False(t, present, "header whose value resolves to empty must be absent")
}

// TestConfig_configureProviders_LiteralEmptyHeaderDropped pins design
// decision #18 for the literal case: a user-authored
// "X-Custom": "" in extra_headers is absent from the resolved map.
// Applies to both known- and custom-provider paths; this test
// exercises the custom-provider loop.
func TestConfig_configureProviders_LiteralEmptyHeaderDropped(t *testing.T) {
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"my-llm": {
				APIKey:  "test-key",
				BaseURL: "https://my-llm.example.com/v1",
				Type:    catwalk.TypeOpenAICompat,
				Models:  []catwalk.Model{{ID: "m"}},
				ExtraHeaders: map[string]string{
					"X-Custom": "",
					"X-Kept":   "present",
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, []catwalk.Provider{})
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("my-llm")
	require.True(t, ok)
	_, present := pc.ExtraHeaders["X-Custom"]
	require.False(t, present, "literal empty-string header must be dropped")
	require.Equal(t, "present", pc.ExtraHeaders["X-Kept"])
}

// TestConfig_configureProviders_EchoEmptyHeaderDropped pins design
// decision #18 for the non-failing empty case: $(echo) exits 0 with
// empty output, resolves cleanly to "", and must be dropped the same
// way an unset bare $VAR is. Exercises the known-provider loop.
func TestConfig_configureProviders_EchoEmptyHeaderDropped(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
			DefaultHeaders: map[string]string{
				"X-Empty": "$(echo)",
				"X-Kept":  "present",
			},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("openai")
	require.True(t, ok)
	_, present := pc.ExtraHeaders["X-Empty"]
	require.False(t, present, "$(echo) → empty → header must be dropped")
	require.Equal(t, "present", pc.ExtraHeaders["X-Kept"])
}

// TestConfig_configureProviders_UnsetAPIKeySkipsProvider verifies that
// under the lenient-nounset shell resolver, $UNSET_API_KEY expands to
// ("", nil) rather than ("", err), and the existing
// `v == "" || err != nil` skip path at load.go:331 still drops the
// provider. The slog.Warn line is emitted on the same
// path but is not asserted here — internal/config/load_test.go's
// TestMain replaces the default slog handler with an io.Discard
// writer, so capturing that log line would require mid-test handler
// swapping and a sync.Mutex dance that adds more flake surface than
// signal. The observable outcome (provider absent from the map) is
// what downstream code — model picker, agent wiring — actually reads,
// so that's what we pin.
func TestConfig_configureProviders_UnsetAPIKeySkipsProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$SOMETHING_UNSET",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	// Existing user config for this known provider so the load.go:332
	// `if configExists` branch fires and actually calls Providers.Del.
	// Without it the provider was never in the map to begin with and
	// the test would pass trivially.
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {BaseURL: "custom-url"},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "skip path must not surface as a load error")

	require.Equal(t, 0, cfg.Providers.Len(), "provider with unset API key must be skipped")
	_, exists := cfg.Providers.Get("openai")
	require.False(t, exists)
}

// TestConfig_configureProviders_FailingAPIKeyCmdSkipsProvider pins
// that the two failure modes for APIKey — ("", nil) from an unset var
// under lenient nounset and ("", err) from a failing $(cmd) — are
// equivalent for the skip outcome at load.go:331. The `v == "" ||
// err != nil` check fires on either branch; this test locks in that
// equivalence so a future refactor that splits the check into two
// paths doesn't accidentally start propagating $(false) as a load
// error while keeping unset-var as a silent skip (or vice versa).
func TestConfig_configureProviders_FailingAPIKeyCmdSkipsProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$(false)",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {BaseURL: "custom-url"},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "failing $(cmd) in API key must skip provider, not fail load")

	require.Equal(t, 0, cfg.Providers.Len(), "provider with failing $(cmd) API key must be skipped")
	_, exists := cfg.Providers.Get("openai")
	require.False(t, exists)
}

// TestConfig_configureProviders_UnsetAzureEndpointSkipsProvider pins
// the same contract on the Azure path at load.go:287 — APIEndpoint is
// the field that gates Azure and goes through the same
// `v == "" || err != nil` skip check. Covered here so both branches
// of the shared skip pattern (APIKey default path and APIEndpoint
// Azure path) are tested; a future refactor that unifies them can
// rely on these two tests to catch drift.

func TestConfig_LoadFromBytes_Env(t *testing.T) {
	data := []byte(`{"env": {"AWS_PROFILE": "my-profile", "AWS_REGION": "us-west-2"}}`)

	loadedConfig, err := loadFromBytes([][]byte{data})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig.Env)
	require.Equal(t, "my-profile", loadedConfig.Env["AWS_PROFILE"])
	require.Equal(t, "us-west-2", loadedConfig.Env["AWS_REGION"])
}

func TestConfig_LoadFromBytes_EnvMerge(t *testing.T) {
	data1 := []byte(`{"env": {"AWS_PROFILE": "first", "AWS_REGION": "us-east-1"}}`)
	data2 := []byte(`{"env": {"AWS_PROFILE": "second"}}`)

	loadedConfig, err := loadFromBytes([][]byte{data1, data2})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig.Env)
	require.Equal(t, "second", loadedConfig.Env["AWS_PROFILE"])
	require.Equal(t, "us-east-1", loadedConfig.Env["AWS_REGION"])
}
