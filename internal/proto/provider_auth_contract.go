package proto

import "github.com/example-git/crux/internal/providerauth"

// These aliases expose the exact service request/status types to generated API
// documentation. They do not introduce a second wire shape or conversion.
type (
	ProviderAuthenticationSnapshot = providerauth.Snapshot
	ProviderAuthenticationTarget   = providerauth.Target
	ProviderAccountsState          = providerauth.AccountsState
	ProviderAccountSwitchRequest   = providerauth.SwitchRequest
	ProviderLogoutRequest          = providerauth.LogoutRequest
	ProviderAccountRemoveRequest   = providerauth.RemoveRequest
	ProviderAPIKeyCheckRequest     = providerauth.APIKeyCheckRequest
	ProviderAPIKeySaveRequest      = providerauth.APIKeySaveRequest
	ProviderOAuthLoginRequest      = providerauth.OAuthLoginRequest
	ProviderOAuthLoginBindRequest  = providerauth.OAuthLoginBindRequest
	ProviderOAuthLoginCodeRequest  = providerauth.OAuthLoginCodeRequest
	ProviderLocalRepairRequest     = providerauth.LocalRepairRequest
)
