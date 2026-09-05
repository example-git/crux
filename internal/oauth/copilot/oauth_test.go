package copilot

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providertransport"
)

type copilotRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip copilotRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestOAuthRequestsRejectOwnerReplacementBeforeDispatch(t *testing.T) {
	original := http.DefaultClient.Transport
	var dispatched atomic.Int64
	http.DefaultClient.Transport = copilotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return nil, errors.New("unexpected dispatch")
	})
	t.Cleanup(func() { http.DefaultClient.Transport = original })
	ctx := providertransport.ContextWithOwnerValidator(t.Context(), func() error {
		return errors.New("owner changed")
	})

	if _, err := RequestDeviceCode(ctx); err == nil || !strings.Contains(err.Error(), "owner changed") {
		t.Fatalf("RequestDeviceCode() error = %v", err)
	}
	if _, err := tryGetToken(ctx, "device-code"); err == nil || !strings.Contains(err.Error(), "owner changed") {
		t.Fatalf("tryGetToken() error = %v", err)
	}
	if _, err := getCopilotToken(ctx, "github-token"); err == nil || !strings.Contains(err.Error(), "owner changed") {
		t.Fatalf("getCopilotToken() error = %v", err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatched = %d", dispatched.Load())
	}
}
