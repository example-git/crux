package imagegen

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestImageSessionIdentityTracksConfigurationAndCredentials(t *testing.T) {
	value := manifest.ImageManifest{Configuration: manifest.Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"project": map[string]any{"type": "string"}}}}}
	configuration := map[string]any{"project": "one"}
	detached, first, err := imageConfiguration(value, configuration)
	require.NoError(t, err)
	configuration["project"] = "two"
	require.Equal(t, "one", detached["project"])
	_, second, err := imageConfiguration(value, configuration)
	require.NoError(t, err)
	credentials := PluginCredentials{Identity: "account-one", Values: map[string]any{"access": "token-one"}}
	identity, err := imageSessionIdentity(first, credentials)
	require.NoError(t, err)
	changed, err := imageSessionIdentity(second, credentials)
	require.NoError(t, err)
	require.NotEqual(t, identity, changed)
	credentials.Values["access"] = "token-two"
	changed, err = imageSessionIdentity(first, credentials)
	require.NoError(t, err)
	require.NotEqual(t, identity, changed)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	_, err = imageSessionIdentity(first, PluginCredentials{CookieJars: map[string]http.CookieJar{"browser": jar}})
	require.Error(t, err)
	_, _, err = imageConfiguration(value, map[string]any{"project": 3})
	require.Error(t, err)
	_, _, err = imageConfiguration(value, map[string]any{"unknown": "private-value"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-value")
}

func TestImageSessionInvalidationCannotEvictReplacement(t *testing.T) {
	runtime := &PluginRuntime{}
	owner := providerplugin.ImageOwner{Backend: "images", PluginID: "synthetic", Version: "1", Digest: "digest"}
	first := runtime.sessionFor(owner, [32]byte{1})
	require.Same(t, first, runtime.sessionFor(owner, [32]byte{1}))
	second := runtime.sessionFor(owner, [32]byte{2})
	require.NotSame(t, first, second)
	runtime.invalidateSession(owner, first)
	require.Same(t, second, runtime.sessionFor(owner, [32]byte{2}))
	runtime.invalidateSession(owner, second)
	require.NotSame(t, second, runtime.sessionFor(owner, [32]byte{2}))
}

func TestImageSessionBootstrapWaitIsCancelable(t *testing.T) {
	session := &imagePluginSession{}
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := session.bootstrap(t.Context(), func() (any, error) {
			close(started)
			<-release
			return "session", nil
		})
		finished <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := session.bootstrap(ctx, func() (any, error) { return nil, errors.New("unexpected second bootstrap") })
	close(release)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, <-finished)
	result, err := session.bootstrap(t.Context(), func() (any, error) { return nil, errors.New("unexpected repeat bootstrap") })
	require.NoError(t, err)
	require.Equal(t, "session", result)
}
