package localaddon

func codexLegacyAuthStatusResult() map[string]any {
	return map[string]any{"authMethod": "apikey", "authToken": nil, "requiresOpenaiAuth": false}
}

func codexGatewayOAuthReadResult() map[string]any {
	return map[string]any{"providerId": "crux", "providerName": "Crux", "required": false, "status": nil, "error": nil}
}
