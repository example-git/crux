package connection

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrollmentPairsClientForPinnedMutualTLS(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enrollment.Close() })

	saved, err := Pair(t.Context(), "workstation", enrollment.SetupCode())
	require.NoError(t, err)
	result, err := enrollment.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "workstation", result.Name)
	require.NotEmpty(t, result.Fingerprint)

	reloaded, exists, err := Get(t.Context(), "workstation")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, saved, reloaded)

	serverTLS, err := ServerTLSConfig(t.Context())
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.NotEmpty(t, request.TLS.PeerCertificates)
		response.WriteHeader(http.StatusOK)
	}))
	server.TLS = serverTLS
	server.StartTLS()
	t.Cleanup(server.Close)
	clientTLS, err := ClientTLSConfig(saved)
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: clientTLS}
	t.Cleanup(transport.CloseIdleConnections)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/health", nil)
	require.NoError(t, err)
	response, err := (&http.Client{Transport: transport}).Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestEnrollmentListenerExpiresWithoutClient(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", 50*time.Millisecond)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enrollment.Close() })

	waitCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = enrollment.Wait(waitCtx)
	require.ErrorContains(t, err, "enrollment code expired")
	require.ErrorContains(t, err, "crux server setup")

	address := strings.TrimPrefix(enrollment.Address(), "tcp://")
	require.Eventually(t, func() bool {
		dialer := net.Dialer{Timeout: 20 * time.Millisecond}
		connection, dialErr := dialer.DialContext(t.Context(), "tcp", address)
		if dialErr != nil {
			return true
		}
		_ = connection.Close()
		return false
	}, time.Second, 10*time.Millisecond)
}

func TestEnrollmentTokenAuthorizesExactlyOneConcurrentClient(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enrollment.Close() })
	setup, err := DecodeEnrollmentSetup(enrollment.SetupCode())
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	var authorizationCalls atomic.Int32
	enrollment.authorizeClient = func(ctx context.Context, name, certificate string) error {
		if authorizationCalls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return AuthorizeClient(ctx, name, certificate)
	}

	const clients = 12
	bodies := make([][]byte, clients)
	for index := range clients {
		name := fmt.Sprintf("concurrent-%d", index)
		identity, identityErr := NewClientIdentity(name)
		require.NoError(t, identityErr)
		bodies[index], err = json.Marshal(enrollmentRequest{Name: name, Certificate: identity.Certificate})
		require.NoError(t, err)
	}

	statuses := make(chan int, clients)
	var requests sync.WaitGroup
	for _, body := range bodies {
		requests.Add(1)
		go func() {
			defer requests.Done()
			status, _ := enrollmentRequestStatus(t.Context(), setup, body)
			statuses <- status
		}()
	}
	<-entered
	time.Sleep(25 * time.Millisecond)
	close(release)
	requests.Wait()
	close(statuses)

	created := 0
	for status := range statuses {
		if status == http.StatusCreated {
			created++
		}
	}
	require.Equal(t, 1, created)
	require.EqualValues(t, 1, authorizationCalls.Load())
	authorized, err := ListAuthorizedClients(t.Context())
	require.NoError(t, err)
	require.Len(t, authorized, 1)
}

func TestEnrollmentAuthorizationFailureReleasesSoleReservation(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	duplicate, err := NewClientIdentity("duplicate")
	require.NoError(t, err)
	require.NoError(t, AuthorizeClient(t.Context(), "duplicate", duplicate.Certificate))
	enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enrollment.Close() })
	setup, err := DecodeEnrollmentSetup(enrollment.SetupCode())
	require.NoError(t, err)

	duplicateBody, err := json.Marshal(enrollmentRequest{Name: "duplicate", Certificate: duplicate.Certificate})
	require.NoError(t, err)
	response := sendEnrollmentRequest(t, setup, duplicateBody)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.NoError(t, response.Body.Close())

	valid, err := NewClientIdentity("valid-after-failure")
	require.NoError(t, err)
	validBody, err := json.Marshal(enrollmentRequest{Name: "valid-after-failure", Certificate: valid.Certificate})
	require.NoError(t, err)
	response = sendEnrollmentRequest(t, setup, validBody)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestEnrollmentRejectsWrongFingerprintTokenExpiryAndReplay(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)

	t.Run("fingerprint", func(t *testing.T) {
		enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
		require.NoError(t, err)
		t.Cleanup(func() { _ = enrollment.Close() })
		setup, err := DecodeEnrollmentSetup(enrollment.SetupCode())
		require.NoError(t, err)
		setup.Fingerprint = strings.Repeat("0", 64)
		_, err = Pair(t.Context(), "wrong-fingerprint", encodeEnrollmentSetup(t, setup))
		require.ErrorContains(t, err, "fingerprint does not match")
	})

	t.Run("token", func(t *testing.T) {
		enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
		require.NoError(t, err)
		t.Cleanup(func() { _ = enrollment.Close() })
		setup, err := DecodeEnrollmentSetup(enrollment.SetupCode())
		require.NoError(t, err)
		setup.Token = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, enrollmentTokenBytes))
		_, err = Pair(t.Context(), "wrong-token", encodeEnrollmentSetup(t, setup))
		require.ErrorContains(t, err, "invalid enrollment token")
	})

	t.Run("expiry", func(t *testing.T) {
		enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
		require.NoError(t, err)
		t.Cleanup(func() { _ = enrollment.Close() })
		setup, err := DecodeEnrollmentSetup(enrollment.SetupCode())
		require.NoError(t, err)
		setup.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		_, err = Pair(t.Context(), "expired", encodeEnrollmentSetup(t, setup))
		require.ErrorContains(t, err, "expired")
	})

	t.Run("replay", func(t *testing.T) {
		enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
		require.NoError(t, err)
		_, err = Pair(t.Context(), "first", enrollment.SetupCode())
		require.NoError(t, err)
		_, err = enrollment.Wait(t.Context())
		require.NoError(t, err)
		_, err = Pair(t.Context(), "replay", enrollment.SetupCode())
		require.Error(t, err)
	})
}

