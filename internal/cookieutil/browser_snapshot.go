package cookieutil

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/example-git/crux/internal/redact"
	"golang.org/x/net/publicsuffix"
)

const MaxBrowserSnapshotCookies = 512
const MaxBrowserSnapshotBytes = 1 << 20

// BrowserCookie preserves host-only versus domain, path, security and expiry
// semantics. It is private credential input, never public browser metadata.
type BrowserCookie struct {
	Host     string `json:"host"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"http_only"`
	Expires  int64  `json:"expires"`
}

func (BrowserCookie) String() string   { return "[private browser cookie]" }
func (BrowserCookie) GoString() string { return "[private browser cookie]" }

type browserSnapshotJar struct {
	cookies []BrowserCookie
	err     error
	bytes   int
}

func (*browserSnapshotJar) Cookies(*url.URL) []*http.Cookie { return nil }
func (j *browserSnapshotJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	if j.err != nil {
		return
	}
	for _, c := range cookies {
		j.bytes += len(target.Hostname()) + len(c.Name) + len(c.Value) + len(c.Domain) + len(c.Path) + 128
		if j.bytes > MaxBrowserSnapshotBytes {
			j.err = errors.New("selected browser cookies exceed snapshot byte limit")
			return
		}
		if len(j.cookies) >= MaxBrowserSnapshotCookies {
			j.err = errors.New("selected browser cookies exceed snapshot count limit")
			return
		}
		expires := int64(0)
		if !c.Expires.IsZero() {
			expires = c.Expires.Unix()
		}
		j.cookies = append(j.cookies, BrowserCookie{Host: target.Hostname(), Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HttpOnly, Expires: expires})
	}
}

// Export reads only the explicitly selected profile and declared domains. No
// browser writes or alternate-profile search occurs after selection.
func (p BrowserProfile) Export(ctx context.Context, domains []string) ([]BrowserCookie, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ID == "" || !filepath.IsAbs(p.profile.cookiesPath) {
		return nil, errors.New("browser profile selection is unavailable")
	}
	if err := ValidateDomains(domains); err != nil {
		return nil, err
	}
	jar := &browserSnapshotJar{}
	var err error
	if p.profile.kind == browserProfileFirefox {
		err = loadFirefoxCookies(ctx, p.profile.cookiesPath, jar, domains)
	} else {
		err = loadChromiumCookies(ctx, p.profile, jar, domains, true)
	}
	if err != nil {
		return nil, errors.New("could not capture selected browser credentials")
	}
	if jar.err != nil {
		return nil, jar.err
	}
	if err := ValidateBrowserSnapshot(jar.cookies, domains); err != nil {
		return nil, err
	}
	sort.Slice(jar.cookies, func(i, j int) bool {
		a, b := jar.cookies[i], jar.cookies[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})
	for _, c := range jar.cookies {
		redact.Register(c.Value)
	}
	return jar.cookies, nil
}

func ValidateBrowserSnapshot(cookies []BrowserCookie, domains []string) error {
	if err := ValidateDomains(domains); err != nil {
		return err
	}
	if len(cookies) == 0 || len(cookies) > MaxBrowserSnapshotCookies {
		return errors.New("browser snapshot has an invalid cookie count")
	}
	data, err := json.Marshal(cookies)
	if err != nil || len(data) > MaxBrowserSnapshotBytes {
		return errors.New("browser snapshot exceeds byte limit")
	}
	seen := map[string]bool{}
	for _, c := range cookies {
		if c.Host == "" || c.Host != strings.ToLower(c.Host) || !MatchesDomain(c.Host, domains) || strings.ContainsAny(c.Host, "/:@?#\\") || c.Value == "" || c.Expires < 0 {
			return errors.New("browser snapshot has an invalid credential scope")
		}
		if c.Domain != "" && (c.Domain != "."+c.Host || !MatchesDomain(c.Domain, domains)) {
			return errors.New("browser snapshot broadens its declared domain")
		}
		if !strings.HasPrefix(c.Path, "/") || strings.ContainsAny(c.Path, "\r\n\x00") {
			return errors.New("browser snapshot has an invalid cookie path")
		}
		cookie := http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly}
		if err := cookie.Valid(); err != nil {
			return errors.New("browser snapshot contains an invalid cookie")
		}
		key := c.Host + "\x00" + c.Domain + "\x00" + c.Path + "\x00" + c.Name
		if seen[key] {
			return errors.New("browser snapshot repeats a cookie identity")
		}
		seen[key] = true
	}
	return nil
}

func BrowserSnapshotJar(cookies []BrowserCookie, domains []string) (http.CookieJar, error) {
	if err := ValidateBrowserSnapshot(cookies, domains); err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	for _, c := range cookies {
		expires := time.Time{}
		if c.Expires != 0 {
			expires = time.Unix(c.Expires, 0)
		}
		jar.SetCookies(&url.URL{Scheme: "https", Host: c.Host, Path: c.Path}, []*http.Cookie{{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly, Expires: expires}})
		redact.Register(c.Value)
	}
	return jar, nil
}
