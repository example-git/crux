package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
)

func runtimeControlTestStore(t *testing.T, protocol string, controls []manifest.RuntimeControl) (*ConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	workingDir := filepath.Join(root, "project")
	source := filepath.Join(root, "source.plugin")
	for _, directory := range []string{configDir, workingDir, source} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
	}
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	base := env.NewFromMap(map[string]string{
		"HOME": root, "CRUX_GLOBAL_CONFIG": configDir,
		"CRUX_GLOBAL_DATA": filepath.Join(root, "global-data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"),
		"CRUX_PROVIDER_PROFILE": string(ProviderProfilePluginNative),
	})
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json"))
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(data, &document))
	capabilities := document["capabilities"].(map[string]any)
	capabilities["runtime_controls"] = controls
	operation := capabilities["operations"].([]any)[0].(map[string]any)
	operation["protocol"] = protocol
	if protocol == "openai-responses" {
		operation["transport"] = "sse"
	}
	data, err = json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(filepath.Join(root, "global-data"), filepath.Join(root, "cache")))
	require.NoError(t, err)
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
	manager.Close()
	configuration := `{"providers":{"example-echo":{"plugin":{"id":"example.echo","version":"1.0.0"},"api_key":"fixture-key"}},"models":{"large":{"provider":"example-echo","model":"echo-1","max_tokens":1024,"temperature":0.25,"provider_options":{"sibling":false}},"small":{"provider":"example-echo","model":"echo-1","max_tokens":512,"top_p":0.8,"provider_options":{"sibling":7}}}}`
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "crux.json"), []byte(configuration), 0o600))
	store, err := LoadIsolated(workingDir, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	return store, root
}

func runtimeControlTestTarget(t *testing.T, store *ConfigStore, id string) RuntimeControlTarget {
	t.Helper()
	selected := store.Config().Models[SelectedModelTypeLarge]
	owner, ok := store.Config().ProviderOwner(selected.Provider)
	require.True(t, ok)
	state, err := store.RuntimeControlState(t.Context(), ScopeWorkspace, RuntimeControlTarget{
		Owner: owner, ControlID: id, Selection: RuntimeControlSelection{ModelType: SelectedModelTypeLarge, ModelID: selected.Model},
	})
	require.NoError(t, err)
	return state.Target
}

func TestRuntimeControlLiteralValuesAndRemoval(t *testing.T) {
	for _, test := range []struct {
		typeName string
		value    string
		fallback any
	}{
		{"boolean", "false", true}, {"integer", "0", 4}, {"number", "0.0", 1.25}, {"string", `""`, "fallback"},
	} {
		t.Run(test.typeName, func(t *testing.T) {
			control := manifest.RuntimeControl{ID: "vendor.mode", Label: "Mode", Type: test.typeName, Scope: "model", RequestPath: "/vendor/mode", Default: test.fallback}
			store, _ := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
			target := runtimeControlTestTarget(t, store, control.ID)
			before := store.RuntimeSnapshot()
			state, err := store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, json.RawMessage(test.value))
			require.NoError(t, err)
			require.True(t, state.ScopedKnown)
			require.True(t, state.Scoped.Present)
			require.True(t, RuntimeControlJSONEqual(state.Effective.Value, json.RawMessage(test.value)))
			require.Equal(t, "model", state.Source.Kind)
			require.Equal(t, control.ID, state.Source.Key)
			require.Equal(t, before.AgentModelState().Small, state.Models.Small)
			require.Equal(t, before.AgentModelState().Large.Model.Temperature, state.Models.Large.Model.Temperature)
			require.Equal(t, false, state.Models.Large.Model.ProviderOptions["sibling"])
			require.NotContains(t, before.AgentModelState().Large.Model.ProviderOptions, control.ID)
			data, err := os.ReadFile(store.workspacePath)
			require.NoError(t, err)
			scoped, err := runtimeControlReadField(data, []string{"models", "large", "provider_options", control.ID})
			require.NoError(t, err)
			require.True(t, RuntimeControlJSONEqual(scoped.Value, json.RawMessage(test.value)))
			wrong, err := runtimeControlReadField(data, []string{"models", "large", "provider_options", "vendor"})
			require.NoError(t, err)
			require.False(t, wrong.Present, "dotted control IDs are literal keys")
			state.Effective.Value[0] = 'X'
			delete(state.Models.Large.Model.ProviderOptions, control.ID)
			current, err := store.RuntimeControlState(t.Context(), ScopeWorkspace, target)
			require.NoError(t, err)
			require.True(t, RuntimeControlJSONEqual(current.Effective.Value, json.RawMessage(test.value)))
			removed, err := store.RemoveRuntimeControl(t.Context(), ScopeWorkspace, target)
			require.NoError(t, err)
			require.False(t, removed.Scoped.Present)
			require.Equal(t, "manifest", removed.Source.Kind)
			fallback, err := json.Marshal(test.fallback)
			require.NoError(t, err)
			require.True(t, RuntimeControlJSONEqual(removed.Effective.Value, fallback))
		})
	}
}

