package cookieutil

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestBrowserProfileUserAgentFromMetadata(t *testing.T) {
	for _, test := range []struct {
		name                                     string
		kind                                     browserProfileKind
		cookiePath, metadataPath, metadata, want string
	}{
		{"chromium", browserProfileChromium, "Chrome/Default/Cookies", "Chrome/Last Version", "144.0.7500.10", "Chrome/144.0.0.0"},
		{"chromium-network", browserProfileChromium, "Chrome/Profile 1/Network/Cookies", "Chrome/Last Version", "145.0.7600.1", "Chrome/145.0.0.0"},
		{"firefox", browserProfileFirefox, "Firefox/synthetic/cookies.sqlite", "Firefox/synthetic/compatibility.ini", "[Compatibility]\nLastVersion=144.0.2_20260905/20260905\n", "Firefox/144.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			metadata := filepath.Join(root, test.metadataPath)
			require.NoError(t, os.MkdirAll(filepath.Dir(metadata), 0o700))
			require.NoError(t, os.WriteFile(metadata, []byte(test.metadata), 0o600))
			profile := BrowserProfile{ID: "synthetic", profile: browserProfile{kind: test.kind, cookiesPath: filepath.Join(root, test.cookiePath)}}
			ua, err := profile.UserAgent(t.Context())
			require.NoError(t, err)
			require.Contains(t, ua, test.want)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = profile.UserAgent(ctx)
			require.ErrorIs(t, err, context.Canceled)
			require.NoError(t, os.WriteFile(metadata, []byte("invalid-version"), 0o600))
			_, err = profile.UserAgent(t.Context())
			require.ErrorContains(t, err, "version metadata is invalid")
			require.NoError(t, os.Remove(metadata))
			_, err = profile.UserAgent(t.Context())
			require.ErrorContains(t, err, "version is unavailable")
		})
	}
	ua, err := browserUserAgent(browserProfileChromium, "Microsoft Edge", "144.0.1.2", runtime.GOOS, runtime.GOARCH)
	require.NoError(t, err)
	require.Contains(t, ua, " Edg/144.0.0.0")
}

func TestBrowserCookieCopyPreservesScopeAndRedactsValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.sqlite")
	database, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `CREATE TABLE moz_cookies (host TEXT, path TEXT, isSecure INTEGER, expiry INTEGER, name TEXT, value TEXT, isHttpOnly INTEGER)`)
	require.NoError(t, err)
	for _, row := range []struct {
		host, path, name, value string
		secure                  int
		expiry                  int64
	}{
		{"auth.example.test", "/account", "hostonly", "synthetic-hostonly-cookie-9481", 1, 0},
		{".example.test", "/", "shared", "synthetic-shared-cookie-8241", 0, 0},
		{".example.test", "/", "expired", "synthetic-expired-cookie-3918", 0, time.Now().Add(-time.Hour).Unix()},
		{".different.test", "/", "unrelated", "synthetic-unrelated-cookie-8428", 0, 0},
	} {
		_, err = database.ExecContext(t.Context(), `INSERT INTO moz_cookies VALUES (?, ?, ?, ?, ?, ?, 1)`, row.host, row.path, row.secure, row.expiry, row.name, row.value)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	profile := BrowserProfile{ID: "synthetic", profile: browserProfile{kind: browserProfileFirefox, cookiesPath: path}}
	target, err := url.Parse("https://auth.example.test/account/details")
	require.NoError(t, err)
	jar, err := profile.CopyCookiesForURL(t.Context(), target)
	require.NoError(t, err)
	for _, test := range []struct {
		address string
		names   []string
	}{
		{"https://auth.example.test/account/details", []string{"hostonly", "shared"}},
		{"https://auth.example.test/elsewhere", []string{"shared"}},
		{"http://auth.example.test/account/details", []string{"shared"}},
		{"https://child.auth.example.test/account/details", []string{"shared"}},
		{"https://different.test/", nil},
	} {
		u, err := url.Parse(test.address)
		require.NoError(t, err)
		var names []string
		for _, cookie := range jar.Cookies(u) {
			names = append(names, cookie.Name)
		}
		require.ElementsMatch(t, test.names, names, test.address)
	}
	require.NotContains(t, redact.String("synthetic-hostonly-cookie-9481"), "synthetic-hostonly-cookie-9481")
	require.Equal(t, "synthetic-unrelated-cookie-8428", redact.String("synthetic-unrelated-cookie-8428"))
	jar.SetCookies(target, []*http.Cookie{{Name: "hostonly", Value: "synthetic-new-cookie-8472", Path: "/account", Secure: true}})
	require.NotContains(t, redact.String("synthetic-new-cookie-8472"), "synthetic-new-cookie-8472")
	require.Equal(t, "synthetic-new-cookie-8472", jar.Cookies(target)[0].Value)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	emptyTarget, err := url.Parse("https://uncookied.test/")
	require.NoError(t, err)
	empty, err := profile.CopyCookiesForURL(t.Context(), emptyTarget)
	require.NoError(t, err)
	require.Empty(t, empty.Cookies(emptyTarget))
}

func TestBrowserCookieCopyFailsOnUnavailableDecryption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Cookies")
	database, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `CREATE TABLE meta (key TEXT, value INTEGER); CREATE TABLE cookies (host_key TEXT, path TEXT, is_secure INTEGER, expires_utc INTEGER, name TEXT, value TEXT, encrypted_value BLOB, is_httponly INTEGER); INSERT INTO cookies VALUES ('.example.test', '/', 1, 0, 'session', '', X'76323000000000', 1)`)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	profile := BrowserProfile{ID: "synthetic", profile: browserProfile{kind: browserProfileChromium, cookiesPath: path}}
	target, err := url.Parse("https://example.test")
	require.NoError(t, err)
	jar, err := profile.CopyCookiesForURL(t.Context(), target)
	require.Error(t, err)
	require.Nil(t, jar)
}
