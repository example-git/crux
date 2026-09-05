package providertransport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

const maxRetryErrorBodyBytes = int64(1 << 20)

func RoundTripWithRetry(request *http.Request, transport http.RoundTripper, policy manifest.RetryPolicy, errorMappings ...[]manifest.ErrorMapping) (*http.Response, error) {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return executeWithRetry(request, policy, firstErrorMappings(errorMappings), transport.RoundTrip)
}

func DoWithRetry(request *http.Request, client *http.Client, policy manifest.RetryPolicy, errorMappings ...[]manifest.ErrorMapping) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	return executeWithRetry(request, policy, firstErrorMappings(errorMappings), client.Do)
}

func executeWithRetry(request *http.Request, policy manifest.RetryPolicy, errorMappings []manifest.ErrorMapping, execute func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	attempts := policy.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var body []byte
	if request.Body != nil && request.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(request.Body)
		_ = request.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		current := request.Clone(request.Context())
		if body != nil {
			current.Body = io.NopCloser(bytes.NewReader(body))
			current.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
			current.ContentLength = int64(len(body))
		}
		response, err := execute(current)
		retry, retryAfter := retryDecision(response, err, policy, errorMappings)
		if !retry || attempt == attempts {
			return response, err
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err := waitForRetry(request.Context(), retryDelay(policy, attempt, retryAfter)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func retryDecision(response *http.Response, err error, policy manifest.RetryPolicy, errorMappings []manifest.ErrorMapping) (bool, time.Duration) {
	if IsOwnerValidationError(err) {
		return false, 0
	}
	if err != nil {
		return policy.TransportErrors, 0
	}
	if response == nil {
		return policy.TransportErrors, 0
	}
	retry := containsStatus(policy.Statuses, response.StatusCode)
	failedResponse := response.StatusCode < 200 || response.StatusCode >= 300
	needsBody := len(policy.Codes) > 0 || failedResponse && errorMappingsNeedCodeBody(errorMappings)
	var body []byte
	bodyReadable := true
	if needsBody && response.Body != nil {
		var readErr error
		body, readErr = io.ReadAll(io.LimitReader(response.Body, maxRetryErrorBodyBytes+1))
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		if response.Header == nil {
			response.Header = make(http.Header)
		}
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		bodyReadable = readErr == nil && int64(len(body)) <= maxRetryErrorBodyBytes
	}
	if !retry && len(policy.Codes) > 0 && bodyReadable {
		retry = containsCode(policy.Codes, body)
	}
	if failedResponse && len(errorMappings) > 0 {
		providerErr := &fantasy.ProviderError{StatusCode: response.StatusCode}
		if bodyReadable {
			providerErr.ResponseBody = body
		}
		retry = retry || ErrorMappingRetryable(errorMappings, providerErr)
	}
	if !retry || !policy.RetryAfter {
		return retry, 0
	}
	return true, parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
}

func firstErrorMappings(values [][]manifest.ErrorMapping) []manifest.ErrorMapping {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func errorMappingsNeedCodeBody(mappings []manifest.ErrorMapping) bool {
	for _, mapping := range mappings {
		if len(mapping.Codes) > 0 {
			return true
		}
	}
	return false
}

func containsStatus(statuses []int, status int) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func containsCode(codes []string, body []byte) bool {
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&document) != nil {
		return false
	}
	values := []any{document["code"]}
	if nested, ok := document["error"].(map[string]any); ok {
		values = append(values, nested["code"])
	}
	for _, value := range values {
		var code string
		switch typed := value.(type) {
		case string:
			code = typed
		case json.Number:
			code = typed.String()
		}
		for _, candidate := range codes {
			if code == candidate {
				return true
			}
		}
	}
	return false
}

func RetryDelay(policy manifest.RetryPolicy, attempt int, retryAfter string) time.Duration {
	return retryDelay(policy, attempt, parseRetryAfter(retryAfter, time.Now()))
}

func retryDelay(policy manifest.RetryPolicy, attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	delay := time.Duration(policy.InitialDelayMS) * time.Millisecond
	factor := policy.Factor
	if factor < 1 {
		factor = 1
	}
	for index := 1; index < attempt; index++ {
		delay = time.Duration(float64(delay) * factor)
	}
	maximum := time.Duration(policy.MaxDelayMS) * time.Millisecond
	if maximum > 0 && delay > maximum {
		delay = maximum
	}
	if policy.Jitter && delay > 0 {
		delay = time.Duration(rand.Int64N(int64(delay) + 1))
	}
	return delay
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func WaitForRetry(ctx context.Context, delay time.Duration) error {
	return waitForRetry(ctx, delay)
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return context.Cause(ctx)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
