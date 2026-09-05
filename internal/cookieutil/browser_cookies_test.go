package cookieutil

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBrowserProfilesOnlyDiscoverCapturedAbsoluteRoots(t *testing.T) {
	home := t.TempDir()
	var root string
	environment := []string{"HOME=" + home, "APPDATA=" + home, "LOCALAPPDATA=" + home}
	switch runtime.GOOS {
	case "darwin":
		root = filepath.Join(home, "Library", "Application Support", "Firefox", "Profiles")
	case "linux":
		root = filepath.Join(home, ".mozilla", "firefox")
	case "windows":
		root = filepath.Join(home, "Mozilla", "Firefox", "Profiles")
	default:
		t.Skip("browser discovery is unavailable")
	}
	profileRoot := filepath.Join(root, "synthetic")
	if err := os.MkdirAll(profileRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileRoot, "cookies.sqlite"), []byte("not a database; private contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles := BrowserProfiles(environment)
	if len(profiles) != 1 {
		t.Fatalf("profiles = %d", len(profiles))
	}
	if len(profiles[0].ID) != 64 || profiles[0].Name != "Firefox / synthetic" {
		t.Fatal("profile identity or label is invalid")
	}
	data, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), home) || strings.Contains(string(data), "private contents") || !strings.Contains(string(data), `"id"`) {
		t.Fatal("profile JSON exposes private state or lacks identity")
	}
	if len(BrowserProfiles(nil)) != 0 {
		t.Fatal("empty environment borrowed process profiles")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := profiles[0].Import(ctx, []string{"provider.example"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled import = %v", err)
	}
	t.Chdir(home)
	if err := os.MkdirAll(filepath.Join("relative", "profile"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("relative", "profile", "cookies.sqlite"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(discoverBrowserProfiles(nil, []string{"relative"})) != 0 {
		t.Fatal("relative browser root was accepted")
	}
}

func TestLoadFirefoxCookiesImportsSessionInMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `CREATE TABLE moz_cookies (host TEXT, path TEXT, isSecure INTEGER, expiry INTEGER, name TEXT, value TEXT, isHttpOnly INTEGER)`); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).Unix()
	if _, err := database.ExecContext(t.Context(), `INSERT INTO moz_cookies VALUES (?, '/', 1, ?, 'session_id', 'session-value', 1)`, ".provider.example", expiry); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `INSERT INTO moz_cookies VALUES (?, '/', 1, ?, 'ignored', 'other-value', 1)`, ".example.com", expiry); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	if err := loadFirefoxCookies(t.Context(), path, jar, []string{"provider.example"}); err != nil {
		t.Fatalf("loadFirefoxCookies: %v", err)
	}
	cookies := jar.Cookies(&url.URL{Scheme: "https", Host: "app.provider.example", Path: "/"})
	if len(cookies) != 1 || cookies[0].Name != "session_id" || cookies[0].Value != "session-value" {
		t.Fatalf("cookies = %#v", cookies)
	}
}

func TestLoadChromiumCookiesAcceptsPlaintextAndSkipsOtherDomains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Cookies")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `CREATE TABLE meta (key TEXT, value INTEGER); INSERT INTO meta VALUES ('version', 24); CREATE TABLE cookies (host_key TEXT, path TEXT, is_secure INTEGER, expires_utc INTEGER, name TEXT, value TEXT, encrypted_value BLOB, is_httponly INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `INSERT INTO cookies VALUES ('.provider.example', '/', 1, 0, 'session_id', 'plain-session', X'', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `INSERT INTO cookies VALUES ('.example.com', '/', 1, 0, 'ignored', 'other', X'', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	if err := loadChromiumCookies(t.Context(), browserProfile{kind: browserProfileChromium, cookiesPath: path}, jar, []string{"provider.example"}); err != nil {
		t.Fatalf("loadChromiumCookies: %v", err)
	}
	cookies := jar.Cookies(&url.URL{Scheme: "https", Host: "app.provider.example", Path: "/"})
	if len(cookies) != 1 || cookies[0].Value != "plain-session" {
		t.Fatalf("cookies = %#v", cookies)
	}
}

func TestStripChromiumHostDigest(t *testing.T) {
	host := ".provider.example"
	digest := sha256.Sum256([]byte(host))
	plaintext := append(append([]byte(nil), digest[:]...), []byte("session")...)
	stripped, err := stripChromiumHostDigest(host, plaintext, true)
	if err != nil || string(stripped) != "session" {
		t.Fatalf("strip = %q, %v", stripped, err)
	}
	plaintext[0] ^= 0xff
	if _, err := stripChromiumHostDigest(host, plaintext, true); err == nil {
		t.Fatal("invalid host digest was accepted")
	}
}
