package imagegen

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/cookieutil"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestHostImageBrowserAuthenticationSurvivesBrowserRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	t.Setenv("LOCALAPPDATA", home)
	var profileRoot string
	switch runtime.GOOS {
	case "darwin":
		profileRoot = filepath.Join(home, "Library", "Application Support", "Firefox", "Profiles", "synthetic")
	case "linux":
		profileRoot = filepath.Join(home, ".mozilla", "firefox", "synthetic")
	case "windows":
		profileRoot = filepath.Join(home, "Mozilla", "Firefox", "Profiles", "synthetic")
	default:
		t.Skip("browser discovery is unavailable")
	}
	require.NoError(t, os.MkdirAll(profileRoot, 0o700))
	cookiePath := filepath.Join(profileRoot, "cookies.sqlite")
	database, err := sql.Open("sqlite", cookiePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	_, err = database.ExecContext(t.Context(), `CREATE TABLE moz_cookies (host TEXT, path TEXT, isSecure INTEGER, expiry INTEGER, name TEXT, value TEXT, isHttpOnly INTEGER)`)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `INSERT INTO moz_cookies VALUES ('images.example.test', '/', 1, 0, 'session', 'synthetic-browser-secret', 1)`)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	service, source := imageSetupFixture(t)
	profiles := cookieutil.BrowserProfiles(service.Store.HostEnvironment())
	require.Len(t, profiles, 1)
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	var value manifest.ImageManifest
	require.NoError(t, json.Unmarshal(data, &value))
	value.Credentials = []manifest.ImageCredential{{ID: "browser", Source: "browser", Domains: []string{"images.example.test"}}}
	value.Origins[0].Credentials = []string{"browser"}
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	_, err = service.Runtime.Manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	owner, err := service.Runtime.Manager.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	require.NoError(t, service.Store.CompareAndSetImageConfiguration(nil, &config.ImageConfiguration{Preferred: []providerplugin.ImageOwner{owner}, Providers: map[string]config.ImageProviderConfiguration{owner.Backend: {Owner: owner, BrowserProfiles: map[string]string{"browser": profiles[0].ID}}}}))
	require.NoError(t, service.Authenticate(t.Context(), SetupRequest{}, owner))
	bundle, err := service.Runtime.Manager.ImageBundleForOwner(owner)
	require.NoError(t, err)
	first, err := service.Runtime.ResolveCredentials(t.Context(), bundle)
	require.NoError(t, err)
	address, err := url.Parse("https://images.example.test/")
	require.NoError(t, err)
	require.Equal(t, "synthetic-browser-secret", first.CookieJars["browser"].Cookies(address)[0].Value)
	first.CookieJars["browser"].SetCookies(address, []*http.Cookie{{Name: "session", Value: "synthetic-rotated-secret", Secure: true}})
	require.NoError(t, os.Remove(cookiePath))
	require.Empty(t, cookieutil.BrowserProfiles(service.Store.HostEnvironment()))
	require.NoError(t, service.Authenticate(t.Context(), SetupRequest{}, owner))
	second, err := service.Runtime.ResolveCredentials(t.Context(), bundle)
	require.NoError(t, err)
	require.Same(t, first.CookieJars["browser"], second.CookieJars["browser"])
	require.Equal(t, first.Identity, second.Identity)
	require.Equal(t, "synthetic-rotated-secret", second.CookieJars["browser"].Cookies(address)[0].Value)
	stored, err := json.Marshal(service.Store.ImageConfiguration())
	require.NoError(t, err)
	require.NotContains(t, string(stored), "synthetic-browser-secret")
	require.NotContains(t, string(stored), "synthetic-rotated-secret")
	fresh, err := NewHostPluginRuntime(t.Context(), service.Store, PluginCredentialBindings{})
	require.NoError(t, err)
	t.Cleanup(fresh.Manager.Close)
	_, err = fresh.ResolveCredentials(t.Context(), bundle)
	require.ErrorContains(t, err, "browser profile is unavailable")
}
