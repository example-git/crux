package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

// ManifestFetcher builds a host-owned usage executor from a compiled operation.
func ManifestFetcher(operation *providertransport.Operation, policy manifest.UsagePolicy) (Fetcher, error) {
	if operation == nil {
		return nil, fmt.Errorf("usage operation is missing")
	}
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
	userAgent := "Crux"
	return func(ctx context.Context, token string) (*Usage, error) {
		requestCtx := ctx
		if operation.RequestTimeout > 0 {
			var cancel context.CancelFunc
			requestCtx, cancel = context.WithTimeout(ctx, operation.RequestTimeout)
			defer cancel()
		}
		request, err := http.NewRequestWithContext(requestCtx, operation.Method, target.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", "application/json")
		for _, rule := range operation.Headers {
			if rule.Value == nil || rule.Operation == "delete" {
				if rule.Operation == "delete" {
					request.Header.Del(rule.Name)
				}
				continue
			}
			value, ok := rule.Value.Value.(string)
			if rule.Value.Kind == "context" && rule.Value.Ref == "client.user_agent" {
				value = userAgent
				ok = true
			}
			if !ok {
				continue
			}
			switch rule.Operation {
			case "set":
				request.Header.Set(rule.Name, value)
			case "set-if-absent":
				if request.Header.Get(rule.Name) == "" {
					request.Header.Set(rule.Name, value)
				}
			case "append":
				request.Header.Add(rule.Name, value)
			case "append-unique":
				appendUniqueUsage(request.Header, rule.Name, value)
			}
		}
		client := &http.Client{}
		if !operation.Endpoint.FollowRedirects {
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxBody {
			return nil, fmt.Errorf("usage response exceeds %d bytes", maxBody)
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("usage: %s returned HTTP %d", target, response.StatusCode)
		}
		var document any
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, err
		}
		result := &Usage{}
		for _, window := range policy.Windows {
			used, usedOK := numberAt(document, window.UsedPointer)
			limit, limitOK := numberAt(document, window.LimitPointer)
			remaining, remainingOK := numberAt(document, window.RemainingPointer)
			if !usedOK && remainingOK && limitOK {
				used = limit - remaining
				usedOK = true
			}
			if !usedOK {
				continue
			}
			percent := used
			if limitOK && limit > 0 {
				percent = used / limit * 100
			}
			item := Window{Name: window.ID, Percent: clampPct(percent)}
			reset := valueAt(document, window.ResetPointer)
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

func valueAt(document any, path string) any {
	if path == "" {
		return nil
	}
	current := document
	for _, raw := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		current = object[key]
	}
	return current
}
func numberAt(document any, path string) (float64, bool) { return number(valueAt(document, path)) }
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
