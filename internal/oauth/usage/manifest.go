package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/providertransport/clientidentity"
)

// ManifestFetcher builds a host-owned usage executor from a compiled operation.
type manifestUsageOperation struct {
	operation        *providertransport.Operation
	target           *url.URL
	client           *http.Client
	identityMu       sync.Mutex
	identityResolved bool
	userAgent        string
}

type manifestUsageSetup struct {
	declaration manifest.UsageSetup
	operation   *manifestUsageOperation
}

type usageConnectTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

type usageCancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (transport usageConnectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := transport.base
	if base == nil {
		base = http.DefaultTransport
	}
	if transport.timeout <= 0 {
		return base.RoundTrip(request)
	}
	ctx, cancel := context.WithCancelCause(request.Context())
	timeoutErr := fmt.Errorf("connect timeout after %s", transport.timeout)
	timer := time.AfterFunc(transport.timeout, func() { cancel(timeoutErr) })
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { timer.Stop() },
	})
	response, err := base.RoundTrip(request.Clone(ctx))
	timer.Stop()
	if err != nil {
		cause := context.Cause(ctx)
		cancel(nil)
		if cause == timeoutErr {
			return response, timeoutErr
		}
		return response, err
	}
	if response == nil || response.Body == nil {
		cancel(nil)
		return response, nil
	}
	response.Body = &usageCancelReadCloser{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func (body *usageCancelReadCloser) Close() error {
	err := body.ReadCloser.Close()
	body.cancel(nil)
	return err
}

func compileManifestUsageOperation(operation *providertransport.Operation) (*manifestUsageOperation, error) {
	operation = operation.Clone()
	base, err := url.Parse(operation.Endpoint.BaseURL)
	if err != nil {
		return nil, err
	}
	reference, err := url.Parse(operation.Path)
	if err != nil {
		return nil, err
	}
	target := base.ResolveReference(reference)
	if !containsFoldUsage(operation.Endpoint.AllowedSchemes, target.Scheme) || !containsFoldUsage(operation.Endpoint.AllowedHosts, target.Hostname()) {
		return nil, fmt.Errorf("usage endpoint violates its allowlist")
	}
	client := &http.Client{Transport: usageConnectTransport{base: http.DefaultTransport, timeout: operation.ConnectTimeout}}
	if !operation.Endpoint.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return &manifestUsageOperation{operation: operation, target: target, client: client}, nil
}

func (operation *manifestUsageOperation) execute(ctx context.Context, token string, values providertransport.TemplateValues) (result any, err error) {
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return nil, err
	}
	started := time.Now()
	diagnostic := providertransport.OperationDiagnostic{ID: operation.operation.ID, Kind: operation.operation.Kind}
	defer func() {
		diagnostic.Duration = time.Since(started)
		diagnostic.Failed = err != nil
		providertransport.RecordOperationDiagnostic(ctx, diagnostic)
	}()
	operation.identityMu.Lock()
	if !operation.identityResolved {
		identity := operation.operation.ClientIdentity
		if identity == nil && operation.operation.Anthropic != nil {
			identity = operation.operation.Anthropic.ClientIdentity
		}
		_, resolved, err := clientidentity.ResolveForContext(ctx, identity)
		if err != nil {
			operation.identityMu.Unlock()
			return nil, err
		}
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			operation.identityMu.Unlock()
			return nil, err
		}
		operation.userAgent = resolved
		operation.identityResolved = true
	}
	userAgent := operation.userAgent
	operation.identityMu.Unlock()
	contextValues := make(map[string]string, len(values.Context)+1)
	for name, value := range values.Context {
		contextValues[name] = value
	}
	if userAgent != "" {
		contextValues["client.user_agent"] = userAgent
	}
	values.Context = contextValues
	credentialValues := make(map[string]string, len(values.Credentials)+1)
	for name, value := range values.Credentials {
		credentialValues[name] = value
	}
	if operation.operation.Endpoint.Credential != "" {
		credentialValues[operation.operation.Endpoint.Credential] = token
	}
	values.Credentials = credentialValues

	document := map[string]any{}
	var body io.Reader
	if operation.operation.RequestTransform != nil {
		if err := providertransport.ApplyJSONPipeline(document, operation.operation.RequestTransform, values); err != nil {
			return nil, fmt.Errorf("request transform: %w", err)
		}
		data, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		if len(data) > maxBody {
			return nil, fmt.Errorf("usage request exceeds %d bytes", maxBody)
		}
		body = bytes.NewReader(data)
	}
	requestCtx := ctx
	if operation.operation.RequestTimeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, operation.operation.RequestTimeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestCtx, operation.operation.Method, operation.target.String(), body)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}
	if body != nil {
		headers["Content-Type"] = "application/json"
	}
	headers, err = operation.operation.ApplyHeadersWithValues(headers, values)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := providertransport.ClientWithContextOwnerValidator(requestCtx, operation.client).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	diagnostic.StatusCode = response.StatusCode
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("usage response exceeds %d bytes", maxBody)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage: %s returned HTTP %d", operation.target, response.StatusCode)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if err := providertransport.ApplyJSONPipeline(result, operation.operation.ResponseTransform, values); err != nil {
		return nil, fmt.Errorf("response transform: %w", err)
	}
	return result, nil
}

