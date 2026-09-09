package config

import (
	"context"
	"errors"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport/clientidentity"
)

type RemoteImageClientIdentity struct {
	Owner     providerplugin.ImageOwner `json:"owner"`
	Name      string                    `json:"name"`
	Version   string                    `json:"version"`
	UserAgent string                    `json:"user_agent"`
	OS        string                    `json:"os"`
	Arch      string                    `json:"arch"`
}

func selectedImageOwners(images *ImageConfiguration) []providerplugin.ImageOwner {
	owners := map[providerplugin.ImageOwner]bool{}
	if images != nil {
		for _, owner := range images.Preferred {
			owners[owner] = true
		}
		for _, provider := range images.Providers {
			owners[provider.Owner] = true
		}
	}
	result := make([]providerplugin.ImageOwner, 0, len(owners))
	for owner := range owners {
		result = append(result, owner)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Digest != result[j].Digest {
			return result[i].Digest < result[j].Digest
		}
		return result[i].Backend < result[j].Backend
	})
	return result
}

func (s RuntimeSnapshot) prepareImageClientIdentities(ctx context.Context) error {
	if s.Config() == nil {
		return errors.New("image configuration is unavailable")
	}
	for _, owner := range selectedImageOwners(s.Config().Images) {
		if s.Config().providerScan == nil || s.imageInputs == nil {
			return errors.New("client image input capture is unavailable")
		}
		transport, ok := s.Config().providerScan.bundles[owner.Digest]
		if !ok {
			return errors.New("client image bundle was not captured")
		}
		bundle, err := providerplugin.ValidateDetachedBundle(transport)
		if err != nil {
			return err
		}
		if bundle.Image() == nil || bundle.ID() != owner.PluginID || bundle.Version() != owner.Version || bundle.ProviderID() != owner.Backend {
			return errors.New("client image owner does not match captured bundle")
		}
		names := make([]string, 0, len(bundle.Image().ClientIdentities))
		for name := range bundle.Image().ClientIdentities {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			key := "identity:" + imageBrowserKey(owner, name)
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				capture := s.imageInputs
				capture.mu.Lock()
				if _, ok := capture.identities[key]; ok {
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
				declaration := bundle.Image().ClientIdentities[name]
				version, agent, err := clientidentity.ResolveWithEnvironment(ctx, &declaration, s.Environment())
				capture.mu.Lock()
				delete(capture.running, key)
				if err == nil {
					capture.identities[key] = RemoteImageClientIdentity{Owner: owner, Name: name, Version: version, UserAgent: agent, OS: runtime.GOOS, Arch: runtime.GOARCH}
				}
				close(done)
				capture.mu.Unlock()
				if err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func (s RuntimeSnapshot) collectedImageClientIdentities() []RemoteImageClientIdentity {
	if s.imageInputs == nil {
		return nil
	}
	s.imageInputs.mu.Lock()
	defer s.imageInputs.mu.Unlock()
	keys := make([]string, 0, len(s.imageInputs.identities))
	for key := range s.imageInputs.identities {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result []RemoteImageClientIdentity
	for _, key := range keys {
		result = append(result, s.imageInputs.identities[key])
	}
	return result
}

func validateRemoteImageIdentities(proposal RemoteRuntimeProposal, bundles map[string]providerplugin.DetachedBundle) error {
	expected := map[string]manifest.ResolvedClientIdentity{}
	owners := map[string]providerplugin.ImageOwner{}
	for _, owner := range selectedImageOwners(proposal.Images) {
		bundle, ok := bundles[owner.Digest]
		if !ok || bundle.Image() == nil {
			return errors.New("client image identity bundle is unavailable")
		}
		for name, declaration := range bundle.Image().ClientIdentities {
			key := imageBrowserKey(owner, name)
			expected[key] = declaration
			owners[key] = owner
		}
	}
	if len(proposal.ImageClientIdentities) != len(expected) {
		return errors.New("client image identities do not match selected declarations")
	}
	seen := map[string]bool{}
	for _, identity := range proposal.ImageClientIdentities {
		key := imageBrowserKey(identity.Owner, identity.Name)
		declaration, ok := expected[key]
		if !ok || seen[key] || identity.Owner != owners[key] {
			return errors.New("invalid or repeated client image identity")
		}
		seen[key] = true
		pattern, err := regexp.Compile(declaration.VersionPattern)
		if err != nil || len(identity.Version) > 256 || strings.ContainsAny(identity.Version, "\r\n\x00") || !pattern.MatchString(identity.Version) || len(identity.UserAgent) > 2048 || strings.ContainsAny(identity.UserAgent, "\r\n\x00") {
			return errors.New("invalid captured image client identity")
		}
		for _, part := range []string{identity.OS, identity.Arch} {
			if part == "" || len(part) > 32 || strings.IndexFunc(part, func(r rune) bool { return r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') }) >= 0 {
				return errors.New("invalid captured image client platform")
			}
		}
		if strings.NewReplacer("{version}", identity.Version, "{os}", identity.OS, "{arch}", identity.Arch).Replace(declaration.UserAgentFormat) != identity.UserAgent {
			return errors.New("captured image identity does not match declared format")
		}
	}
	return nil
}

func (s RuntimeSnapshot) ClientImageIdentities(owner providerplugin.ImageOwner) (map[string]any, error) {
	bundle, handled, err := s.ClientImageBundle(owner)
	if err != nil {
		return nil, err
	}
	if !handled {
		return nil, errors.New("client image identity authority is unavailable")
	}
	result := map[string]any{}
	for _, identity := range s.clientRuntime.proposal.ImageClientIdentities {
		if identity.Owner == owner {
			result[identity.Name] = map[string]any{"version": identity.Version, "user_agent": identity.UserAgent}
		}
	}
	if len(result) != len(bundle.Manifest.ClientIdentities) {
		return nil, errors.New("captured client image identities are incomplete")
	}
	return result, nil
}