func TestRuntimeControlRejectsTypesAndFencesWithoutWrites(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.mode", Label: "Mode", Type: "enum", Values: []string{"low", "high"}, Default: "low", Scope: "model", RequestPath: "/vendor/mode"}
	store, root := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
	target := runtimeControlTestTarget(t, store, control.ID)
	before := store.Config()
	files := remoteBaselineTree(t, root)
	for _, value := range []json.RawMessage{nil, []byte("null"), []byte("false"), []byte("0"), []byte(`""`), []byte(`"unknown"`), []byte(`"low" {}`)} {
		_, err := store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, value)
		require.Error(t, err)
		require.Same(t, before, store.Config())
		require.Equal(t, files, remoteBaselineTree(t, root))
	}
	for _, change := range []func(*RuntimeControlTarget){
		func(value *RuntimeControlTarget) { value.Owner.ManifestVersion = "stale" },
		func(value *RuntimeControlTarget) { value.Selection.ModelID = "other" },
		func(value *RuntimeControlTarget) { value.Selection.ModelType = "other" },
		func(value *RuntimeControlTarget) { value.DescriptorDigest = "" },
		func(value *RuntimeControlTarget) { value.DescriptorDigest = strings.Repeat("f", 64) },
	} {
		stale := target
		change(&stale)
		_, err := store.SetRuntimeControl(t.Context(), ScopeWorkspace, stale, []byte(`"high"`))
		require.Error(t, err)
		require.Equal(t, files, remoteBaselineTree(t, root))
	}
}

func TestRuntimeControlScopeAndPinnedShadows(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.mode", Label: "Mode", Type: "string", Default: "default", Scope: "model", RequestPath: "/vendor/mode"}
	store, root := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
	target := runtimeControlTestTarget(t, store, control.ID)
	_, err := store.SetRuntimeControl(t.Context(), ScopeGlobal, target, []byte(`"global"`))
	require.NoError(t, err)
	_, err = store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte(`"workspace"`))
	require.NoError(t, err)
	files := remoteBaselineTree(t, root)
	_, err = store.SetRuntimeControl(t.Context(), ScopeGlobal, target, []byte(`"shadowed"`))
	require.ErrorContains(t, err, "shadowed")
	require.Equal(t, files, remoteBaselineTree(t, root))
	state, err := store.RemoveRuntimeControl(t.Context(), ScopeWorkspace, target)
	require.NoError(t, err)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"global"`)))
	require.Equal(t, ScopeGlobal, *state.Source.Scope)
	models := store.Config().AgentModelState()
	_, err = store.OverrideModelsForOwners(models)
	require.NoError(t, err)
	files = remoteBaselineTree(t, root)
	_, err = store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte(`"blocked-by-pin"`))
	require.ErrorContains(t, err, "pinned run override")
	require.Equal(t, files, remoteBaselineTree(t, root))
	state, err = store.RemoveRuntimeControl(t.Context(), ScopeGlobal, target)
	require.NoError(t, err)
	require.False(t, state.Scoped.Present)
	require.Equal(t, "run-override", state.Source.Kind)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"global"`)))
}

