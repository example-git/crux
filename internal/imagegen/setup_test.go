package imagegen

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/question"
	"github.com/stretchr/testify/require"
)

type imageSetupQuestions struct {
	question.Service
	ask func(context.Context, question.Request) ([]question.Answer, error)
}

func (q imageSetupQuestions) Ask(ctx context.Context, request question.Request) ([]question.Answer, error) {
	return q.ask(ctx, request)
}

func imageSetupFixture(t *testing.T) (*SetupService, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "host"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv("CRUX_PROVIDER_PROFILE", "core-only")
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, config.SnapshotEnvironment())
	require.NoError(t, err)
	runtime, err := NewHostPluginRuntime(t.Context(), store, PluginCredentialBindings{})
	require.NoError(t, err)
	t.Cleanup(runtime.Manager.Close)
	value := manifest.ImageManifest{
		PluginType: manifest.PluginTypeImageProvider, ManifestVersion: 1, ID: "fixture.setup.images", Version: "1.0.0", Name: "Fixture images", Description: "Setup contract",
		Publisher: manifest.Publisher{ID: "fixture", Name: "Fixture"}, Compatibility: manifest.Compatibility{HostAPI: manifest.VersionBounds{Min: 1, Max: 1}},
		Backend: "fixture-setup-images", Configuration: manifest.Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false}},
		Origins: []manifest.ImageOrigin{{URL: "https://images.example.test"}}, Models: []manifest.ImageModel{{ID: "model", Name: "Model"}}, DefaultModel: "model",
		Options: manifest.ImageOptions{Quality: []string{"auto"}, Background: []string{"auto"}, Sizes: []string{"auto"}, OutputExtension: ".png"},
		Limits:  manifest.ImageLimits{Concurrency: 1, Variants: 1, InputImages: 1, InputBytes: 1024, TotalInputBytes: 1024, OutputBytes: 1024, ResponseBytes: 4096, TimeoutSeconds: 30}, Generate: "generate", VariantMode: "individual",
		Workflows: map[string]manifest.ImageWorkflow{"generate": {Steps: []manifest.ImageStep{{ID: "send", Request: &manifest.ImageRequest{Method: "POST", URL: manifest.ImageValue{Literal: json.RawMessage(`"https://images.example.test"`)}, Encoding: "json", Body: &manifest.ImageValue{Object: map[string]manifest.ImageValue{}}, Response: "json", Phase: "generation", MaxBytes: 4096, TimeoutSeconds: 5}}}, Result: manifest.ImageValue{Ref: "/steps/send/body/images"}}},
	}
	data, err := json.Marshal(value)
	require.NoError(t, err)
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	return &SetupService{Runtime: runtime, Store: store}, source
}

func TestExplicitImageSetupRequiresExactConsent(t *testing.T) {
	service, source := imageSetupFixture(t)
	bundle, err := service.Runtime.Manager.InspectImageSource(t.Context(), source)
	require.NoError(t, err)
	require.ErrorContains(t, service.Install(t.Context(), source, "", ""), "consent")
	require.ErrorContains(t, service.Install(t.Context(), source, "mismatch", ""), "digest")
	bundles, err := service.Runtime.Manager.RegisteredImageBundles()
	require.NoError(t, err)
	require.Empty(t, bundles)
	require.NoError(t, service.Install(t.Context(), source, bundle.Digest, ""))
	require.Equal(t, bundle.Owner(), service.Store.ImageConfiguration().Preferred[0])
	require.ErrorContains(t, service.Install(t.Context(), source, bundle.Digest, ""), "replace")
}

func TestExplicitImageSetupAddsDistinctOwner(t *testing.T) {
	service, source := imageSetupFixture(t)
	first, err := service.Runtime.Manager.InspectImageSource(t.Context(), source)
	require.NoError(t, err)
	require.NoError(t, service.Install(t.Context(), source, first.Digest, ""))
	before := service.Store.ImageConfiguration()
	value := first.Manifest
	value.ID = "fixture.additional.images"
	value.Backend = "additional-images"
	data, err := json.Marshal(value)
	require.NoError(t, err)
	secondSource := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secondSource, "manifest.json"), data, 0o600))
	second, err := service.Runtime.Manager.InspectImageSource(t.Context(), secondSource)
	require.NoError(t, err)
	require.NoError(t, service.Install(t.Context(), secondSource, second.Digest, ""))
	after := service.Store.ImageConfiguration()
	require.Equal(t, []providerplugin.ImageOwner{first.Owner(), second.Owner()}, after.Preferred)
	require.Equal(t, before.Providers[first.Owner().Backend], after.Providers[first.Owner().Backend])
	require.Equal(t, second.Owner(), after.Providers[second.Owner().Backend].Owner)
	bundles, err := service.Runtime.Manager.RegisteredImageBundles()
	require.NoError(t, err)
	require.Len(t, bundles, 2)
	selected, err := service.Runtime.Select(t.Context())
	require.NoError(t, err)
	require.Equal(t, first.Owner(), selected)
	require.ErrorContains(t, service.Install(t.Context(), secondSource, second.Digest, ""), "replace")
	require.Equal(t, after, service.Store.ImageConfiguration())
	value.Version = "1.1.0"
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(secondSource, "manifest.json"), data, 0o600))
	updated, err := service.Runtime.Manager.InspectImageSource(t.Context(), secondSource)
	require.NoError(t, err)
	require.ErrorContains(t, service.Update(t.Context(), secondSource, second.Digest, ""), "digest")
	require.NoError(t, service.Update(t.Context(), secondSource, updated.Digest, ""))
	require.Equal(t, []providerplugin.ImageOwner{first.Owner(), updated.Owner()}, service.Store.ImageConfiguration().Preferred)
	require.Equal(t, before.Providers[first.Owner().Backend], service.Store.ImageConfiguration().Providers[first.Owner().Backend])
	require.Error(t, service.Runtime.Manager.ValidateImageOwner(t.Context(), second.Owner()))
	require.NoError(t, service.Runtime.Manager.ValidateImageOwner(t.Context(), updated.Owner()))
}

