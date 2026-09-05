package cookieutil

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	_ "modernc.org/sqlite"
)

type browserProfileKind uint8

const (
	browserProfileChromium browserProfileKind = iota
	browserProfileFirefox
)

type browserProfile struct {
	kind           browserProfileKind
	cookiesPath    string
	localStatePath string
	keyService     string
	keyAccount     string
	keyApplication string
}

type chromiumBrowser struct {
	root           string
	localStatePath string
	keyService     string
	keyAccount     string
	keyApplication string
}

type BrowserProfile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	profile browserProfile
}

func BrowserProfiles(environment []string) []BrowserProfile {
	values := map[string]string{}
	for _, entry := range environment {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	profiles := platformBrowserProfilesFromEnvironment(func(key string) string { return values[key] })
	result := make([]BrowserProfile, 0, len(profiles))
	for _, profile := range profiles {
		digest := sha256.Sum256([]byte(profile.cookiesPath))
		name := filepath.Base(filepath.Dir(profile.cookiesPath))
		if name == "Network" {
			name = filepath.Base(filepath.Dir(filepath.Dir(profile.cookiesPath)))
		}
		browser := profile.keyAccount
		if browser == "" {
			browser = profile.keyApplication
		}
		if browser == "" && profile.kind == browserProfileFirefox {
			browser = "Firefox"
		}
		if browser == "" {
			browser = "Chromium"
		}
		result = append(result, BrowserProfile{ID: fmt.Sprintf("%x", digest), Name: browser + " / " + name, profile: profile})
	}
	return result
}

func (p BrowserProfile) Import(ctx context.Context, domains []string) (http.CookieJar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ID == "" || !filepath.IsAbs(p.profile.cookiesPath) {
		return nil, errors.New("browser profile selection is unavailable")
	}
	if err := ValidateDomains(domains); err != nil {
		return nil, err
	}
	return loadBrowserCookieJar(ctx, p.profile, domains)
}

func platformBrowserProfiles() []browserProfile {
	return platformBrowserProfilesFromEnvironment(os.Getenv)
}

func ImportBrowserJars(ctx context.Context, domains []string) ([]http.CookieJar, error) {
	if err := ValidateDomains(domains); err != nil {
		return nil, err
	}
	profiles := platformBrowserProfiles()
	if len(profiles) == 0 {
		return nil, errors.New("no supported local browser profiles were found")
	}
	jars := make([]http.CookieJar, 0, len(profiles))
	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		jar, err := loadBrowserCookieJar(ctx, profile, domains)
		if err != nil || jar == nil {
			continue
		}
		jars = append(jars, jar)
	}
	if len(jars) == 0 {
		return nil, errors.New("no usable cookies were found for the declared domains")
	}
	return jars, nil
}

type importedJar struct {
	http.CookieJar
	populated bool
}

func (j *importedJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	j.CookieJar.SetCookies(target, cookies)
	j.populated = j.populated || len(j.Cookies(target)) > 0
}

func loadBrowserCookieJar(ctx context.Context, profile browserProfile, domains []string) (http.CookieJar, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	loaded := &importedJar{CookieJar: jar}
	if profile.kind == browserProfileFirefox {
		err = loadFirefoxCookies(ctx, profile.cookiesPath, loaded, domains)
	} else {
		err = loadChromiumCookies(ctx, profile, loaded, domains)
	}
	if err != nil {
		return nil, err
	}
	if !loaded.populated {
		return nil, errors.New("browser profile has no matching cookies")
	}
	return jar, nil
}

func loadFirefoxCookies(ctx context.Context, path string, jar http.CookieJar, domains []string) error {
	database, err := openBrowserCookieDatabase(ctx, path)
	if err != nil {
		return err
	}
	defer database.Close()
	where, args := domainQuery("host", domains)
	rows, err := database.QueryContext(ctx, `SELECT host, path, isSecure, expiry, name, value, isHttpOnly FROM moz_cookies WHERE `+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var host, path, name, value string
		var secure, httpOnly int
		var expiry int64
		if err := rows.Scan(&host, &path, &secure, &expiry, &name, &value, &httpOnly); err != nil {
			return err
		}
		if value == "" || !MatchesDomain(host, domains) {
			continue
		}
		expires := time.Time{}
		if expiry > 0 {
			expires = time.Unix(expiry, 0)
			if expires.Before(time.Now()) {
				continue
			}
		}
		setBrowserCookie(jar, host, path, name, value, secure != 0, httpOnly != 0, expires)
	}
	return rows.Err()
}