func TestRuntimeControlGlobalOverrideAndSmallModel(t *testing.T) {
	control := manifest.RuntimeControl{ID: "reasoning_effort", Label: "Effort", Type: "enum", Values: []string{"low", "high"}, Default: "low", Scope: "model", RequestPath: "/reasoning/effort"}
	store, root := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
	target := runtimeControlTestTarget(t, store, control.ID)
	_, err := store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte(`"high"`))
	require.NoError(t, err)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.analysis_effort", "low"))
	files := remoteBaselineTree(t, root)
	_, err = store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte(`"high"`))
	require.ErrorContains(t, err, `options.analysis_effort="low"`)
	require.Equal(t, files, remoteBaselineTree(t, root))
	state, err := store.RemoveRuntimeControl(t.Context(), ScopeWorkspace, target)
	require.NoError(t, err)
	require.Equal(t, "host-option", state.Source.Kind)
	require.NotNil(t, state.GlobalOverride)
	target.Selection.ModelType = SelectedModelTypeSmall
	state, err = store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte(`"high"`))
	require.NoError(t, err)
	require.Nil(t, state.GlobalOverride, "explicit small-model control replaces the constructor global")
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"high"`)))
	state, err = store.RemoveRuntimeControl(t.Context(), ScopeWorkspace, target)
	require.NoError(t, err)
	require.False(t, state.Scoped.Present)
	require.Equal(t, "host-option", state.Source.Kind)
	require.NotNil(t, state.GlobalOverride)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"low"`)), "small removal inherits the constructor global")
}

func TestRuntimeControlSmallConstructorPriority(t *testing.T) {
	for _, protocol := range []string{"generic-json", "openai-responses"} {
		t.Run(protocol, func(t *testing.T) {
			control := manifest.RuntimeControl{ID: "reasoning_effort", Label: "Effort", Type: "enum", Values: []string{"low", "medium", "high"}, Scope: "model", RequestPath: "/reasoning/effort"}
			store, _ := runtimeControlTestStore(t, protocol, []manifest.RuntimeControl{control})
			for _, fallback := range []struct {
				name  string
				value any
			}{{"without-default", nil}, {"with-default", "medium"}} {
				for _, reasoning := range []struct {
					name string
					can  bool
				}{{"nonreasoning", false}, {"reasoning", true}} {
					for _, global := range []string{"", "low"} {
						for _, explicit := range []string{"", "high"} {
							t.Run(fallback.name+"/"+reasoning.name+"/global="+global+"/explicit="+explicit, func(t *testing.T) {
								cfg := store.Config().cloneForWrite()
								registration, ok := cfg.ProviderRegistration("example-echo")
								require.True(t, ok)
								registration.RuntimeControls[0].Default = fallback.value
								registration.Manifest.Capabilities.RuntimeControls = registration.RuntimeControls
								registry, err := providerregistry.New(registration)
								require.NoError(t, err)
								scan := *cfg.providerScan
								scan.Registry = registry
								cfg.bindProviderScan(scan)
								cfg.Options.AnalysisEffort = global
								provider, _ := cfg.Providers.Get("example-echo")
								provider = cloneProviderConfig(provider)
								provider.Models[0].CanReason = reasoning.can
								provider.Models[0].ReasoningLevels = []string{"medium", "high"}
								provider.Models[0].DefaultReasoningEffort = "medium"
								cfg.Providers.Set("example-echo", provider)
								selected := cloneSelectedModel(cfg.Models[SelectedModelTypeSmall])
								selected.ReasoningEffort = "high"
								if explicit != "" {
									selected.ProviderOptions[control.ID] = explicit
								}
								cfg.Models[SelectedModelTypeSmall] = selected
								target := RuntimeControlTarget{Owner: registration.Owner(), ControlID: control.ID, Selection: RuntimeControlSelection{ModelType: SelectedModelTypeSmall, ModelID: selected.Model}}
								state, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
								require.NoError(t, err)
								var expected any
								kind := "absent"
								switch {
								case explicit != "":
									expected, kind = explicit, "model"
								case reasoning.can:
									expected, kind = "high", "model"
								case global != "":
									expected, kind = global, "host-option"
								case fallback.value != nil:
									expected, kind = fallback.value, "manifest"
								}
								require.Equal(t, kind, state.Source.Kind)
								require.Equal(t, protocol == "openai-responses" && (kind == "absent" || kind == "manifest" || kind == "host-option"), state.RuntimeDependent)
								require.Equal(t, expected != nil, state.Effective.Present)
								if expected != nil {
									raw, err := json.Marshal(expected)
									require.NoError(t, err)
									require.True(t, RuntimeControlJSONEqual(raw, state.Effective.Value))
								}
								if kind == "host-option" {
									require.NotNil(t, state.GlobalOverride)
								} else {
									require.Nil(t, state.GlobalOverride)
								}
								if global != "" {
									target.Selection.ModelType = SelectedModelTypeLarge
									main, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
									require.NoError(t, err)
									require.Equal(t, "host-option", main.Source.Kind)
									require.False(t, main.RuntimeDependent)
									require.True(t, RuntimeControlJSONEqual(main.Effective.Value, []byte(`"low"`)))
								}
							})
						}
					}
				}
			}
		})
	}
}

