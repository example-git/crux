package providerplugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestImagePluginErrorsRetainSafeCauses(t *testing.T) {
	cause := errors.New("registry read failed: access_token=synthetic-secret")
	wrapped := &imagePluginCauseError{message: "image plugin ownership could not be revalidated", cause: cause}
	require.ErrorIs(t, wrapped, cause)
	require.ErrorContains(t, wrapped, "registry read failed")
	require.NotContains(t, wrapped.Error(), "synthetic-secret")
	diagnostic := &DiagnosticError{Report: DiagnosticReport{Diagnostics: []Diagnostic{{Code: "invalid-field", Path: "/generate", Message: "workflow is unavailable; access_token=synthetic-secret"}}}}
	require.ErrorContains(t, diagnostic, "/generate: workflow is unavailable")
	require.NotContains(t, diagnostic.Error(), "synthetic-secret")
	manager := newTestManager(t)
	_, err := manager.InspectImageSource(t.Context(), filepath.Join(t.TempDir(), "missing.plugin"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorContains(t, err, "existing readable local bundle directory")
}

func TestImageManagerInstallTrustAndNamespace(t *testing.T) {
	manager := newTestManager(t)
	chat := readExampleManifest(t)
	chatSource := filepath.Join(t.TempDir(), "chat.plugin")
	writeBundleManifest(t, chatSource, chat)
	_, err := manager.Install(t.Context(), InstallRequest{Source: chatSource, Trust: true})
	require.NoError(t, err)
	value := manifest.ImageManifest{
		PluginType: manifest.PluginTypeImageProvider, ManifestVersion: 1, ID: "example.images", Version: "1.0.0", Name: "Example images", Description: "Synthetic image contract",
		Publisher: manifest.Publisher{ID: "example", Name: "Example"}, Compatibility: manifest.Compatibility{HostAPI: manifest.VersionBounds{Min: 1, Max: 1}},
		Backend: chat.Provider.ID, Configuration: manifest.Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false}},
		Origins: []manifest.ImageOrigin{{URL: "https://images.example.test"}},
		Models:  []manifest.ImageModel{{ID: "model", Name: "Model", Parameters: map[string]any{"code": json.Number("9007199254740993")}}}, DefaultModel: "model",
		Options:  manifest.ImageOptions{Quality: []string{"auto"}, Background: []string{"auto"}, Sizes: []string{"auto"}, OutputExtension: ".png"},
		Limits:   manifest.ImageLimits{Concurrency: 1, Variants: 1, InputImages: 1, InputBytes: 1024, TotalInputBytes: 1024, OutputBytes: 1024, ResponseBytes: 4096, TimeoutSeconds: 10},
		Generate: "generate", VariantMode: "individual",
		Workflows: map[string]manifest.ImageWorkflow{"generate": {
			Steps:  []manifest.ImageStep{{ID: "value", Value: &manifest.ImageValue{Object: map[string]manifest.ImageValue{}}}},
			Result: manifest.ImageValue{Array: []manifest.ImageValue{}},
		}},
	}
	data, err := json.Marshal(value)
	require.NoError(t, err)
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, manifestFilename), data, 0o600))
	snapshot, err := manager.Install(t.Context(), InstallRequest{Source: source})
	require.NoError(t, err)
	var imageStatus Status
	for _, status := range snapshot.Plugins {
		if status.ID == value.ID {
			imageStatus = status
		}
	}
	require.Equal(t, StateUntrusted, imageStatus.State)
	images, err := manager.RegisteredImageBundles()
	require.NoError(t, err)
	require.Empty(t, images)
	_, err = manager.SetTrust(t.Context(), value.ID, TrustRequest{Digest: imageStatus.Digest, Trusted: true})
	require.NoError(t, err)
	images, err = manager.RegisteredImageBundles()
	require.NoError(t, err)
	require.Len(t, images, 1)
	owner, err := manager.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	require.Equal(t, images[0].Owner(), owner)
	require.NoError(t, manager.ValidateImageOwner(t.Context(), owner))
	t.Run("concurrent revalidation", func(t *testing.T) {
		manager.mu.Lock()
		var unlock sync.Once
		defer unlock.Do(manager.mu.Unlock)
		results := make(chan error, 5)
		go func() { results <- manager.ValidateImageOwner(t.Context(), owner) }()
		require.Eventually(t, func() bool {
			return len(manager.rescanGate) == 1
		}, time.Second, time.Millisecond)
		for range 4 {
			go func() { results <- manager.ValidateImageOwner(t.Context(), owner) }()
		}
		cancelCtx, cancelWait := context.WithCancel(t.Context())
		cancelWait()
		require.ErrorIs(t, manager.ValidateImageOwner(cancelCtx, owner), context.Canceled)
		time.Sleep(managerLockTimeout + 200*time.Millisecond)
		unlock.Do(manager.mu.Unlock)
		for range 5 {
			require.NoError(t, <-results)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, manager.ValidateImageOwner(ctx, owner), context.Canceled)
	})
	for _, changed := range []ImageOwner{
		{},
		{Backend: "other", PluginID: owner.PluginID, Version: owner.Version, Digest: owner.Digest},
		{Backend: owner.Backend, PluginID: "other", Version: owner.Version, Digest: owner.Digest},
		{Backend: owner.Backend, PluginID: owner.PluginID, Version: "2.0.0", Digest: owner.Digest},
		{Backend: owner.Backend, PluginID: owner.PluginID, Version: owner.Version, Digest: "different"},
	} {
		_, err := manager.ImageBundleForOwner(changed)
		require.Error(t, err)
	}
	require.Len(t, manager.RegisteredBundles(), 1)
	require.Equal(t, imageStatus.Digest, images[0].Digest)
	require.Equal(t, json.Number("9007199254740993"), images[0].Manifest.Models[0].Parameters["code"])
	require.NotNil(t, images[0].Manifest.Workflows["generate"].Result.Array)
	require.NotNil(t, images[0].Manifest.Workflows["generate"].Steps[0].Value.Object)
	images[0].Manifest.Models[0].Parameters["code"] = "changed"
	images[0].Manifest.Workflows["generate"].Steps[0].Value.Object["changed"] = manifest.ImageValue{Ref: "/request/prompt"}
	fresh, err := manager.RegisteredImageBundles()
	require.NoError(t, err)
	require.Equal(t, json.Number("9007199254740993"), fresh[0].Manifest.Models[0].Parameters["code"])
	require.Empty(t, fresh[0].Manifest.Workflows["generate"].Steps[0].Value.Object)
	installed := filepath.Join(manager.paths.Bundles, imageStatus.BundleName, manifestFilename)
	changed := value
	changed.Description = "Changed installed bytes"
	changedData, err := json.Marshal(changed)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(installed, changedData, 0o600))
	require.Error(t, manager.ValidateImageOwner(t.Context(), owner))
	require.NoError(t, os.WriteFile(installed, data, 0o600))
	require.NoError(t, manager.ValidateImageOwner(t.Context(), owner))
	_, err = manager.SetTrust(t.Context(), value.ID, TrustRequest{Digest: imageStatus.Digest, Trusted: false})
	require.NoError(t, err)
	images, err = manager.RegisteredImageBundles()
	require.NoError(t, err)
	require.Empty(t, images)
	require.Error(t, manager.ValidateImageOwner(t.Context(), owner))
	require.Len(t, manager.RegisteredBundles(), 1)
}

func TestInstallRejectsChangedDigestConsent(t *testing.T) {
	manager := newTestManager(t)
	value := readExampleManifest(t)
	source := filepath.Join(t.TempDir(), "source.plugin")
	writeBundleManifest(t, source, value)
	report, err := manager.Diagnose(t.Context(), DiagnoseRequest{Source: source})
	require.NoError(t, err)
	require.True(t, report.Valid)
	value.Description = "Changed after consent"
	writeBundleManifest(t, source, value)
	_, err = manager.Install(t.Context(), InstallRequest{Source: source, Trust: true, ExpectedDigest: report.Digest})
	require.ErrorContains(t, err, "exact-digest consent")
	_, err = os.Stat(filepath.Join(manager.paths.Bundles, value.ID+bundleSuffix))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(manager.paths.ProvenanceFile)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Empty(t, manager.RegisteredBundles())
	report, err = manager.Diagnose(t.Context(), DiagnoseRequest{Source: source})
	require.NoError(t, err)
	_, err = manager.Install(t.Context(), InstallRequest{Source: source, Trust: true, ExpectedDigest: report.Digest})
	require.NoError(t, err)
	require.Len(t, manager.RegisteredBundles(), 1)
}
