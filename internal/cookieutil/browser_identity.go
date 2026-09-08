package cookieutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/example-git/crux/internal/redact"
	"golang.org/x/net/publicsuffix"
)

func (p BrowserProfile) UserAgent(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.ID == "" || !filepath.IsAbs(p.profile.cookiesPath) {
		return "", errors.New("browser profile selection is unavailable")
	}
	profileDir := filepath.Dir(p.profile.cookiesPath)
	if filepath.Base(profileDir) == "Network" {
		profileDir = filepath.Dir(profileDir)
	}
	var version string
	if p.profile.kind == browserProfileFirefox {
		data, err := readBrowserIdentityFile(filepath.Join(profileDir, "compatibility.ini"))
		if err != nil {
			return "", err
		}
		for line := range strings.SplitSeq(data, "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "LastVersion="); ok {
				version, _, _ = strings.Cut(value, "_")
				break
			}
		}
	} else {
		data, err := readBrowserIdentityFile(filepath.Join(filepath.Dir(profileDir), "Last Version"))
		if err != nil {
			return "", err
		}
		version = strings.TrimSpace(data)
	}
	return browserUserAgent(p.profile.kind, filepath.Dir(profileDir), version, runtime.GOOS, runtime.GOARCH)
}

func readBrowserIdentityFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("selected browser version is unavailable; open that browser and retry")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return "", errors.New("selected browser version metadata is unreadable")
	}
	return string(data), nil
}

func browserUserAgent(kind browserProfileKind, root, version, goos, goarch string) (string, error) {
	majorText, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 1 {
		return "", errors.New("selected browser version metadata is invalid")
	}
	platform := ""
	switch goos {
	case "darwin":
		platform = "Macintosh; Intel Mac OS X 10_15_7"
	case "windows":
		platform = "Windows NT 10.0; Win64; x64"
	case "linux":
		platform = "X11; Linux x86_64"
		if kind == browserProfileFirefox && goarch == "arm64" {
			platform = "X11; Linux aarch64"
		}
	default:
		return "", errors.New("browser user agent is unavailable on this platform")
	}
	if kind == browserProfileFirefox {
		if goos == "darwin" {
			platform = "Macintosh; Intel Mac OS X 10.15"
		}
		return fmt.Sprintf("Mozilla/5.0 (%s; rv:%d.0) Gecko/20100101 Firefox/%d.0", platform, major, major), nil
	}
	if kind != browserProfileChromium {
		return "", errors.New("selected browser family is unsupported")
	}
	value := fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", platform, major)
	root = strings.ToLower(filepath.ToSlash(root))
	if strings.Contains(root, "microsoft edge") || strings.Contains(root, "microsoft-edge") || strings.Contains(root, "microsoft/edge/") {
		value += fmt.Sprintf(" Edg/%d.0.0.0", major)
	}
	return value, nil
}

func (p BrowserProfile) CopyCookiesForURL(ctx context.Context, target *url.URL) (http.CookieJar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ID == "" || !filepath.IsAbs(p.profile.cookiesPath) {
		return nil, errors.New("browser profile selection is unavailable")
	}
	if target == nil || target.Hostname() == "" || target.User != nil || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, errors.New("browser fetch requires an HTTP or HTTPS URL without embedded credentials")
	}
	domain := strings.ToLower(target.Hostname())
	if registered, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		domain = registered
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	copied := browserCopyJar{CookieJar: jar}
	switch p.profile.kind {
	case browserProfileFirefox:
		err = loadFirefoxCookies(ctx, p.profile.cookiesPath, copied, []string{domain})
	case browserProfileChromium:
		err = loadChromiumCookies(ctx, p.profile, copied, []string{domain}, true)
	default:
		err = errors.New("selected browser family is unsupported")
	}
	if err != nil {
		return nil, fmt.Errorf("cannot copy cookies from the selected browser: %w", err)
	}
	return copied, nil
}

type browserCopyJar struct {
	http.CookieJar
}

func (j browserCopyJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	for _, cookie := range cookies {
		redact.Register(cookie.Value)
	}
	j.CookieJar.SetCookies(target, cookies)
}