func TestRuntimeControlTransformFallbackIsExplicitlyRequestDependent(t *testing.T) {
	control := manifest.RuntimeControl{ID: "reasoning_effort", Label: "Effort", Type: "enum", Values: []string{"low", "medium", "high"}, Default: "medium", Scope: "model", RequestPath: "/reasoning/effort"}
	store, _ := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
	cfg := store.Config().cloneForWrite()
	registration, ok := cfg.ProviderRegistration("example-echo")
	require.True(t, ok)
	registration.Operation = registration.Operation.Clone()
	registration.Operation.RequestTransform = &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "delete", Path: "/reasoning/effort"}}}
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	scan := *cfg.providerScan
	scan.Registry = registry
	cfg.bindProviderScan(scan)
	target := RuntimeControlTarget{Owner: registration.Owner(), ControlID: control.ID, Selection: RuntimeControlSelection{ModelType: SelectedModelTypeSmall, ModelID: "echo-1"}}
	for _, global := range []string{"", "low"} {
		cfg.Options.AnalysisEffort = global
		state, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
		require.NoError(t, err)
		require.Equal(t, "before-request-transform", state.Binding.FallbackMode)
		require.True(t, state.RuntimeDependent)
		expected := `"medium"`
		if global != "" {
			expected = `"low"`
		}
		require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(expected)), "configured fallback is preserved, not silently converted to absent")
		data, err := json.Marshal(state)
		require.NoError(t, err)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &wire))
		require.Equal(t, "true", string(wire["runtime_dependent"]))
	}
	registration.RuntimeControls[0].Default = nil
	registration.Manifest.Capabilities.RuntimeControls = registration.RuntimeControls
	registry, err = providerregistry.New(registration)
	require.NoError(t, err)
	scan.Registry = registry
	cfg.bindProviderScan(scan)
	cfg.Options.AnalysisEffort = ""
	absent, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
	require.NoError(t, err)
	require.False(t, absent.Effective.Present)
	require.Equal(t, "absent", absent.Source.Kind)
	require.True(t, absent.RuntimeDependent, "request transform can supply a value despite no configured default")
	cfg.Options.AnalysisEffort = "low"
	selected := cloneSelectedModel(cfg.Models[SelectedModelTypeSmall])
	selected.ProviderOptions[control.ID] = "high"
	cfg.Models[SelectedModelTypeSmall] = selected
	state, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
	require.NoError(t, err)
	require.False(t, state.RuntimeDependent)
	require.Nil(t, state.GlobalOverride)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"high"`)))
	target.Selection.ModelType = SelectedModelTypeLarge
	state, err = cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
	require.NoError(t, err)
	require.False(t, state.RuntimeDependent)
	require.NotNil(t, state.GlobalOverride)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"low"`)))
}