func TestImageSetupConsentAndActivation(t *testing.T) {
	for _, branch := range []string{"accept", "decline", "cancel", "decline-trust", "invalid-source", "changed-digest", "noninteractive"} {
		t.Run(branch, func(t *testing.T) {
			service, source := imageSetupFixture(t)
			calls := 0
			service.Questions = imageSetupQuestions{ask: func(ctx context.Context, request question.Request) ([]question.Answer, error) {
				calls++
				require.NoError(t, request.Validate())
				id := request.Questions[0].ID
				if branch == "cancel" {
					return nil, question.ErrCancelled
				}
				answer := question.Answer{QuestionID: id}
				yes := branch != "decline" && (branch != "decline-trust" || id != "image-trust")
				answer.Yes = &yes
				if id == "image-source" {
					answer.FillInText = source
					if branch == "invalid-source" {
						answer.FillInText = "relative-source"
					}
				}
				if id == "image-trust" {
					preview, err := service.Runtime.Manager.InspectImageSource(ctx, source)
					require.NoError(t, err)
					require.Contains(t, request.Questions[0].Description, preview.Digest)
					if branch == "changed-digest" {
						require.NoError(t, os.WriteFile(filepath.Join(source, "changed.txt"), []byte("changed after consent"), 0o600))
					}
				}
				return []question.Answer{answer}, nil
			}}
			err := service.Ensure(t.Context(), SetupRequest{SessionID: "parent", Interactive: branch != "noninteractive"})
			bundles, readErr := service.Runtime.Manager.RegisteredImageBundles()
			require.NoError(t, readErr)
			if branch == "accept" {
				require.NoError(t, err)
				require.Len(t, bundles, 1)
				require.Equal(t, bundles[0].Owner(), service.Store.ImageConfiguration().Preferred[0])
				selected, err := service.Runtime.Select(t.Context())
				require.NoError(t, err)
				require.Equal(t, bundles[0].Owner(), selected)
			} else {
				require.Error(t, err)
				require.Empty(t, bundles)
				require.Empty(t, service.Runtime.Manager.Snapshot().Plugins)
				require.Nil(t, service.Store.ImageConfiguration())
			}
			if branch == "noninteractive" {
				require.Zero(t, calls)
			}
		})
	}
}

type imageSetupWaitContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *imageSetupWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestImageSetupConcurrentWaiterCancellation(t *testing.T) {
	service, _ := imageSetupFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	service.Questions = imageSetupQuestions{ask: func(ctx context.Context, request question.Request) ([]question.Answer, error) {
		calls.Add(1)
		close(entered)
		<-release
		no := false
		return []question.Answer{{QuestionID: request.Questions[0].ID, Yes: &no}}, nil
	}}
	leader := make(chan error, 1)
	go func() { leader <- service.Ensure(t.Context(), SetupRequest{SessionID: "parent", Interactive: true}) }()
	<-entered
	require.ErrorIs(t, service.Ensure(t.Context(), SetupRequest{SessionID: "headless"}), ErrSetupRequired)
	waitContext := &imageSetupWaitContext{Context: t.Context(), joined: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- service.Ensure(waitContext, SetupRequest{SessionID: "waiter", Interactive: true}) }()
	<-waitContext.joined
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, service.Ensure(ctx, SetupRequest{SessionID: "other", Interactive: true}), context.Canceled)
	close(release)
	require.ErrorIs(t, <-leader, ErrSetupRequired)
	require.ErrorIs(t, <-waiter, ErrSetupRequired)
	require.EqualValues(t, 1, calls.Load())
}

func TestImageSetupPrivateConfigurationStrictDecode(t *testing.T) {
	for _, data := range []string{`{"unknown":"private"}`, `{} {}`, `{"owner":{"backend":"other"}}`} {
		path := filepath.Join(t.TempDir(), "configuration.json")
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
		_, err := readImageSetupConfiguration(path, providerplugin.ImageOwner{Backend: "fixture"})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "private")
	}
}
