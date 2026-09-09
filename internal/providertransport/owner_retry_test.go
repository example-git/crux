package providertransport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/stretchr/testify/require"
)

func TestOwnerValidationRefusalSkipsNetworkRetryAndAuthRefresh(t *testing.T) {
	for _, shape := range []string{"http-client", "authentication", "transient", "indefinite"} {
		t.Run(shape, func(t *testing.T) {
			refusal := errors.New("local owner no longer admits this request")
			validations, dispatched, attempts, retries, refreshes := 0, 0, 0, 0, 0
			client := ClientWithOwnerValidator(&http.Client{Transport: ownerRoundTripFunc(func(*http.Request) (*http.Response, error) {
				dispatched++
				return nil, errors.New("unexpected network dispatch")
			})}, func() error {
				validations++
				return refusal
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			retry := fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[int](fantasy.RetryOptions{
				MaxRetries: 3, InitialDelayIn: time.Minute, BackoffFactor: 2,
				OnRetry: func(*fantasy.ProviderError, time.Duration) { retries++ },
				OnAuthRefresh: func(context.Context, *fantasy.ProviderError) error {
					refreshes++
					return nil
				},
			})
			_, err := retry(ctx, func() (int, error) {
				attempts++
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.invalid", nil)
				require.NoError(t, err)
				_, err = client.Do(request)
				require.ErrorIs(t, err, refusal)
				var networkError net.Error
				require.ErrorAs(t, err, &networkError, "net/http wrapping must reproduce the original retry misclassification")
				if shape != "http-client" {
					wrapped := &fantasy.ProviderError{Cause: err, Message: "wrapped admission refusal"}
					switch shape {
					case "authentication":
						wrapped.StatusCode = http.StatusUnauthorized
					case "transient":
						wrapped.TransientError = true
					case "indefinite":
						wrapped.UnlimitedRetry = fantasy.UnlimitedRetryServerOverload
					}
					require.False(t, wrapped.IsRetryable())
					require.False(t, fantasy.IsIndefinitelyRetryable(wrapped))
					err = wrapped
				}
				return 0, err
			})
			require.ErrorIs(t, err, refusal)
			require.NoError(t, ctx.Err(), "local denial must return before any retry delay")
			require.Equal(t, 1, attempts)
			require.Equal(t, 1, validations)
			require.Zero(t, dispatched)
			require.Zero(t, retries)
			require.Zero(t, refreshes)
		})
	}
}

func TestOwnerValidationPreservesAdmittedNetworkRetry(t *testing.T) {
	validations, dispatched, retries := 0, 0, 0
	client := ClientWithOwnerValidator(&http.Client{Transport: ownerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched++
		if dispatched == 1 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("accepted"))}, nil
	})}, func() error { validations++; return nil })
	retry := fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[string](fantasy.RetryOptions{
		MaxRetries: 1, InitialDelayIn: time.Millisecond, BackoffFactor: 2,
		OnRetry: func(*fantasy.ProviderError, time.Duration) { retries++ },
	})
	result, err := retry(t.Context(), func() (string, error) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		return string(data), err
	})
	require.NoError(t, err)
	require.Equal(t, "accepted", result)
	require.Equal(t, 2, validations)
	require.Equal(t, 2, dispatched)
	require.Equal(t, 1, retries)
}
