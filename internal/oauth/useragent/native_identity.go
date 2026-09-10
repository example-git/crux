package useragent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
)

// NativeIdentity declares finite, resolved native header defaults. Existing
// provider-specific extra-header precedence remains unchanged. It contains no
// environment, executable path or credentials.
type NativeIdentity struct {
	UserAgent  string `json:"user_agent"`
	Version    string `json:"version,omitempty"`
	Originator string `json:"originator,omitempty"`
}

func (identity NativeIdentity) ValidateCodex() error {
	if !identityHeader(identity.UserAgent, 2048) || !identityHeader(identity.Version, 128) || !identityHeader(identity.Originator, 256) || !strings.HasPrefix(identity.UserAgent, identity.Originator+"/"+identity.Version+" (") {
		return errors.New("captured Codex native identity is invalid")
	}
	return nil
}

func (identity NativeIdentity) ValidateGemini() error {
	if !identityHeader(identity.UserAgent, 2048) || !strings.HasPrefix(identity.UserAgent, "antigravity/cli/") || identity.Version != "" || identity.Originator != "" {
		return errors.New("captured Gemini native identity is invalid")
	}
	return nil
}

func identityHeader(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, value := range []byte(value) {
		if value < 32 || value == 127 {
			return false
		}
	}
	return true
}

type (
	codexIdentityKey  struct{}
	geminiIdentityKey struct{}
)

// ContextWithCodexIdentity binds an already resolved identity, including the
// absence of all execution-host overrides. Request cancellation is unchanged.
func ContextWithCodexIdentity(ctx context.Context, identity NativeIdentity) (context.Context, error) {
	if err := identity.ValidateCodex(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, codexIdentityKey{}, identity), nil
}

func ContextWithGeminiIdentity(ctx context.Context, identity NativeIdentity) (context.Context, error) {
	if err := identity.ValidateGemini(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, geminiIdentityKey{}, identity), nil
}

// ResolveCodexIdentity resolves the version once so the UA and separate version
// header cannot observe different metadata releases within one capture.
func ResolveCodexIdentity(ctx context.Context) (NativeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return NativeIdentity{}, err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return NativeIdentity{}, err
	}
	if identity, ok := ctx.Value(codexIdentityKey{}).(NativeIdentity); ok {
		return identity, nil
	}
	version, err := CodexVersionForContext(ctx)
	if err != nil {
		return NativeIdentity{}, err
	}
	osRelease, err := osVersionForContext(ctx)
	if err != nil {
		return NativeIdentity{}, err
	}
	originator := CodexOriginatorForContext(ctx)
	identity := NativeIdentity{Version: version, Originator: originator, UserAgent: fmt.Sprintf("%s/%s (%s %s; %s) %s", originator, version, codexOSType(), osRelease, runtime.GOARCH, codexTerminalTokenForContext(ctx))}
	return identity, nil
}

// CodexRequestUserAgent preserves unbound server behavior while consuming an
// explicit client identity or an OAuth environment when the caller bound one.
func CodexRequestUserAgent(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return "", err
	}
	if identity, ok := ctx.Value(codexIdentityKey{}).(NativeIdentity); ok {
		return identity.UserAgent, nil
	}
	if _, bound := oauth.EnvironmentFromContext(ctx); bound {
		return CodexForContext(ctx)
	}
	return Codex(), nil
}