func TestRuntimeControlUnmappedRequestOptionFallbackCollision(t *testing.T) {
	for _, pointer := range []struct {
		path     string
		ancestor string
	}{{"/reasoning/effort", "reasoning"}, {"/reasoning~1variant/effort", "reasoning/variant"}, {"/reasoning~0variant/effort", "reasoning~variant"}} {
		t.Run(pointer.path, func(t *testing.T) {
			control := manifest.RuntimeControl{ID: "analysis_effort", Label: "Effort", Type: "enum", Values: []string{"low", "medium", "high"}, Default: "medium", Scope: "model", RequestPath: pointer.path}
			store, _ := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
			for _, options := range []struct {
				name      string
				values    map[string]any
				dependent bool
			}{{"absent", nil, false}, {"unrelated", map[string]any{"unrelated": true}, false}, {"ancestor", map[string]any{pointer.ancestor: map[string]any{"effort": "high"}}, true}} {
				t.Run(options.name, func(t *testing.T) {
					cfg := store.Config().cloneForWrite()
					selected := cloneSelectedModel(cfg.Models[SelectedModelTypeSmall])
					selected.ProviderOptions = nil
					if options.values != nil {
						selected.ProviderOptions = cloneSelectedModel(SelectedModel{ProviderOptions: options.values}).ProviderOptions
					}
					cfg.Models[SelectedModelTypeSmall] = selected
					owner, ok := cfg.ProviderOwner(selected.Provider)
					require.True(t, ok)
					target := RuntimeControlTarget{Owner: owner, ControlID: control.ID, Selection: RuntimeControlSelection{ModelType: SelectedModelTypeSmall, ModelID: selected.Model}}
					for _, global := range []string{"", "low"} {
						cfg.Options.AnalysisEffort = global
						state, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
						require.NoError(t, err)
						require.Equal(t, "before-request-options", state.Binding.FallbackMode)
						require.Equal(t, options.dependent, state.RuntimeDependent)
						expected := `"medium"`
						if global != "" {
							expected = `"low"`
						}
						require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(expected)))
					}
					registration, ok := cfg.ProviderRegistration(selected.Provider)
					require.True(t, ok)
					registration.RuntimeControls[0].Default = nil
					registration.Manifest.Capabilities.RuntimeControls = registration.RuntimeControls
					registry, err := providerregistry.New(registration)
					require.NoError(t, err)
					scan := *cfg.providerScan
					scan.Registry = registry
					cfg.bindProviderScan(scan)
					cfg.Options.AnalysisEffort = ""
					absent, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
					require.NoError(t, err)
					require.False(t, absent.Effective.Present)
					require.Equal(t, "absent", absent.Source.Kind)
					require.Equal(t, options.dependent, absent.RuntimeDependent, "unmapped ancestor may supply an otherwise absent control")
					cfg.Options.AnalysisEffort = "low"
					if selected.ProviderOptions == nil {
						selected.ProviderOptions = map[string]any{}
					}
					selected.ProviderOptions[control.ID] = "high"
					cfg.Models[SelectedModelTypeSmall] = selected
					state, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
					require.NoError(t, err)
					require.False(t, state.RuntimeDependent)
					require.Nil(t, state.GlobalOverride)
					require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"high"`)))
					target.Selection.ModelType = SelectedModelTypeLarge
					state, err = cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
					require.NoError(t, err)
					require.False(t, state.RuntimeDependent)
					require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"low"`)))
				})
			}
		})
	}
}

