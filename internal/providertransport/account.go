package providertransport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

func AccountIdentity(operation *Operation, credentials []manifest.Credential) func(context.Context, string) (string, string, json.RawMessage) {
	return func(ctx context.Context, accessToken string) (string, string, json.RawMessage) {
		id, display, raw, err := ExecuteAccountIdentity(ctx, operation, credentials, accessToken)
		if err != nil {
			return "", "", nil
		}
		return id, display, raw
	}
}

func ExecuteAccountIdentity(ctx context.Context, operation *Operation, credentials []manifest.Credential, accessToken string) (id string, display string, raw json.RawMessage, err error) {
	if operation == nil || operation.Kind != "account" {
		return "", "", nil, fmt.Errorf("account operation is unavailable")
	}
	if err := ValidateContextOwner(ctx); err != nil {
		return "", "", nil, err
	}
	started := time.Now()
	diagnostic := OperationDiagnostic{ID: operation.ID, Kind: operation.Kind}
	defer func() {
		diagnostic.Duration = time.Since(started)
		diagnostic.Failed = err != nil
		RecordOperationDiagnostic(ctx, diagnostic)
	}()

	credentialValues := map[string]string{}
	for _, credential := range credentials {
		if credential.Kind == "oauth2" || credential.Kind == "bearer" {
			credentialValues[credential.ID] = accessToken
		}
	}
	values := TemplateValues{Credentials: credentialValues, Context: map[string]string{"oauth.access_token": accessToken, "client.user_agent": "Crux"}}
	headers, err := operation.ApplyHeadersWithValues(nil, values)
	if err != nil {
		return "", "", nil, err
	}
	base, err := url.Parse(operation.Endpoint.BaseURL)
	if err != nil {
		return "", "", nil, err
	}
	reference, err := url.Parse(operation.Path)
	if err != nil {
		return "", "", nil, err
	}
	var body io.Reader
	if operation.RequestTransform != nil {
		document := map[string]any{}
		if err := ApplyJSONPipeline(document, operation.RequestTransform, values); err != nil {
			return "", "", nil, fmt.Errorf("account request transform: %w", err)
		}
		data, err := json.Marshal(document)
		if err != nil {
			return "", "", nil, err
		}
		if len(data) > 1<<20 {
			return "", "", nil, fmt.Errorf("account request exceeds host limit")
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, operation.Method, base.ResolveReference(reference).String(), body)
	if err != nil {
		return "", "", nil, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := ClientWithContextOwnerValidator(ctx, operation.HTTPClient(nil))
	response, err := client.Do(request)
	if err != nil {
		return "", "", nil, err
	}
	defer response.Body.Close()
	diagnostic.StatusCode = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", nil, fmt.Errorf("account operation returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil {
		return "", "", nil, err
	}
	if len(data) > 1<<20 {
		return "", "", nil, fmt.Errorf("account response exceeds host limit")
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return "", "", nil, err
	}
	if err := ApplyJSONPipeline(document, operation.ResponseTransform, TemplateValues{}); err != nil {
		return "", "", nil, err
	}
	id = identityString(document, "/id")
	display = identityString(document, "/display_name")
	if id == "" {
		return "", "", nil, fmt.Errorf("account operation returned no identity")
	}
	raw, err = json.Marshal(document)
	if err != nil {
		return "", "", nil, err
	}
	if err := ValidateContextOwner(ctx); err != nil {
		return "", "", nil, err
	}
	return id, display, raw, nil
}

func identityString(document any, pointer string) string {
	value := JSONPointer(document, pointer)
	if value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return fmt.Sprint(value)
	}
	return text
}
