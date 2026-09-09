package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"sync"

	"github.com/example-git/crux/internal/cookieutil"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/redact"
)

// RemoteImageBrowserCredential is private proposal data. The receiver uses
// these exact scoped cookies and never resolves the profile on its own host.
type RemoteImageBrowserCredential struct {
	Owner        providerplugin.ImageOwner  `json:"owner"`
	CredentialID string                     `json:"credential_id"`
	ProfileID    string                     `json:"profile_id"`
	Cookies      []cookieutil.BrowserCookie `json:"cookies"`
}

func (RemoteImageBrowserCredential) String() string   { return "[private image browser credential]" }
func (RemoteImageBrowserCredential) GoString() string { return "[private image browser credential]" }

type imageRuntimeCapture struct {
	config      *Config
	environment []string
	mu          sync.Mutex
	values      map[string][]cookieutil.BrowserCookie
	running     map[string]chan struct{}
	identities  map[string]RemoteImageClientIdentity
}

func newImageRuntimeCapture(cfg *Config, environment []string) *imageRuntimeCapture {
	environment = slices.Clone(environment)
	slices.Sort(environment)
	return &imageRuntimeCapture{config: cfg, environment: environment, values: map[string][]cookieutil.BrowserCookie{}, running: map[string]chan struct{}{}, identities: map[string]RemoteImageClientIdentity{}}
}
func (c *imageRuntimeCapture) matches(cfg *Config, environment []string) bool {
	if c == nil || c.config != cfg {
		return false
	}
	environment = slices.Clone(environment)
	slices.Sort(environment)
	return slices.Equal(c.environment, environment)
}

func imageBrowserKey(owner providerplugin.ImageOwner, id string) string {
	return owner.Digest + "\x00" + owner.Backend + "\x00" + id
}

type declaredImageBrowser struct {
	owner      providerplugin.ImageOwner
	credential manifest.ImageCredential
	profile    string
}

func declaredImageBrowsers(images *ImageConfiguration, bundle func(providerplugin.ImageOwner) (*manifest.ImageManifest, error)) (map[string]declaredImageBrowser, error) {
	result := map[string]declaredImageBrowser{}
	if images == nil {
		return result, nil
	}
	if err := images.Validate(); err != nil {
		return nil, err
	}
	owners := map[providerplugin.ImageOwner]bool{}
	for _, owner := range images.Preferred {
		owners[owner] = true
	}
	for _, provider := range images.Providers {
		owners[provider.Owner] = true
	}
	for owner := range owners {
		value, err := bundle(owner)
		if err != nil {
			return nil, err
		}
		for _, declaration := range value.Credentials {
			if declaration.Source != "browser" {
				continue
			}
			profile := images.Providers[owner.Backend].BrowserProfiles[declaration.ID]
			if profile == "" {
				return nil, errors.New("selected client image browser credential requires an explicit owning-client browser profile")
			}
			result[imageBrowserKey(owner, declaration.ID)] = declaredImageBrowser{owner: owner, credential: declaration, profile: profile}
		}
	}
	if len(result) > 64 {
		return nil, errors.New("too many client image browser bindings")
	}
	return result, nil
}

func (s RuntimeSnapshot) selectedImageBrowsers() (map[string]declaredImageBrowser, error) {
	if s.Config() == nil {
		return nil, errors.New("image configuration capture is unavailable")
	}
	return declaredImageBrowsers(s.Config().Images, func(owner providerplugin.ImageOwner) (*manifest.ImageManifest, error) {
		if s.Config().providerScan == nil {
			return nil, errors.New("client image bundle scan is unavailable")
		}
		transport, ok := s.Config().providerScan.bundles[owner.Digest]
		if !ok {
			return nil, errors.New("client image bundle is not captured")
		}
		bundle, err := providerplugin.ValidateDetachedBundle(transport)
		if err != nil {
			return nil, err
		}
		if bundle.Image() == nil || bundle.ID() != owner.PluginID || bundle.Version() != owner.Version || bundle.ProviderID() != owner.Backend {
			return nil, errors.New("client image owner does not match its captured bundle")
		}
		return bundle.Image(), nil
	})
}

