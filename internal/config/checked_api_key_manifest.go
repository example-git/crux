package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/providertransport/clientidentity"
)

// No native endpoint is inferred from a protocol or provider ID. The closed
// manifest must declare one unambiguous catalog operation; inference calls are
// never used as a connection probe.
var errCheckedCatalogNotDeclared = errors.New("checked API key connection policy is not implemented without a declared model-catalog operation")

func checkedAPIKeyProbeOperation(snapshot RuntimeSnapshot, provider ProviderConfig) (*providertransport.Operation, error) {
	if provider.Owner.Construction == providerregistry.ConstructionOpenAICompat {
		return nil, nil
	}
	registration, ok := providerDeclaredRegistrationForProvider(snapshot.registry, provider.ID, provider)
	if !ok || registration.Manifest == nil {
		return nil, errors.New("checked API key connection policy is not implemented for this provider construction")
	}
	var selected *providertransport.Operation
	for _, operation := range registration.Operations {
		if operation == nil || operation.Kind != "model-catalog" {
			continue
		}
		if selected != nil {
			return nil, errors.New("checked API key requires an unambiguous declared model-catalog operation")
		}
		selected = operation
	}
	if selected == nil {
		return nil, errCheckedCatalogNotDeclared
	}
	if err := providerregistry.ValidateModelCatalogOperation(selected); err != nil {
		return nil, err
	}
	return selected.Clone(), nil
}

func checkedCredentialCatalogAudience(snapshot RuntimeSnapshot, provider ProviderConfig, slot ProviderCredentialSlot, operation *providertransport.Operation) bool {
	registration, ok := providerDeclaredRegistrationForProvider(snapshot.registry, provider.ID, provider)
	if !ok || registration.Manifest == nil || operation == nil {
		return false
	}
	for _, credential := range registration.Manifest.Capabilities.Credentials {
		if "configuration."+credential.ID == slot.ID && credential.ConfigProperty == slot.Property {
			return slices.Contains(credential.Audience, operation.Endpoint.ID)
		}
	}
	return false
}

func checkedAPIKeyManifestValues(snapshot RuntimeSnapshot, provider ProviderConfig, operation *providertransport.Operation) (providertransport.TemplateValues, error) {
	values := providertransport.TemplateValues{Config: cloneProviderOptions(provider.Configuration), Credentials: map[string]string{}, Context: map[string]string{"client.user_agent": "Crux"}}
	registration, ok := snapshot.ProviderRegistrationFor(provider.ID, provider)
	if !ok || registration.Manifest == nil {
		return values, errors.New("checked provider manifest is unavailable")
	}
	credentialProperties := map[string]bool{}
	for _, credential := range registration.Manifest.Capabilities.Credentials {
		if credential.ConfigProperty != "" {
			credentialProperties[credential.ConfigProperty] = credentialProperties[credential.ConfigProperty] || slices.Contains(credential.Audience, operation.Endpoint.ID)
		}
	}
	for property, allowed := range credentialProperties {
		if !allowed {
			delete(values.Config, property)
		}
	}
	for _, credential := range registration.Manifest.Capabilities.Credentials {
		// A credential cannot be made available to an undeclared audience just
		// because another operation of the same plugin may use it.
		if !slices.Contains(credential.Audience, operation.Endpoint.ID) {
			continue
		}
		if credential.ConfigProperty != "" {
			text, ok := provider.Configuration[credential.ConfigProperty].(string)
			if !ok || text == "" {
				return values, errors.New("checked provider credential configuration is unavailable")
			}
			values.Credentials[credential.ID] = text
			continue
		}
		switch credential.Kind {
		case "oauth2", "bearer", "api-key":
			values.Credentials[credential.ID] = provider.APIKey
			values.Context["oauth.access_token"] = provider.APIKey
		case "none":
		default:
			return values, errors.New("checked provider credential kind is unsupported")
		}
	}
	return values, nil
}

