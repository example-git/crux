package imagegen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/cookieutil"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/redact"
)

type imageBrowserCache struct {
	mu      sync.Mutex
	entries map[string]imageBrowserEntry
}

type imageBrowserEntry struct {
	identity string
	jar      http.CookieJar
}

func (c *imageBrowserCache) resolve(ctx context.Context, environment []string, owner providerplugin.ImageOwner, declaration manifest.ImageCredential, selected string) (http.CookieJar, string, error) {
	if selected == "" {
		return nil, "", errors.New("image browser credential requires an explicit browser_profiles binding")
	}
	var profile cookieutil.BrowserProfile
	for _, candidate := range cookieutil.BrowserProfiles(environment) {
		if candidate.ID == selected {
			profile = candidate
			break
		}
	}
	if profile.ID == "" {
		return nil, "", errors.New("selected image browser profile is unavailable on the execution host")
	}
	jar, err := profile.Import(ctx, declaration.Domains)
	if err != nil {
		return nil, "", err
	}
	identity := sha256.New()
	identity.Write([]byte(selected))
	for _, domain := range declaration.Domains {
		cookies := jar.Cookies(&url.URL{Scheme: "https", Host: strings.TrimPrefix(domain, "."), Path: "/"})
		for _, cookie := range cookies {
			redact.Register(cookie.Value)
		}
		data, err := json.Marshal(cookies)
		if err != nil {
			return nil, "", errors.New("cannot identify selected browser session")
		}
		identity.Write(data)
	}
	digest := hex.EncodeToString(identity.Sum(nil))
	keyBytes, err := json.Marshal(struct {
		Owner      providerplugin.ImageOwner
		Credential manifest.ImageCredential
	}{owner, declaration})
	if err != nil {
		return nil, "", errors.New("cannot identify browser credential owner")
	}
	key := string(keyBytes)
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok && entry.identity == digest {
		return entry.jar, digest, nil
	}
	if c.entries == nil || len(c.entries) >= 64 {
		c.entries = map[string]imageBrowserEntry{}
	}
	c.entries[key] = imageBrowserEntry{identity: digest, jar: jar}
	return jar, digest, nil
}