func TestEnrollmentRejectsMalformedOversizedAndDuplicateRequests(t *testing.T) {
	for _, test := range []struct {
		name       string
		prepare    func(*testing.T)
		body       func(*testing.T) []byte
		statusCode int
	}{
		{
			name: "malformed certificate",
			body: func(*testing.T) []byte {
				return []byte(`{"name":"client","certificate":"invalid"}`)
			},
			statusCode: http.StatusBadRequest,
		},
		{
			name: "oversized request",
			body: func(*testing.T) []byte {
				return []byte(`{"name":"client","certificate":"` + strings.Repeat("x", enrollmentMaxRequestBytes) + `"}`)
			},
			statusCode: http.StatusBadRequest,
		},
		{
			name: "duplicate name",
			prepare: func(t *testing.T) {
				identity, err := NewClientIdentity("duplicate")
				require.NoError(t, err)
				require.NoError(t, AuthorizeClient(t.Context(), "duplicate", identity.Certificate))
			},
			body: func(t *testing.T) []byte {
				identity, err := NewClientIdentity("duplicate")
				require.NoError(t, err)
				content, err := json.Marshal(enrollmentRequest{Name: "duplicate", Certificate: identity.Certificate})
				require.NoError(t, err)
				return content
			},
			statusCode: http.StatusConflict,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			setConnectionRoot(t, t.TempDir())
			_, err := EnsureServerIdentity(t.Context())
			require.NoError(t, err)
			if test.prepare != nil {
				test.prepare(t)
			}
			enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
			require.NoError(t, err)
			t.Cleanup(func() { _ = enrollment.Close() })
			setup, err := DecodeEnrollmentSetup(enrollment.SetupCode())
			require.NoError(t, err)
			response := sendEnrollmentRequest(t, setup, test.body(t))
			require.Equal(t, test.statusCode, response.StatusCode)
			require.NoError(t, response.Body.Close())
		})
	}
}

func TestEnrollmentStopsAfterFailedAttemptLimit(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enrollment.Close() })
	setup, err := DecodeEnrollmentSetup(enrollment.SetupCode())
	require.NoError(t, err)
	identity, err := NewClientIdentity("client")
	require.NoError(t, err)
	body, err := json.Marshal(enrollmentRequest{Name: "client", Certificate: identity.Certificate})
	require.NoError(t, err)
	setup.Token = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, enrollmentTokenBytes))
	for attempt := 0; attempt < enrollmentMaxAttempts; attempt++ {
		response := sendEnrollmentRequest(t, setup, body)
		if attempt == enrollmentMaxAttempts-1 {
			require.Equal(t, http.StatusTooManyRequests, response.StatusCode)
		} else {
			require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		}
		require.NoError(t, response.Body.Close())
	}
	_, err = enrollment.Wait(t.Context())
	require.ErrorContains(t, err, "attempt limit")
}

func enrollmentRequestStatus(ctx context.Context, setup EnrollmentSetup, body []byte) (int, error) {
	address := strings.TrimPrefix(setup.Address, "tcp://")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+address+enrollmentPath, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Crux-Enrollment "+setup.Token)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport, Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func sendEnrollmentRequest(t *testing.T, setup EnrollmentSetup, body []byte) *http.Response {
	t.Helper()
	address := strings.TrimPrefix(setup.Address, "tcp://")
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+address+enrollmentPath, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Crux-Enrollment "+setup.Token)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}
	t.Cleanup(transport.CloseIdleConnections)
	response, err := (&http.Client{Transport: transport}).Do(request)
	require.NoError(t, err)
	return response
}

func encodeEnrollmentSetup(t *testing.T, setup EnrollmentSetup) string {
	t.Helper()
	content, err := json.Marshal(setup)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(content)
}
