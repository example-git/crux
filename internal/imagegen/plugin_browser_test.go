package imagegen

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestImageBrowserAuthenticationStaysInMemory(t *testing.T) {
	address, err := url.Parse("https://images.example.test/")
	require.NoError(t, err)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	jar.SetCookies(address, []*http.Cookie{{Name: "session", Value: "initial"}})
	var imports atomic.Int64
	cache := imageBrowserCache{importCookies: func(context.Context, []string, manifest.ImageCredential, string) (http.CookieJar, error) {
		if imports.Add(1) != 1 {
			return nil, errors.New("browser is closed")
		}
		return jar, nil
	}}
	owner := providerplugin.ImageOwner{Backend: "fixture", PluginID: "fixture.images", Version: "1", Digest: "first"}
	credential := manifest.ImageCredential{ID: "browser", Source: "browser", Domains: []string{"images.example.test"}}
	first, identity, err := cache.resolve(t.Context(), nil, owner, credential, "profile", nil)
	require.NoError(t, err)
	first.SetCookies(address, []*http.Cookie{{Name: "session", Value: "rotated"}})
	second, secondIdentity, err := cache.resolve(t.Context(), nil, owner, credential, "profile", nil)
	require.NoError(t, err)
	require.Same(t, first, second)
	require.Equal(t, identity, secondIdentity)
	require.Equal(t, "rotated", second.Cookies(address)[0].Value)
	require.EqualValues(t, 1, imports.Load())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = cache.resolve(ctx, nil, owner, credential, "profile", nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestImageBrowserAuthenticationBindingAndFailedImport(t *testing.T) {
	var imports int
	fail := false
	cache := imageBrowserCache{importCookies: func(context.Context, []string, manifest.ImageCredential, string) (http.CookieJar, error) {
		imports++
		if fail {
			return nil, errors.New("browser unavailable")
		}
		return cookiejar.New(nil)
	}}
	owner := providerplugin.ImageOwner{Backend: "fixture", PluginID: "fixture.images", Version: "1", Digest: "first"}
	credential := manifest.ImageCredential{ID: "browser", Source: "browser", Domains: []string{"images.example.test"}}
	first, _, err := cache.resolve(t.Context(), nil, owner, credential, "profile", nil)
	require.NoError(t, err)
	second, _, err := cache.resolve(t.Context(), nil, owner, credential, "other-profile", nil)
	require.NoError(t, err)
	require.NotSame(t, first, second)
	third, _, err := cache.resolve(t.Context(), nil, owner, credential, "other-profile", map[string]any{"project": "other"})
	require.NoError(t, err)
	require.NotSame(t, second, third)
	owner.Digest = "replacement"
	fourth, _, err := cache.resolve(t.Context(), nil, owner, credential, "other-profile", map[string]any{"project": "other"})
	require.NoError(t, err)
	require.NotSame(t, third, fourth)
	fail = true
	_, _, err = cache.resolve(t.Context(), nil, owner, credential, "failed-profile", nil)
	require.ErrorContains(t, err, "browser unavailable")
	fail = false
	_, _, err = cache.resolve(t.Context(), nil, owner, credential, "failed-profile", nil)
	require.NoError(t, err)
	require.Equal(t, 6, imports)
}

func TestImageBrowserAuthenticationWaitCanBeCancelled(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var imports atomic.Int64
	cache := imageBrowserCache{importCookies: func(context.Context, []string, manifest.ImageCredential, string) (http.CookieJar, error) {
		imports.Add(1)
		close(started)
		<-release
		return cookiejar.New(nil)
	}}
	owner := providerplugin.ImageOwner{Backend: "fixture"}
	credential := manifest.ImageCredential{ID: "browser", Source: "browser", Domains: []string{"images.example.test"}}
	done := make(chan error, 1)
	go func() {
		_, _, err := cache.resolve(t.Context(), nil, owner, credential, "profile", nil)
		done <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := cache.resolve(ctx, nil, owner, credential, "profile", nil)
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	require.NoError(t, <-done)
	_, _, err = cache.resolve(t.Context(), nil, owner, credential, "profile", nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, imports.Load())
}
