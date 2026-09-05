package imagegen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestPersistentImageUploadsSurviveRuntimeRecreation(t *testing.T) {
	service, source := imageSetupFixture(t)
	bundle, err := service.Runtime.Manager.InspectImageSource(t.Context(), source)
	require.NoError(t, err)
	var uploads, lookups atomic.Int64
	var available atomic.Bool
	available.Store(true)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/upload":
			data, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, "original-image", string(data))
			uploads.Add(1)
			_, _ = io.WriteString(w, `{"id":"opaque-media-id"}`)
		case "/lookup":
			lookups.Add(1)
			require.Equal(t, "opaque-media-id", r.URL.Query().Get("id"))
			require.NoError(t, json.NewEncoder(w).Encode(map[string]bool{"available": available.Load()}))
		case "/generate":
			require.Equal(t, "opaque-media-id", r.URL.Query().Get("id"))
			_, _ = io.WriteString(w, `{"images":["eA=="]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	literal := func(value string) manifest.ImageValue {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		return manifest.ImageValue{Literal: data}
	}
	workflow := func(path, result, phase string, query map[string]manifest.ImageValue) manifest.ImageWorkflow {
		request := manifest.ImageRequest{Method: "GET", URL: literal(server.URL + path), Encoding: "none", Response: "json", Phase: phase, Query: query, MaxBytes: 4096, TimeoutSeconds: 5}
		if path == "/upload" {
			request.Method = "POST"
			request.Encoding = "binary"
			request.Body = &manifest.ImageValue{Ref: "/input/data"}
		}
		return manifest.ImageWorkflow{Steps: []manifest.ImageStep{{ID: "send", Request: &request}}, Result: manifest.ImageValue{Ref: "/steps/send/body/" + result}}
	}
	value := bundle.Manifest
	value.Origins = []manifest.ImageOrigin{{URL: server.URL}}
	value.Edit = value.Generate
	value.Upload = &manifest.ImageUpload{Workflow: "upload", Lookup: "lookup", CacheKey: &manifest.ImageValue{Ref: "/input/sha256"}, Persistent: true}
	value.Workflows["upload"] = workflow("/upload", "id", "upload", nil)
	value.Workflows["lookup"] = workflow("/lookup", "available", "media", map[string]manifest.ImageValue{"id": {Ref: "/upload"}})
	value.Workflows[value.Generate] = workflow("/generate", "images", "generation", map[string]manifest.ImageValue{"id": {Ref: "/uploads/0"}})
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	_, err = service.Runtime.Manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	owner, err := service.Runtime.Manager.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	directory := t.TempDir()
	for index := range 3 {
		if index == 2 {
			available.Store(false)
		}
		runtime := &PluginRuntime{Manager: service.Runtime.Manager, Client: server.Client(), UploadDirectory: directory}
		response, err := runtime.Execute(t.Context(), owner, JobRequest{Mode: ModeEdit, Prompt: "edit", Count: 1}, []EditImage{{Filename: "original.png", MIMEType: "image/png", Data: []byte("original-image")}})
		require.NoError(t, err)
		require.Len(t, response.Data, 1)
	}
	require.EqualValues(t, 2, uploads.Load())
	require.EqualValues(t, 2, lookups.Load())
}

func TestImageUploadCacheRejectsMalformedReferences(t *testing.T) {
	owner := providerplugin.ImageOwner{Backend: "synthetic", PluginID: "synthetic.images", Version: "1.0.0", Digest: "digest"}
	key := strings.Repeat("a", 64)
	for _, test := range []struct {
		name        string
		key         string
		credentials []string
	}{
		{name: "short key", key: "abc"},
		{name: "uppercase key", key: strings.ToUpper(key)},
		{name: "empty credential", key: key, credentials: []string{""}},
		{name: "oversized credential", key: key, credentials: []string{strings.Repeat("a", 65)}},
		{name: "invalid credential", key: key, credentials: []string{"secret/value"}},
		{name: "duplicate credential", key: key, credentials: []string{"access", "access"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			cache, err := openImageUploadCache(t.Context(), directory, owner, [32]byte{}, func() error { return nil })
			require.NoError(t, err)
			defer cache.release()
			value := providertransport.ImageUploadReference{Identifier: "opaque-id", Credentials: test.credentials}
			require.Error(t, cache.save(test.key, value))
			require.Empty(t, cache.entries)
			require.NoFileExists(t, cache.path)
			data, err := json.Marshal(map[string]providertransport.ImageUploadReference{test.key: value})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(cache.path, data, 0o600))
			require.Error(t, cache.load())
		})
	}
}

func TestImageUploadCacheRevalidatesAndIsolates(t *testing.T) {
	directory := t.TempDir()
	owner := providerplugin.ImageOwner{Backend: "synthetic", PluginID: "synthetic.images", Version: "1.0.0", Digest: "digest"}
	identity := sha256.Sum256([]byte("session"))
	keyHash := sha256.Sum256([]byte("input"))
	key := hex.EncodeToString(keyHash[:])
	valid := func() error { return nil }
	cache, err := openImageUploadCache(t.Context(), directory, owner, identity, valid)
	require.NoError(t, err)
	require.NoError(t, cache.save(key, providertransport.ImageUploadReference{Identifier: "opaque-id"}))
	before, err := os.ReadFile(cache.path)
	require.NoError(t, err)
	calls := 0
	changed := errors.New("owner changed")
	cache.validate = func() error {
		calls++
		if calls == 2 {
			return changed
		}
		return nil
	}
	require.ErrorIs(t, cache.save(key, providertransport.ImageUploadReference{Identifier: "replacement"}), changed)
	after, err := os.ReadFile(cache.path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, "opaque-id", cache.entries[key].Identifier)
	cache.release()
	for _, change := range []string{"none", "owner", "session"} {
		other := owner
		otherIdentity := identity
		if change == "owner" {
			other.Digest = "replacement"
		}
		if change == "session" {
			otherIdentity[0]++
		}
		loaded, err := openImageUploadCache(t.Context(), directory, other, otherIdentity, valid)
		require.NoError(t, err)
		if change == "none" {
			require.Equal(t, "opaque-id", loaded.entries[key].Identifier)
		} else {
			require.Empty(t, loaded.entries)
		}
		loaded.release()
	}
	redact.Register("synthetic-cookie-secret")
	for _, value := range []string{"synthetic-cookie-secret", "https://example.test/image?token=value", "contains space", ""} {
		require.False(t, validUploadIdentifier(value))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = openImageUploadCache(ctx, directory, owner, identity, func() error { return ctx.Err() })
	require.ErrorIs(t, err, context.Canceled)
}