func usageStringAt(document any, pointers []string) string {
	for _, pointer := range pointers {
		value, ok := providertransport.JSONPointer(document, pointer).(string)
		if ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func ManifestFetcher(operations map[string]*providertransport.Operation, policy manifest.UsagePolicy) (Fetcher, error) {
	operation, ok := operations[policy.Operation]
	if !ok || operation == nil {
		return nil, fmt.Errorf("usage operation %q is missing", policy.Operation)
	}
	final, err := compileManifestUsageOperation(operation)
	if err != nil {
		return nil, err
	}
	setup := make([]manifestUsageSetup, 0, len(policy.Setup))
	for _, declaration := range policy.Setup {
		operation, ok := operations[declaration.Operation]
		if !ok || operation == nil {
			return nil, fmt.Errorf("usage setup operation %q is missing", declaration.Operation)
		}
		compiled, err := compileManifestUsageOperation(operation)
		if err != nil {
			return nil, err
		}
		setup = append(setup, manifestUsageSetup{declaration: declaration, operation: compiled})
	}
	return func(ctx context.Context, token string) (*Usage, error) {
		values := providertransport.TemplateValues{Context: map[string]string{}}
		result := &Usage{}
		for _, item := range setup {
			document, err := item.operation.execute(ctx, token, values)
			if err != nil {
				return nil, fmt.Errorf("usage setup operation %q: %w", item.operation.operation.ID, err)
			}
			if result.Plan == "" {
				result.Plan = usageStringAt(document, item.declaration.PlanPointers)
			}
			for _, extraction := range item.declaration.Extract {
				value, ok := providertransport.JSONPointer(document, extraction.Pointer).(string)
				if !ok || strings.TrimSpace(value) == "" {
					return nil, fmt.Errorf("usage setup operation %q did not produce context %q", item.operation.operation.ID, extraction.Context)
				}
				values.Context[extraction.Context] = value
			}
		}
		document, err := final.execute(ctx, token, values)
		if err != nil {
			return nil, fmt.Errorf("usage operation %q: %w", final.operation.ID, err)
		}
		if result.Plan == "" {
			result.Plan = usageStringAt(document, policy.PlanPointers)
		}
		for _, window := range policy.Windows {
			used, usedOK := numberAt(document, window.UsedPointer)
			limit, limitOK := numberAt(document, window.LimitPointer)
			remaining, remainingOK := numberAt(document, window.RemainingPointer)
			remainingFraction, remainingFractionOK := numberAt(document, window.RemainingFractionPointer)
			if remainingFractionOK {
				used = (1 - remainingFraction) * 100
				usedOK = true
			} else if !usedOK && remainingOK && limitOK {
				used = limit - remaining
				usedOK = true
			}
			if !usedOK {
				continue
			}
			percent := used
			if !remainingFractionOK && limitOK && limit > 0 {
				percent = used / limit * 100
			}
			item := Window{Name: window.ID, Percent: clampPct(percent)}
			reset := providertransport.JSONPointer(document, window.ResetPointer)
			switch window.ResetFormat {
			case "rfc3339":
				if text, ok := reset.(string); ok {
					item.ResetsAt, _ = time.Parse(time.RFC3339, text)
				}
			case "unix-seconds":
				if value, ok := number(reset); ok {
					item.ResetsAt = time.Unix(int64(value), 0)
				}
			case "unix-milliseconds":
				if value, ok := number(reset); ok {
					item.ResetsAt = time.UnixMilli(int64(value))
				}
			case "duration-seconds":
				if value, ok := number(reset); ok {
					item.ResetsAt = time.Now().Add(time.Duration(value) * time.Second)
				}
			}
			result.Windows = append(result.Windows, item)
		}
		return result, nil
	}, nil
}

func appendUniqueUsage(headers http.Header, name, value string) {
	for _, current := range strings.Split(headers.Get(name), ",") {
		if strings.TrimSpace(current) == value {
			return
		}
	}
	if current := headers.Get(name); current != "" {
		headers.Set(name, current+","+value)
	} else {
		headers.Set(name, value)
	}
}

func numberAt(document any, path string) (float64, bool) {
	return number(providertransport.JSONPointer(document, path))
}

func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case json.Number:
		result, err := typed.Float64()
		return result, err == nil
	}
	return 0, false
}

func containsFoldUsage(values []string, sought string) bool {
	for _, value := range values {
		if strings.EqualFold(value, sought) {
			return true
		}
	}
	return false
}