func probeCheckedAPIKeyManifest(ctx context.Context, snapshot RuntimeSnapshot, provider ProviderConfig, operation *providertransport.Operation, validateOwner func(context.Context) error, slots ...ProviderCredentialSlot) (result ConnectionProbeResult, err error) {
	result = ConnectionProbeResult{Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicyManifestHTTP200}
	// The check budget includes identity resolution, transforms and the body.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	validate := func() error { return validateOwner(ctx) }
	ctx = providertransport.ContextWithOwnerValidator(ctx, validate)
	if err := validate(); err != nil {
		return result, err
	}
	endpoint, err := operation.ResolveEndpoint(provider.BaseURL)
	if err != nil {
		return result, err
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return result, err
	}
	reference, err := url.Parse(operation.Path)
	if err != nil || reference.IsAbs() || reference.Host != "" || strings.Contains(operation.Path, "{model}") {
		return result, errors.New("checked catalog path is invalid or requires a model")
	}
	target := base.ResolveReference(reference)
	allowed := func(destination *url.URL) bool {
		fold := func(values []string, value string) bool {
			return slices.ContainsFunc(values, func(candidate string) bool { return strings.EqualFold(candidate, value) })
		}
		return destination != nil && destination.User == nil && destination.Fragment == "" && fold(operation.Endpoint.AllowedSchemes, destination.Scheme) && fold(operation.Endpoint.AllowedHosts, destination.Hostname())
	}
	if !allowed(target) {
		return result, errors.New("checked catalog destination violates its allowlist")
	}
	values, err := checkedAPIKeyManifestValues(snapshot, provider, operation)
	if err != nil {
		return result, err
	}
	version, userAgent, err := clientidentity.ResolveWithEnvironment(ctx, operation.ClientIdentity, snapshot.Environment())
	if err != nil {
		return result, err
	}
	if userAgent != "" {
		values.Context["client.user_agent"] = userAgent
		values.Context["client.version"] = version
	}
	var body io.Reader
	if operation.RequestTransform != nil {
		document := map[string]any{}
		requestValues := values
		requestValues.Config = cloneProviderOptions(values.Config)
		if err := providertransport.ApplyJSONPipeline(document, operation.RequestTransform, requestValues); err != nil {
			return result, err
		}
		data, err := json.Marshal(document)
		if err != nil || len(data) > 1<<20 {
			return result, errors.New("checked catalog request is invalid or exceeds its limit")
		}
		body = bytes.NewReader(data)
	}
	headers := make(map[string]string, len(provider.ExtraHeaders))
	for name, value := range provider.ExtraHeaders {
		canonical := http.CanonicalHeaderKey(name)
		if _, duplicate := headers[canonical]; duplicate {
			return result, errors.New("checked catalog headers contain ambiguous casing")
		}
		headers[canonical] = value
	}
	if _, configured := headers["Accept"]; !configured {
		headers["Accept"] = "application/json"
	}
	if body != nil {
		if _, configured := headers["Content-Type"]; !configured {
			headers["Content-Type"] = "application/json"
		}
	}
	headers, entered, overridden, err := checkedAPIKeyCatalogHeaders(operation, headers, values, snapshot, provider, slots...)
	if err != nil {
		return result, err
	}
	request, err := http.NewRequestWithContext(ctx, operation.Method, target.String(), body)
	if err != nil {
		return result, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := operation.HTTPClient(http.DefaultClient)
	priorRedirect := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if err := validate(); err != nil {
			return err
		}
		if !allowed(next.URL) {
			return errors.New("checked catalog redirect violates its allowlist")
		}
		if priorRedirect != nil {
			return priorRedirect(next, via)
		}
		if len(via) >= 10 {
			return errors.New("checked catalog redirect limit reached")
		}
		return nil
	}
	client.Transport = providertransport.TransportWithConnectTimeout(client.Transport, operation.ConnectTimeout)
	observed := &catalogProbeTransport{base: client.Transport}
	client.Transport = observed
	client = providertransport.ClientWithContextOwnerValidator(ctx, client)
	result.Kind = ConnectionProbeHTTPAttempt
	result.EnteredKeyInAuthorization, result.AuthorizationOverridden = entered, overridden
	response, err := providertransport.DoWithRetry(request, client, operation.Retry, operation.Errors)
	if observed.status != 0 {
		result.Kind, result.HTTPStatus = ConnectionProbeHTTPResponse, observed.status
	}
	if response != nil {
		result.Kind, result.HTTPStatus = ConnectionProbeHTTPResponse, response.StatusCode
		defer response.Body.Close()
	}
	if err != nil {
		return result, err
	}
	if err := validate(); err != nil {
		return result, err
	}
	if response.StatusCode != http.StatusOK {
		return result, errors.New("checked catalog did not return HTTP 200")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return result, errors.New("checked catalog response is unreadable or exceeds its limit")
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return result, errors.New("checked catalog response is not JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("checked catalog response contains trailing data")
	}
	responseValues := values
	responseValues.Config = cloneProviderOptions(values.Config)
	if err := providertransport.ApplyJSONPipeline(document, operation.ResponseTransform, responseValues); err != nil {
		return result, err
	}
	transformed, err := json.Marshal(document)
	if err != nil || len(transformed) > 1<<20 {
		return result, errors.New("checked catalog transformed response is invalid or exceeds its limit")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, validate()
}

// Retry cancellation can return no final response after a previous HTTP
// failure. Retain that observed status instead of claiming no response existed.
type catalogProbeTransport struct {
	base   http.RoundTripper
	status int
}

func (t *catalogProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(request)
	if response != nil {
		t.status = response.StatusCode
	}
	return response, err
}

// Evaluate each header template once, while tracking whether the final
// Authorization header came from the entered slot. Equal bytes from an
// unrelated configured value do not establish that provenance.
func checkedAPIKeyCatalogHeaders(operation *providertransport.Operation, headers map[string]string, values providertransport.TemplateValues, snapshot RuntimeSnapshot, provider ProviderConfig, slots ...ProviderCredentialSlot) (map[string]string, bool, bool, error) {
	registration, ok := snapshot.ProviderRegistrationFor(provider.ID, provider)
	if !ok || registration.Manifest == nil {
		return nil, false, false, errors.New("checked provider manifest is unavailable")
	}
	enteredCredentials := map[string]bool{}
	slot := ProviderCredentialSlot{ID: "provider.api_key"}
	if len(slots) > 0 {
		slot = slots[0]
	}
	for _, credential := range registration.Manifest.Capabilities.Credentials {
		if credential.ConfigProperty == slot.Property && credential.Kind != "none" && values.Credentials[credential.ID] != "" {
			enteredCredentials[credential.ID] = true
		}
	}
	var usesInput func(manifest.Template) bool
	usesInput = func(value manifest.Template) bool {
		if value.Kind == "credential" {
			return enteredCredentials[value.Ref]
		}
		if value.Kind == "context" {
			return slot.Property == "" && value.Ref == "oauth.access_token" && values.Context[value.Ref] != ""
		}
		if value.Kind == "config" {
			return slot.Property != "" && value.Ref == slot.Property
		}
		return value.Kind == "concat" && slices.ContainsFunc(value.Parts, usesInput)
	}
	authorization := func(headers map[string]string) (string, bool) {
		for name, value := range headers {
			if strings.EqualFold(name, "Authorization") {
				return value, true
			}
		}
		return "", false
	}
	_, foreign := authorization(headers)
	entered := false
	for _, rule := range operation.Headers {
		before, present := authorization(headers)
		original := rule.Value
		if original != nil {
			value, err := providertransport.EvaluateTemplate(*original, values)
			if err != nil {
				return nil, false, false, err
			}
			rule.Value = &manifest.Template{Kind: "literal", Value: value}
		}
		one := *operation
		one.Headers = []manifest.HeaderRule{rule}
		var err error
		headers, err = one.ApplyHeadersWithValues(headers, values)
		if err != nil {
			return nil, false, false, err
		}
		if !strings.EqualFold(rule.Name, "Authorization") {
			continue
		}
		after, _ := authorization(headers)
		input := original != nil && usesInput(*original)
		switch rule.Operation {
		case "delete":
			entered, foreign = false, true
		case "set":
			entered, foreign = input && after != "", !input
		case "set-if-absent":
			if !present {
				entered, foreign = input && after != "", !input
			}
		case "append", "append-unique":
			if before != after {
				if !present {
					entered, foreign = input, !input
				} else {
					entered = entered || input
					foreign = foreign || !input
				}
			}
		}
	}
	return headers, entered && !foreign, foreign, nil
}
