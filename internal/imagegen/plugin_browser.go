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
	mu            sync.Mutex
	entries       map[string]*imageBrowserEntry
	importCookies func(context.Context, []string, manifest.ImageCredential, string) (http.CookieJar, error)
}

type imageBrowserEntry struct {
	binding  string
	identity string
	jar      http.CookieJar
	done     chan struct{}
	err      error
}

func (c *imageBrowserCache) resolve(ctx context.Context, environment []string, owner providerplugin.ImageOwner, declaration manifest.ImageCredential, selected string, configuration map[string]any) (http.CookieJar, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if selected == "" {
		return nil, "", errors.New("image browser credential requires an explicit browser_profiles binding")
	}
	keyBytes, err := json.Marshal(struct {
		Owner      providerplugin.ImageOwner
		Credential string
	}{owner, declaration.ID})
	if err != nil {
		return nil, "", errors.New("cannot identify browser credential owner")
	}
	bindingBytes, err := json.Marshal(struct {
		Declaration   manifest.ImageCredential
		Profile       string
		Configuration map[string]any
	}{declaration, selected, configuration})
	if err != nil {
		return nil, "", errors.New("cannot identify browser credential configuration")
	}
	bindingDigest := sha256.Sum256(bindingBytes)
	binding := hex.EncodeToString(bindingDigest[:])
	key := string(keyBytes)
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && entry.binding == binding {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-entry.done:
			return entry.jar, entry.identity, entry.err
		}
	}
	entry := &imageBrowserEntry{binding: binding, done: make(chan struct{})}
	if c.entries == nil {
		c.entries = map[string]*imageBrowserEntry{}
	}
	c.entries[key] = entry
	c.mu.Unlock()
	importCookies := c.importCookies
	if importCookies == nil {
		importCookies = func(ctx context.Context, environment []string, declaration manifest.ImageCredential, selected string) (http.CookieJar, error) {
			for _, profile := range cookieutil.BrowserProfiles(environment) {
				if profile.ID == selected {
					return profile.Import(ctx, declaration.Domains)
				}
			}
			return nil, errors.New("selected image browser profile is unavailable on the execution host; open the selected browser and authenticate again")
		}
	}
	entry.jar, entry.err = importCookies(ctx, environment, declaration, selected)
	if entry.err == nil && entry.jar == nil {
		entry.err = errors.New("image browser authentication returned no cookie jar")
	}
	if entry.err == nil {
		identity := sha256.New()
		identity.Write([]byte(binding))
		for _, domain := range declaration.Domains {
			cookies := entry.jar.Cookies(&url.URL{Scheme: "https", Host: strings.TrimPrefix(domain, "."), Path: "/"})
			for _, cookie := range cookies {
				redact.Register(cookie.Value)
			}
			data, err := json.Marshal(cookies)
			if err != nil {
				entry.err = errors.New("cannot identify selected browser session")
				break
			}
			identity.Write(data)
		}
		entry.identity = hex.EncodeToString(identity.Sum(nil))
	}
	if entry.err == nil {
		entry.err = ctx.Err()
	}
	c.mu.Lock()
	if entry.err != nil {
		entry.jar = nil
		entry.identity = ""
		if c.entries[key] == entry {
			delete(c.entries, key)
		}
	}
	close(entry.done)
	c.mu.Unlock()
	return entry.jar, entry.identity, entry.err
}