func TestRuntimeControlSetRejectsPinnedRequestDependentFallbackBeforeWrite(t *testing.T) {
	control := manifest.RuntimeControl{ID: "reasoning_effort", Label: "Effort", Type: "enum", Values: []string{"low", "high"}, Scope: "model", RequestPath: "/reasoning/effort"}
	store, root := runtimeControlTestStore(t, "openai-responses", []manifest.RuntimeControl{control})
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.analysis_effort", "low"))
	target := runtimeControlTestTarget(t, store, control.ID)
	target.Selection.ModelType = SelectedModelTypeSmall
	selected := cloneSelectedModel(store.Config().Models[SelectedModelTypeSmall])
	_, err := store.OverrideModelsForOwners(AgentModelState{Small: &OwnedSelectedModel{Owner: target.Owner, Model: selected}})
	require.NoError(t, err)
	before := remoteBaselineTree(t, root)
	_, err = store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte(`"low"`))
	require.Error(t, err, "same-value constructor fallback cannot acknowledge an exact explicit set")
	require.Equal(t, before, remoteBaselineTree(t, root))
	state, err := store.RuntimeControlState(t.Context(), ScopeWorkspace, target)
	require.NoError(t, err)
	require.True(t, state.RuntimeDependent)
	require.False(t, state.Scoped.Present)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"low"`)))
}

func TestRuntimeControlCanonicalKeyCannotBeConsumedByEarlierAlias(t *testing.T) {
	for _, binding := range []struct {
		scope    string
		protocol string
	}{{"model", "generic-json"}, {"provider", "openai-responses"}} {
		t.Run(binding.scope, func(t *testing.T) {
			controls := []manifest.RuntimeControl{
				{ID: "vendor.count", Label: "Vendor count", Type: "integer", Default: 0, Scope: binding.scope, RequestPath: "/vendor/count"},
				{ID: "count", Label: "Count", Type: "integer", Default: 1, Scope: binding.scope, RequestPath: "/count"},
			}
			store, root := runtimeControlTestStore(t, binding.protocol, controls)
			target := runtimeControlTestTarget(t, store, "count")
			before := store.Config()
			files := remoteBaselineTree(t, root)
			_, err := store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte("1"))
			require.ErrorContains(t, err, "canonical key")
			require.Same(t, before, store.Config())
			require.Equal(t, files, remoteBaselineTree(t, root))
			first, err := store.RuntimeControlState(t.Context(), ScopeWorkspace, RuntimeControlTarget{Owner: target.Owner, ControlID: "vendor.count", Selection: target.Selection})
			require.NoError(t, err)
			require.True(t, RuntimeControlJSONEqual(first.Effective.Value, []byte("0")))
			// An earlier canonical key in the provider layer prevents it from
			// consuming the later control's selected-model key as an alias.
			require.NoError(t, store.SetConfigField(ScopeGlobal, "providers.example-echo.provider_options", map[string]any{"vendor.count": 7}))
			state, err := store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte("1"))
			require.NoError(t, err)
			require.True(t, RuntimeControlJSONEqual(state.Scoped.Value, []byte("1")))
			require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte("1")))
			_, options := runtimeControlMergedOptions(store.Config(), target)
			_, byPath, keys := providerregistry.ResolveRuntimeControlOptions(controls, options)
			require.Equal(t, "vendor.count", keys["vendor.count"])
			require.Equal(t, "count", keys["count"])
			for path, expected := range map[string]string{"/vendor/count": "7", "/count": "1"} {
				raw, err := json.Marshal(byPath[path])
				require.NoError(t, err)
				require.True(t, RuntimeControlJSONEqual(raw, []byte(expected)))
			}
		})
	}
}

func TestRuntimeControlProviderScopeAndAliasPrecedence(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.mode", Label: "Mode", Type: "string", Scope: "provider", RequestPath: "/vendor/mode", Default: "default"}
	store, _ := runtimeControlTestStore(t, "openai-responses", []manifest.RuntimeControl{control})
	target := runtimeControlTestTarget(t, store, control.ID)
	state, err := store.SetRuntimeControl(t.Context(), ScopeGlobal, target, []byte(`"provider"`))
	require.NoError(t, err)
	require.Equal(t, "provider", state.Source.Kind)
	selected := cloneSelectedModel(store.Config().Models[SelectedModelTypeLarge])
	selected.ProviderOptions["mode"] = "selected-alias"
	_, err = store.OverrideModelsForOwners(AgentModelState{Large: &OwnedSelectedModel{Owner: target.Owner, Model: selected}})
	require.NoError(t, err)
	state, err = store.RuntimeControlState(t.Context(), ScopeGlobal, target)
	require.NoError(t, err)
	require.Equal(t, "provider", state.Source.Kind, "lower canonical key precedes higher suffix alias after merging")
	state, err = store.RemoveRuntimeControl(t.Context(), ScopeGlobal, target)
	require.NoError(t, err)
	require.Equal(t, "run-override", state.Source.Kind)
	require.Equal(t, "mode", state.Source.Key)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"selected-alias"`)))
}

