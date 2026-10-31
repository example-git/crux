package providerregistry

import (
	"context"
	"encoding/json"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/providertransport/clientidentity"
)

func ClientIdentityForOperation(operation *providertransport.Operation) *manifest.ResolvedClientIdentity {
	if operation == nil {
		return nil
	}
	if operation.Anthropic != nil {
		return operation.Anthropic.ClientIdentity
	}
	return operation.ClientIdentity
}

func ExecuteAccountOperation(ctx context.Context, operation *providertransport.Operation, credentials []manifest.Credential, accessToken string) (string, string, json.RawMessage, error) {
	declaration := ClientIdentityForOperation(operation)
	var version, userAgent string
	var err error
	if environment, captured := oauth.EnvironmentFromContext(ctx); captured {
		version, userAgent, err = clientidentity.ResolveWithEnvironment(ctx, declaration, environment)
	} else {
		version, userAgent, err = clientidentity.ResolveForContext(ctx, declaration)
	}
	if err != nil {
		return "", "", nil, err
	}
	values := map[string]string{}
	if userAgent != "" {
		values["client.user_agent"] = userAgent
		values["client.version"] = version
	}
	return providertransport.ExecuteAccountIdentity(ctx, operation, credentials, accessToken, values)
}
