package config

// ResolveProviderAPIKey preserves a proven OAuth access token byte-for-byte.
// Ordinary configured API-key expressions retain their resolver semantics.
func ResolveProviderAPIKey(provider ProviderConfig, resolve func(string) (string, error)) (string, error) {
	if providerHasLiteralOAuthCredential(provider) {
		return provider.APIKey, nil
	}
	return resolve(provider.APIKey)
}

func providerHasLiteralOAuthCredential(provider ProviderConfig) bool {
	return provider.OAuthToken != nil && provider.APIKey == provider.OAuthToken.AccessToken
}