func TestRuntimeControlDetachedAndCanceledStoreHasNoFilesystemEffects(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	store := &ConfigStore{clientRuntime: &clientRuntimeState{}, config: &Config{}}
	for _, operation := range []func() error{
		func() error {
			_, err := store.RuntimeControlState(t.Context(), ScopeGlobal, RuntimeControlTarget{})
			return err
		},
		func() error {
			_, err := store.SetRuntimeControl(t.Context(), ScopeGlobal, RuntimeControlTarget{}, []byte("false"))
			return err
		},
		func() error {
			_, err := store.RemoveRuntimeControl(t.Context(), ScopeGlobal, RuntimeControlTarget{})
			return err
		},
	} {
		require.ErrorIs(t, operation(), ErrClientRuntimeManaged)
	}
	store.clientRuntime = nil
	ctx, cancel := context.WithCancel(t.Context())
	store.writeMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := store.SetRuntimeControl(ctx, ScopeGlobal, RuntimeControlTarget{}, []byte("false"))
		done <- err
	}()
	cancel()
	store.writeMu.Unlock()
	require.ErrorIs(t, <-done, context.Canceled)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRuntimeControlCodexEffectiveAndSurface(t *testing.T) {
	var registration providerregistry.Registration
	for _, candidate := range registrytest.Registrations() {
		if candidate.ProviderID == "codex" {
			registration = candidate
		}
	}
	require.Equal(t, "codex", registration.ProviderID)
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	model := catalog.Model{ID: "gpt-5.6", CanReason: true, ReasoningLevels: []string{"low", "high"}, DefaultReasoningEffort: "low"}
	cfg := &Config{Options: &Options{}, Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"codex": {ID: "codex", Plugin: &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}, Owner: providerOwnerReferenceForRegistration(registration), Models: []catalog.Model{model}},
	}), Models: map[SelectedModelType]SelectedModel{
		SelectedModelTypeLarge: {Provider: "codex", Model: model.ID, ReasoningEffort: "high"},
		SelectedModelTypeSmall: {Provider: "codex", Model: model.ID},
	}}
	cfg.bindProviderScan(ProviderScan{Registry: registry})
	target := RuntimeControlTarget{Owner: registration.Owner(), ControlID: "options.analysis_effort", Selection: RuntimeControlSelection{ModelType: SelectedModelTypeLarge, ModelID: model.ID}}
	state, err := cfg.RuntimeControlEffectiveState(ScopeGlobal, target)
	require.NoError(t, err)
	require.False(t, state.ScopedKnown)
	require.Equal(t, "model", state.Source.Kind)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"high"`)))
	cfg.Options.AnalysisEffort = "low"
	state, err = cfg.RuntimeControlEffectiveState(ScopeGlobal, target)
	require.NoError(t, err)
	require.Equal(t, "host-option", state.Source.Kind)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, []byte(`"low"`)))
	surfaces := ProviderSurfaces(cfg)
	var surface providerregistry.Surface
	for _, candidate := range surfaces {
		if candidate.ID == "codex" {
			surface = candidate
		}
	}
	encoded, err := json.Marshal(cfg.RedactedForTransport())
	require.NoError(t, err)
	var public Config
	require.NoError(t, json.Unmarshal(encoded, &public))
	require.NoError(t, public.BindProviderSurfaceOwners(surfaces))
	fromSurface, err := RuntimeControlEffectiveStateForSurface(&public, ScopeGlobal, state.Target, surface)
	require.NoError(t, err)
	require.Equal(t, state.Effective, fromSurface.Effective)
	require.Equal(t, state.Source, fromSurface.Source)
	target.Selection.ModelType = SelectedModelTypeSmall
	_, err = cfg.RuntimeControlEffectiveState(ScopeGlobal, target)
	require.ErrorContains(t, err, "only to the selected large")
}

func TestRuntimeControlSurfaceUsesExactSmallModelAvailability(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.mode", Label: "Mode", Type: "string", Scope: "model", RequestPath: "/vendor/mode", Default: "default"}
	store, _ := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
	cfg := store.Config().cloneForWrite()
	registration, ok := cfg.ProviderRegistration("example-echo")
	require.True(t, ok)
	runtime := *registration.Runtime
	runtime.Available = func(modelID string) bool { return modelID == "echo-1" }
	registration.Runtime = &runtime
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	scan := *cfg.providerScan
	scan.Registry = registry
	cfg.bindProviderScan(scan)
	provider, ok := cfg.Providers.Get("example-echo")
	require.True(t, ok)
	unsupported := provider.Models[0]
	unsupported.ID = "unavailable-main"
	provider.Models = append(provider.Models, unsupported)
	cfg.Providers.Set("example-echo", provider)
	large := cfg.Models[SelectedModelTypeLarge]
	large.Model = unsupported.ID
	cfg.Models[SelectedModelTypeLarge] = large
	target := RuntimeControlTarget{Owner: registration.Owner(), ControlID: control.ID, Selection: RuntimeControlSelection{ModelType: SelectedModelTypeSmall, ModelID: "echo-1"}}
	accepted, err := cfg.RuntimeControlEffectiveState(ScopeWorkspace, target)
	require.NoError(t, err)
	surfaces := ProviderSurfaces(cfg)
	surface, ok := providerregistry.LookupSurface(surfaces, target.Owner.ProviderID)
	require.True(t, ok)
	require.False(t, surface.RuntimeControls[0].Available, "main-model display availability remains false")
	require.Equal(t, []string{"echo-1"}, surface.RuntimeControls[0].AvailableModels)
	encoded, err := json.Marshal(cfg.RedactedForTransport())
	require.NoError(t, err)
	var public Config
	require.NoError(t, json.Unmarshal(encoded, &public))
	require.NoError(t, public.BindProviderSurfaceOwners(surfaces))
	actual, err := RuntimeControlEffectiveStateForSurface(&public, ScopeWorkspace, accepted.Target, surface)
	require.NoError(t, err)
	require.Equal(t, accepted.Effective, actual.Effective)
	unsupportedTarget := target
	unsupportedTarget.Selection = RuntimeControlSelection{ModelType: SelectedModelTypeLarge, ModelID: unsupported.ID}
	_, err = RuntimeControlEffectiveStateForSurface(&public, ScopeWorkspace, unsupportedTarget, surface)
	require.ErrorContains(t, err, "unavailable")
	surface.RuntimeControls[0].Available = true
	surface.RuntimeControls[0].AvailableModels = nil
	_, err = RuntimeControlEffectiveStateForSurface(&public, ScopeWorkspace, target, surface)
	require.ErrorContains(t, err, "unavailable", "legacy availability and catalog membership cannot authorize an exact model")
}

func TestRuntimeControlSameOwnerDescriptorAndPrecisionFence(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.limit", Label: "Limit", Type: "integer", Scope: "model", RequestPath: "/vendor/limit", Default: 1}
	store, root := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
	target := runtimeControlTestTarget(t, store, control.ID)
	files := remoteBaselineTree(t, root)
	_, err := store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte("9007199254740993"))
	require.Error(t, err, "config loading must not silently round an explicit integer")
	require.Equal(t, files, remoteBaselineTree(t, root))
	registration, ok := store.Config().ProviderRegistration(target.Owner.ProviderID)
	require.True(t, ok)
	registration.RuntimeControls[0].Default = 2
	registration.Manifest.Capabilities.RuntimeControls = registration.RuntimeControls
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	next := store.Config().cloneForWrite()
	scan := *next.providerScan
	scan.Registry = registry
	next.bindProviderScan(scan)
	store.setConfig(next)
	store.providerRegistry = registry
	_, err = store.SetRuntimeControl(t.Context(), ScopeWorkspace, target, []byte("3"))
	require.ErrorContains(t, err, "descriptor changed")
	require.Equal(t, files, remoteBaselineTree(t, root))
}

func TestRuntimeControlFileCASPreservesConcurrentEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crux.json")
	store := &ConfigStore{globalDataPath: path}
	require.NoError(t, os.WriteFile(path, []byte(`{"concurrent":true}`), 0o600))
	err := store.writeRuntimeControlFile(t.Context(), ScopeGlobal, []byte(`{}`), false, []byte(`{"replacement":true}`))
	require.ErrorContains(t, err, "changed before persistence")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.JSONEq(t, `{"concurrent":true}`, string(data))
}

func TestRuntimeControlJSONEqualExactNumbers(t *testing.T) {
	for _, values := range [][2]string{{"0", "-0.00"}, {"1e2", "100.0"}, {"1e100000000000000000", "10e99999999999999999"}, {`{"a":[1,true,null]}`, `{"a":[1.0,true,null]}`}} {
		require.True(t, RuntimeControlJSONEqual([]byte(values[0]), []byte(values[1])), values)
	}
	for _, values := range [][2]string{{"9007199254740992", "9007199254740993"}, {"false", "0"}, {"null", ""}, {"1 2", "1"}} {
		require.False(t, RuntimeControlJSONEqual([]byte(values[0]), []byte(values[1])), values)
	}
}
