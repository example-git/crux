// Package providerauth exposes credential-free, generation-bound provider
// authentication state. Credentials and account-store routing stay on the owner.
package providerauth

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/providerregistry"
)

// Owner deliberately omits the private account namespace. Generation binds this
// public registration reference to the complete owner in the service capture.
type Owner struct {
	ProviderID           string                        `json:"provider_id"`
	Construction         providerregistry.Construction `json:"construction,omitempty"`
	CompatibilityAdapter providerregistry.Construction `json:"compatibility_adapter,omitempty"`
	HasOAuth             bool                          `json:"has_oauth,omitempty"`
	OAuthAdapter         providerregistry.LoginAdapter `json:"oauth_adapter,omitempty"`
	OAuthFlowID          string                        `json:"oauth_flow_id,omitempty"`
	HasManifest          bool                          `json:"has_manifest,omitempty"`
	ManifestID           string                        `json:"manifest_id,omitempty"`
	ManifestVersion      string                        `json:"manifest_version,omitempty"`
	HasPreset            bool                          `json:"has_preset,omitempty"`
	PresetID             string                        `json:"preset_id,omitempty"`
	PresetVersion        string                        `json:"preset_version,omitempty"`
	PresetDigest         string                        `json:"preset_digest,omitempty"`
}

func PublicOwner(owner providerregistry.RegistrationOwner) Owner {
	return Owner{
		ProviderID: owner.ProviderID, Construction: owner.Construction,
		CompatibilityAdapter: owner.CompatibilityAdapter, HasOAuth: owner.HasOAuth,
		OAuthAdapter: owner.OAuthAdapter, OAuthFlowID: owner.OAuthFlowID,
		HasManifest: owner.HasManifest, ManifestID: owner.ManifestID, ManifestVersion: owner.ManifestVersion,
		HasPreset: owner.HasPreset, PresetID: owner.PresetID, PresetVersion: owner.PresetVersion, PresetDigest: owner.PresetDigest,
	}
}

type Generation struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence"`
}

type Target struct {
	WorkspaceID string     `json:"workspace_id"`
	Owner       Owner      `json:"owner"`
	Generation  Generation `json:"generation"`
}

// A configured API key may still be a shell expression. Status never resolves
// it or asserts verified provider access. OAuth presence excludes expiry checks:
// expired access remains present, and may remain refreshable.
type CredentialStatus struct {
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Refreshable bool   `json:"refreshable"`
}

type Status struct {
	Owner           Owner              `json:"owner"`
	Configured      bool               `json:"configured"`
	Disabled        bool               `json:"disabled"`
	Credentials     []CredentialStatus `json:"credentials"`
	AccountState    string             `json:"account_state"`
	ActiveAccountID string             `json:"active_account_id,omitempty"`
}

type Snapshot struct {
	WorkspaceID string     `json:"workspace_id"`
	Generation  Generation `json:"generation"`
	Providers   []Status   `json:"providers"`
}

type AccountSummary struct {
	ExpiresAt       int64  `json:"expires_at,omitempty"` // Unix milliseconds; nonpositive means no recorded expiry.
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	Active          bool   `json:"active"`
	CredentialState string `json:"credential_state"`
	Refreshable     bool   `json:"refreshable"`
}

type AccountsState struct {
	Target   Target           `json:"target"`
	Status   Status           `json:"status"`
	Accounts []AccountSummary `json:"accounts"`
}

var (
	ErrStale = errors.New("provider authentication changed; reload authentication status")
	ErrOwner = errors.New("provider authentication owner is unavailable or changed")
)

func validText(value string, max int, required bool) bool {
	return len(value) <= max && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n") && (!required || strings.TrimSpace(value) != "")
}

