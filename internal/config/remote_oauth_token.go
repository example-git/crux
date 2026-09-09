package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
)

// Namespace-free OAuth preserves the complete saved token without inventing an
// account-store identity. The bound applies to its finite token/client object,
// in addition to the complete proposal's transport size limit.
const maxRemoteOAuthTokenBytes = 1 << 20

func validateRemoteOAuthToken(token *oauth.Token) error {
	if token == nil || token.AccessToken == "" {
		return errors.New("client OAuth token requires an access token")
	}
	encoded, err := json.Marshal(token)
	if err != nil || len(encoded) > maxRemoteOAuthTokenBytes {
		return errors.New("client OAuth token exceeds its byte limit")
	}
	return nil
}

// OAuthTokenCredentialID binds all saved token and client-registration fields.
// It is a content identity, never authorization to replace a credential.
func OAuthTokenCredentialID(token *oauth.Token) string {
	if token == nil {
		return ""
	}
	encoded, err := json.Marshal(token)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (RemoteCredentialBinding) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private remote credential binding]"))
}

// ClientOAuthToken returns only the exact accepted namespace-free binding. It
// never reads local account stores or resolves receiver environment values.
func (s RuntimeSnapshot) ClientOAuthToken(owner providerregistry.RegistrationOwner) (*oauth.Token, bool) {
	if !s.IsClientOwned() || !owner.HasOAuth || owner.AccountNamespace != "" {
		return nil, false
	}
	for _, binding := range s.clientRuntime.proposal.Credentials {
		if binding.Owner == owner && !binding.Unavailable && binding.OAuthToken != nil {
			return cloneOAuthToken(binding.OAuthToken), true
		}
	}
	return nil, false
}

func (s RuntimeSnapshot) clientRefreshCredential(owner providerregistry.RegistrationOwner) (accountID, credentialID string, ok bool) {
	if owner.AccountNamespace == "" {
		token, present := s.ClientOAuthToken(owner)
		if !present {
			return "", "", false
		}
		return "", OAuthTokenCredentialID(token), true
	}
	account, present := s.EphemeralAccount(owner)
	if !present || account.ID == "" {
		return "", "", false
	}
	return account.ID, accounts.CredentialID(*account), true
}
