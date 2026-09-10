package config

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providerregistry"
)

// NativeIdentity declares captured native header defaults, never an environment
// for the execution host to interpret. Existing explicit provider header
// precedence is preserved. Strings are immutable and returned by value.
type NativeIdentity = useragent.NativeIdentity

type nativeIdentityCapture struct {
	environment []string
	mu          sync.Mutex
	resolved    map[providerregistry.Construction]NativeIdentity
	running     map[providerregistry.Construction]chan struct{}
}

func newNativeIdentityCapture(entries []string) *nativeIdentityCapture {
	entries = slices.Clone(entries)
	slices.Sort(entries)
	return &nativeIdentityCapture{environment: entries, resolved: make(map[providerregistry.Construction]NativeIdentity), running: make(map[providerregistry.Construction]chan struct{})}
}

func (capture *nativeIdentityCapture) matches(entries []string) bool {
	if capture == nil {
		return false
	}
	entries = slices.Clone(entries)
	slices.Sort(entries)
	return slices.Equal(capture.environment, entries)
}

func nativeConstruction(kind providerregistry.Construction) bool {
	return kind == providerregistry.ConstructionCodex || kind == providerregistry.ConstructionGeminiAntigravity
}

func validateNativeIdentity(kind providerregistry.Construction, identity *NativeIdentity) error {
	if !nativeConstruction(kind) {
		if identity != nil {
			return errors.New("client native identity does not match its provider construction")
		}
		return nil
	}
	if identity == nil {
		return errors.New("client native provider requires a captured native_identity declaration")
	}
	if kind == providerregistry.ConstructionCodex {
		return identity.ValidateCodex()
	}
	return identity.ValidateGemini()
}

func (capture *nativeIdentityCapture) peek(kind providerregistry.Construction) (NativeIdentity, bool) {
	if capture == nil {
		return NativeIdentity{}, false
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	identity, ok := capture.resolved[kind]
	return identity, ok
}

func (capture *nativeIdentityCapture) resolve(ctx context.Context, kind providerregistry.Construction) (NativeIdentity, error) {
	if capture == nil {
		return NativeIdentity{}, errors.New("native identity capture is unavailable")
	}
	for {
		if err := ctx.Err(); err != nil {
			return NativeIdentity{}, err
		}
		capture.mu.Lock()
		if identity, ok := capture.resolved[kind]; ok {
			capture.mu.Unlock()
			return identity, nil
		}
		if done := capture.running[kind]; done != nil {
			capture.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return NativeIdentity{}, ctx.Err()
			}
		}
		done := make(chan struct{})
		capture.running[kind] = done
		capture.mu.Unlock()
		bound := oauth.ContextWithEnvironment(ctx, capture.environment)
		var identity NativeIdentity
		var err error
		switch kind {
		case providerregistry.ConstructionCodex:
			identity, err = useragent.ResolveCodexIdentity(bound)
		case providerregistry.ConstructionGeminiAntigravity:
			identity.UserAgent, err = useragent.GeminiForContext(bound)
		default:
			err = errors.New("provider has no native identity")
		}
		if err == nil {
			err = validateNativeIdentity(kind, &identity)
		}
		if err == nil {
			err = ctx.Err()
		}
		capture.mu.Lock()
		if err == nil {
			capture.resolved[kind] = identity
		}
		delete(capture.running, kind)
		close(done)
		capture.mu.Unlock()
		return identity, err
	}
}

// prepareNativeIdentities runs before collection acquires config/account locks.
// All later exported definitions and refresh digests reuse this exact value.
func (snapshot RuntimeSnapshot) prepareNativeIdentities(ctx context.Context) error {
	if snapshot.IsClientOwned() {
		return errors.New("collect runtime on its owning client")
	}
	if snapshot.config == nil {
		return errors.New("captured provider runtime is required")
	}
	selected := make(map[string]bool)
	for _, model := range snapshot.config.Models {
		selected[model.Provider] = true
	}
	if images := snapshot.config.Images; images != nil {
		for _, image := range images.Providers {
			for _, owner := range image.Credentials {
				selected[owner.ProviderID] = true
			}
		}
	}
	for id := range selected {
		if snapshot.config.providerLoadIssue(id) != nil {
			continue
		}
		_, owner, err := snapshot.clientProviderDefinitionRaw(id)
		if err != nil {
			return err
		}
		if nativeConstruction(owner.Construction) {
			if _, err := snapshot.nativeIdentities.resolve(ctx, owner.Construction); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// ClientNativeIdentity reads only the admitted private definition. Missing or
// invalid metadata is an error; it never invokes execution-host discovery.
func (snapshot RuntimeSnapshot) ClientNativeIdentity(id string) (NativeIdentity, error) {
	if snapshot.clientRuntime == nil {
		return NativeIdentity{}, errors.New("client native identity authority is unavailable")
	}
	for _, definition := range snapshot.clientRuntime.proposal.Providers {
		if definition.Config.ID != id {
			continue
		}
		if definition.Config.Owner == nil || !nativeConstruction(definition.Config.Owner.Construction) {
			break
		}
		if err := validateNativeIdentity(definition.Config.Owner.Construction, definition.NativeIdentity); err != nil {
			return NativeIdentity{}, err
		}
		return *definition.NativeIdentity, nil
	}
	return NativeIdentity{}, errors.New("captured client native identity is unavailable")
}

func (snapshot RuntimeSnapshot) contextWithClientNativeIdentity(ctx context.Context, owner providerregistry.RegistrationOwner) (context.Context, error) {
	if !snapshot.IsClientOwned() || !nativeConstruction(owner.Construction) {
		return ctx, nil
	}
	identity, err := snapshot.ClientNativeIdentity(owner.ProviderID)
	if err != nil {
		return nil, err
	}
	switch owner.Construction {
	case providerregistry.ConstructionCodex:
		return useragent.ContextWithCodexIdentity(ctx, identity)
	default:
		return useragent.ContextWithGeminiIdentity(ctx, identity)
	}
}

func environmentEntries(environment env.Env) []string {
	if environment == nil {
		return nil
	}
	return environment.Env()
}