func (s RuntimeSnapshot) prepareImageBrowserCredentials(ctx context.Context) error {
	if s.IsClientOwned() {
		return nil
	}
	if err := s.prepareImageClientIdentities(ctx); err != nil {
		return err
	}
	declarations, err := s.selectedImageBrowsers()
	if err != nil {
		return err
	}
	if len(declarations) == 0 {
		return nil
	}
	if s.imageInputs == nil {
		return errors.New("owning-client browser capture is unavailable")
	}
	keys := make([]string, 0, len(declarations))
	for key := range declarations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		declaration := declarations[key]
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			capture := s.imageInputs
			capture.mu.Lock()
			if _, ok := capture.values[key]; ok {
				capture.mu.Unlock()
				break
			}
			if done := capture.running[key]; done != nil {
				capture.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			done := make(chan struct{})
			capture.running[key] = done
			capture.mu.Unlock()
			var cookies []cookieutil.BrowserCookie
			err := errors.New("selected client image browser profile is unavailable; configure that profile on the owning client")
			for _, profile := range cookieutil.BrowserProfiles(s.Environment()) {
				if profile.ID == declaration.profile {
					cookies, err = profile.Export(ctx, declaration.credential.Domains)
					break
				}
			}
			capture.mu.Lock()
			delete(capture.running, key)
			if err == nil {
				capture.values[key] = slices.Clone(cookies)
			}
			close(done)
			capture.mu.Unlock()
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func (s RuntimeSnapshot) collectedImageBrowserCredentials() ([]RemoteImageBrowserCredential, error) {
	declarations, err := s.selectedImageBrowsers()
	if err != nil {
		return nil, err
	}
	if len(declarations) == 0 {
		return nil, nil
	}
	if s.imageInputs == nil {
		return nil, errors.New("client browser credentials were not prepared")
	}
	s.imageInputs.mu.Lock()
	defer s.imageInputs.mu.Unlock()
	keys := make([]string, 0, len(declarations))
	for key := range declarations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]RemoteImageBrowserCredential, 0, len(keys))
	for _, key := range keys {
		declaration := declarations[key]
		cookies, ok := s.imageInputs.values[key]
		if !ok {
			return nil, errors.New("client browser credentials were not prepared before collection")
		}
		result = append(result, RemoteImageBrowserCredential{Owner: declaration.owner, CredentialID: declaration.credential.ID, ProfileID: declaration.profile, Cookies: slices.Clone(cookies)})
	}
	return result, nil
}

func validateRemoteImageBrowsers(proposal RemoteRuntimeProposal, bundles map[string]providerplugin.DetachedBundle) error {
	declarations, err := declaredImageBrowsers(proposal.Images, func(owner providerplugin.ImageOwner) (*manifest.ImageManifest, error) {
		bundle, ok := bundles[owner.Digest]
		if !ok || bundle.Image() == nil || bundle.ID() != owner.PluginID || bundle.Version() != owner.Version || bundle.ProviderID() != owner.Backend {
			return nil, errors.New("client browser binding requires its exact image bundle")
		}
		return bundle.Image(), nil
	})
	if err != nil {
		return err
	}
	if len(proposal.ImageBrowserCredentials) != len(declarations) {
		return errors.New("client image browser bindings do not match selected declarations")
	}
	seen := map[string]bool{}
	for _, binding := range proposal.ImageBrowserCredentials {
		key := imageBrowserKey(binding.Owner, binding.CredentialID)
		declaration, ok := declarations[key]
		if !ok || seen[key] || binding.Owner != declaration.owner || binding.ProfileID != declaration.profile {
			return errors.New("invalid or duplicate client image browser binding")
		}
		seen[key] = true
		if err := cookieutil.ValidateBrowserSnapshot(binding.Cookies, declaration.credential.Domains); err != nil {
			return err
		}
	}
	return nil
}

func (s RuntimeSnapshot) ClientImageBrowserCredential(owner providerplugin.ImageOwner, id string) (http.CookieJar, string, error) {
	if !s.IsClientOwned() {
		return nil, "", errors.New("client image browser authority is unavailable")
	}
	bundle, _, err := s.ClientImageBundle(owner)
	if err != nil {
		return nil, "", err
	}
	var domains []string
	for _, declaration := range bundle.Manifest.Credentials {
		if declaration.ID == id && declaration.Source == "browser" {
			domains = declaration.Domains
			break
		}
	}
	for _, binding := range s.clientRuntime.proposal.ImageBrowserCredentials {
		if binding.Owner != owner || binding.CredentialID != id {
			continue
		}
		jar, err := cookieutil.BrowserSnapshotJar(binding.Cookies, domains)
		if err != nil {
			return nil, "", err
		}
		data, err := json.Marshal(binding)
		if err != nil {
			return nil, "", errors.New("invalid captured image browser credential")
		}
		digest := sha256.Sum256(data)
		return jar, hex.EncodeToString(digest[:]), nil
	}
	return nil, "", errors.New("selected client image browser credential was not forwarded")
}

func registerImageBrowserSecrets(bindings []RemoteImageBrowserCredential) {
	for _, binding := range bindings {
		for _, cookie := range binding.Cookies {
			redact.Register(cookie.Value)
		}
	}
}