func (g Generation) Validate() error {
	if len(g.Epoch) != 32 || g.Sequence == 0 || strings.IndexFunc(g.Epoch, func(r rune) bool { return !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') }) >= 0 {
		return errors.New("invalid provider authentication generation")
	}
	return nil
}

func (o Owner) Validate() error {
	if !validText(o.ProviderID, 512, true) {
		return errors.New("invalid provider authentication owner")
	}
	for _, value := range []string{string(o.Construction), string(o.CompatibilityAdapter), string(o.OAuthAdapter), o.OAuthFlowID, o.ManifestID, o.ManifestVersion, o.PresetID, o.PresetVersion, o.PresetDigest} {
		if !validText(value, 1024, false) {
			return errors.New("invalid provider authentication owner")
		}
	}
	if !o.HasOAuth && (o.OAuthAdapter != "" || o.OAuthFlowID != "") || !o.HasManifest && (o.ManifestID != "" || o.ManifestVersion != "") || o.HasManifest && (o.ManifestID == "" || o.ManifestVersion == "") || !o.HasPreset && (o.PresetID != "" || o.PresetVersion != "" || o.PresetDigest != "") || o.HasPreset && (o.PresetID == "" || o.PresetVersion == "" || o.PresetDigest == "") {
		return errors.New("inconsistent provider authentication owner")
	}
	return nil
}

func (t Target) Validate() error {
	if !validText(t.WorkspaceID, 512, true) {
		return errors.New("invalid authentication workspace")
	}
	if err := t.Owner.Validate(); err != nil {
		return err
	}
	return t.Generation.Validate()
}

func (s Status) Validate() error {
	if err := s.Owner.Validate(); err != nil {
		return err
	}
	if !validText(s.ActiveAccountID, 4096, false) || !s.Configured && s.Disabled {
		return errors.New("invalid provider authentication status")
	}
	switch s.AccountState {
	case "none":
		if s.ActiveAccountID != "" {
			return errors.New("unexpected active authentication account")
		}
	case "in-sync":
		if !s.Configured || s.ActiveAccountID == "" {
			return errors.New("inconsistent active authentication account")
		}
	case "out-of-sync":
	default:
		return errors.New("invalid authentication account state")
	}
	if len(s.Credentials) != 2 {
		return errors.New("incomplete provider credential status")
	}
	seen := map[string]bool{}
	for _, c := range s.Credentials {
		if seen[c.Kind] {
			return errors.New("duplicate provider credential status")
		}
		seen[c.Kind] = true
		switch c.Kind {
		case "api-key":
			if c.Refreshable || c.State != "absent" && c.State != "configured" {
				return errors.New("invalid API key status")
			}
		case "oauth":
			if !validCredentialState(c.State, c.Refreshable) {
				return errors.New("invalid OAuth status")
			}
		default:
			return errors.New("invalid provider credential kind")
		}
		if !s.Configured && c.State != "absent" {
			return errors.New("unconfigured provider has configured credentials")
		}
	}
	return nil
}

func validCredentialState(state string, refreshable bool) bool {
	return state == "present" || state == "refresh-only" && refreshable || state == "absent" && !refreshable
}

func (s Snapshot) Validate() error {
	if !validText(s.WorkspaceID, 512, true) {
		return errors.New("invalid authentication workspace")
	}
	if err := s.Generation.Validate(); err != nil {
		return err
	}
	if s.Providers == nil || len(s.Providers) > 4096 {
		return errors.New("invalid authentication provider list")
	}
	seen := map[string]bool{}
	for _, p := range s.Providers {
		if err := p.Validate(); err != nil {
			return err
		}
		if seen[p.Owner.ProviderID] {
			return errors.New("duplicate authentication provider")
		}
		seen[p.Owner.ProviderID] = true
	}
	return nil
}

func (s AccountsState) Validate() error {
	if err := s.Target.Validate(); err != nil {
		return err
	}
	if err := s.Status.Validate(); err != nil {
		return err
	}
	if s.Target.Owner != s.Status.Owner || s.Accounts == nil || len(s.Accounts) > 4096 {
		return errors.New("invalid provider account list")
	}
	seen := map[string]bool{}
	active := false
	for _, a := range s.Accounts {
		if !validText(a.ID, 4096, true) || !validText(a.DisplayName, 4096, false) || seen[a.ID] || !validCredentialState(a.CredentialState, a.Refreshable) {
			return errors.New("invalid provider account summary")
		}
		seen[a.ID] = true
		if a.Active != (a.ID == s.Status.ActiveAccountID) || a.Active && active {
			return errors.New("inconsistent provider account selection")
		}
		active = active || a.Active
	}
	if s.Status.AccountState == "in-sync" && !active {
		return errors.New("active provider account is missing")
	}
	return nil
}
