package proto

import "github.com/example-git/crux/internal/providerauth"

// These aliases expose the exact service request/status types to generated API
// documentation. They do not introduce a second wire shape or conversion.
type ProviderAuthenticationSnapshot = providerauth.Snapshot
type ProviderAuthenticationTarget = providerauth.Target
type ProviderAccountsState = providerauth.AccountsState
type ProviderAccountSwitchRequest = providerauth.SwitchRequest
type ProviderLogoutRequest = providerauth.LogoutRequest
type ProviderAccountRemoveRequest = providerauth.RemoveRequest
type ProviderAPIKeyCheckRequest = providerauth.APIKeyCheckRequest
type ProviderAPIKeySaveRequest = providerauth.APIKeySaveRequest
type ProviderOAuthLoginRequest = providerauth.OAuthLoginRequest
type ProviderOAuthLoginBindRequest = providerauth.OAuthLoginBindRequest
type ProviderOAuthLoginCodeRequest = providerauth.OAuthLoginCodeRequest
type ProviderLocalRepairRequest = providerauth.LocalRepairRequest
