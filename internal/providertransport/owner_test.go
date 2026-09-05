package providertransport

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

type ownerRoundTripFunc func(*http.Request) (*http.Response, error)

func (function ownerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestContextOwnerValidatorRoundTrip(t *testing.T) {
	var validations atomic.Int64
	ctx := ContextWithOwnerValidator(t.Context(), func() error {
		validations.Add(1)
		return nil
	})

	require.NotNil(t, OwnerValidatorFromContext(ctx))
	require.NoError(t, ValidateContextOwner(ctx))
	require.EqualValues(t, 1, validations.Load())
}

func TestContextOwnerValidatorPreservesOwnerlessClientsAndOpeners(t *testing.T) {
	client := &http.Client{}
	require.Same(t, client, ClientWithContextOwnerValidator(t.Context(), client))
	require.Same(t, http.DefaultClient, ClientWithContextOwnerValidator(t.Context(), nil))

	var opened atomic.Int64
	require.NoError(t, OpenURLWithContextOwnerValidator(t.Context(), func(string) error {
		opened.Add(1)
		return nil
	}, "https://provider.example.invalid/authorize"))
	require.EqualValues(t, 1, opened.Load())
}

func TestOpenURLWithContextOwnerValidatorRejectsBeforeOpen(t *testing.T) {
	var opened atomic.Int64
	ctx := ContextWithOwnerValidator(t.Context(), func() error {
		return errors.New("owner changed before browser launch")
	})

	err := OpenURLWithContextOwnerValidator(ctx, func(string) error {
		opened.Add(1)
		return nil
	}, "https://provider.example.invalid/authorize")
	require.ErrorContains(t, err, "owner changed before browser launch")
	require.Zero(t, opened.Load())
}

func TestClientWithContextOwnerValidatorRejectsBeforeDispatch(t *testing.T) {
	var dispatched atomic.Int64
	client := ClientWithContextOwnerValidator(ContextWithOwnerValidator(t.Context(), func() error {
		return errors.New("owner changed before request")
	}), &http.Client{Transport: ownerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return nil, errors.New("unexpected dispatch")
	})})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://provider.example.invalid", nil)
	require.NoError(t, err)

	response, err := client.Do(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	require.Nil(t, response)
	require.ErrorContains(t, err, "owner changed before request")
	require.Zero(t, dispatched.Load())
}

func TestOwnerValidatingTransportRejectsBeforeDispatch(t *testing.T) {
	var dispatched atomic.Int64
	transport := OwnerValidatingTransport(ownerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return nil, errors.New("unexpected dispatch")
	}), func() error {
		return errors.New("owner changed")
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://provider.example.invalid", nil)
	require.NoError(t, err)

	response, err := transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	require.Nil(t, response)
	require.ErrorContains(t, err, "owner changed")
	require.Zero(t, dispatched.Load())
}

func TestOwnerValidatingTransportFailsClosedWithoutValidator(t *testing.T) {
	var dispatched atomic.Int64
	transport := OwnerValidatingTransport(ownerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return nil, errors.New("unexpected dispatch")
	}), nil)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://provider.example.invalid", nil)
	require.NoError(t, err)

	response, err := transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	require.Nil(t, response)
	require.ErrorContains(t, err, "validator is unavailable")
	require.Zero(t, dispatched.Load())
}

func TestOwnerValidatingTransportRevalidatesRetry(t *testing.T) {
	var validations atomic.Int64
	var dispatched atomic.Int64
	transport := OwnerValidatingTransport(ownerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
			Request:    request,
		}, nil
	}), func() error {
		if validations.Add(1) == 2 {
			return errors.New("owner changed before retry")
		}
		return nil
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://provider.example.invalid", nil)
	require.NoError(t, err)

	response, err := RoundTripWithRetry(request, transport, manifest.RetryPolicy{MaxAttempts: 4, Statuses: []int{http.StatusServiceUnavailable}, TransportErrors: true})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	require.Nil(t, response)
	require.ErrorContains(t, err, "owner changed before retry")
	require.EqualValues(t, 2, validations.Load())
	require.EqualValues(t, 1, dispatched.Load())
}

func TestClientWithOwnerValidatorRevalidatesRedirect(t *testing.T) {
	var validations atomic.Int64
	var dispatched atomic.Int64
	client := ClientWithOwnerValidator(&http.Client{Transport: ownerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://redirected.example.invalid/final"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    request,
		}, nil
	})}, func() error {
		if validations.Add(1) == 2 {
			return errors.New("owner changed before redirect")
		}
		return nil
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://provider.example.invalid/start", nil)
	require.NoError(t, err)

	response, err := client.Do(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	require.Nil(t, response)
	require.ErrorContains(t, err, "owner changed before redirect")
	require.EqualValues(t, 2, validations.Load())
	require.EqualValues(t, 1, dispatched.Load())
}