func loadChromiumCookies(ctx context.Context, profile browserProfile, jar http.CookieJar, domains []string) error {
	database, err := openBrowserCookieDatabase(ctx, profile.cookiesPath)
	if err != nil {
		return err
	}
	defer database.Close()
	var databaseVersion int
	_ = database.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'version'`).Scan(&databaseVersion)
	where, args := domainQuery("host_key", domains)
	query := `SELECT host_key, path, is_secure, expires_utc, name, value, encrypted_value, is_httponly FROM cookies WHERE ` + where
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		query = `SELECT host_key, path, secure, expires_utc, name, value, encrypted_value, httponly FROM cookies WHERE ` + where
		rows, err = database.QueryContext(ctx, query, args...)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	var decrypt func([]byte) ([]byte, error)
	decryptAttempted := false
	for rows.Next() {
		var host, path, name, value string
		var encrypted []byte
		var secure, httpOnly int
		var expiresRaw int64
		if err := rows.Scan(&host, &path, &secure, &expiresRaw, &name, &value, &encrypted, &httpOnly); err != nil {
			return err
		}
		if !MatchesDomain(host, domains) {
			continue
		}
		if value == "" && len(encrypted) > 0 {
			if !decryptAttempted {
				decrypt, _ = chromiumCookieDecryptor(ctx, profile)
				decryptAttempted = true
			}
			if decrypt == nil {
				continue
			}
			plaintext, decryptErr := decrypt(encrypted)
			if decryptErr != nil {
				continue
			}
			plaintext, decryptErr = stripChromiumHostDigest(host, plaintext, databaseVersion >= 24)
			if decryptErr != nil {
				continue
			}
			value = string(plaintext)
		}
		if value == "" {
			continue
		}
		expires := chromiumCookieExpiry(expiresRaw)
		if !expires.IsZero() && expires.Before(time.Now()) {
			continue
		}
		setBrowserCookie(jar, host, path, name, value, secure != 0, httpOnly != 0, expires)
	}
	return rows.Err()
}

func openBrowserCookieDatabase(ctx context.Context, path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	location := &url.URL{Scheme: "file", Path: uriPath}
	database, err := sql.Open("sqlite", location.String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		return nil, err
	}
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

func setBrowserCookie(jar http.CookieJar, host, path, name, value string, secure, httpOnly bool, expires time.Time) {
	requestHost := strings.TrimPrefix(strings.TrimSpace(host), ".")
	if requestHost == "" {
		return
	}
	if path == "" {
		path = "/"
	}
	jar.SetCookies(&url.URL{Scheme: "https", Host: requestHost, Path: path}, []*http.Cookie{{
		Name: name, Value: value, Domain: host, Path: path, Secure: secure, HttpOnly: httpOnly, Expires: expires,
	}})
}

func chromiumCookieExpiry(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	const chromeEpochOffsetMicroseconds = int64(11_644_473_600) * 1_000_000
	unixMicroseconds := value - chromeEpochOffsetMicroseconds
	if unixMicroseconds <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(unixMicroseconds)
}

func stripChromiumHostDigest(host string, plaintext []byte, required bool) ([]byte, error) {
	if !required {
		return plaintext, nil
	}
	if len(plaintext) < sha256.Size {
		return nil, errors.New("browser cookie integrity prefix is missing")
	}
	digest := sha256.Sum256([]byte(host))
	if !bytes.Equal(plaintext[:sha256.Size], digest[:]) {
		return nil, errors.New("browser cookie integrity prefix is invalid")
	}
	return plaintext[sha256.Size:], nil
}

func decryptChromiumCBC(encrypted, key []byte) ([]byte, error) {
	if len(encrypted) < 3 || (string(encrypted[:3]) != "v10" && string(encrypted[:3]) != "v11") {
		return nil, errors.New("unsupported browser cookie encryption")
	}
	ciphertext := encrypted[3:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) == 0 || len(ciphertext)%block.BlockSize() != 0 {
		return nil, errors.New("invalid browser cookie ciphertext")
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(plaintext, ciphertext)
	padding := int(plaintext[len(plaintext)-1])
	if padding < 1 || padding > aes.BlockSize || padding > len(plaintext) {
		return nil, errors.New("invalid browser cookie padding")
	}
	for _, value := range plaintext[len(plaintext)-padding:] {
		if int(value) != padding {
			return nil, errors.New("invalid browser cookie padding")
		}
	}
	return plaintext[:len(plaintext)-padding], nil
}

func discoverBrowserProfiles(browsers []chromiumBrowser, firefoxRoots []string) []browserProfile {
	profiles := make([]browserProfile, 0)
	for _, browser := range browsers {
		if !filepath.IsAbs(browser.root) {
			continue
		}
		entries, err := os.ReadDir(browser.root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || (entry.Name() != "Default" && !strings.HasPrefix(entry.Name(), "Profile ")) {
				continue
			}
			profileRoot := filepath.Join(browser.root, entry.Name())
			for _, candidate := range []string{filepath.Join(profileRoot, "Network", "Cookies"), filepath.Join(profileRoot, "Cookies")} {
				if regularFile(candidate) {
					profiles = append(profiles, browserProfile{
						kind: browserProfileChromium, cookiesPath: candidate, localStatePath: browser.localStatePath,
						keyService: browser.keyService, keyAccount: browser.keyAccount, keyApplication: browser.keyApplication,
					})
					break
				}
			}
		}
	}
	for _, root := range firefoxRoots {
		if !filepath.IsAbs(root) {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			candidate := filepath.Join(root, entry.Name(), "cookies.sqlite")
			if regularFile(candidate) {
				profiles = append(profiles, browserProfile{kind: browserProfileFirefox, cookiesPath: candidate})
			}
		}
	}
	sort.SliceStable(profiles, func(left, right int) bool {
		if profiles[left].kind != profiles[right].kind {
			return profiles[left].kind < profiles[right].kind
		}
		return profiles[left].cookiesPath < profiles[right].cookiesPath
	})
	return profiles
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func browserCookieDecryptorFromKey(key []byte) func([]byte) ([]byte, error) {
	return func(encrypted []byte) ([]byte, error) {
		return decryptChromiumCBC(encrypted, key)
	}
}
