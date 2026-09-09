package connection

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func budgetEnrollment(t *testing.T) *EnrollmentListener {
	t.Helper()
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	e, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func requireNoEnrollmentFailures(t *testing.T, e *EnrollmentListener) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Zero(t, e.attempts)
	require.Zero(t, e.malformed)
	require.Zero(t, e.authorizationFailures)
	require.False(t, e.terminal)
}

func TestEnrollmentAdmissionRateRefillAndConcurrencyAreIndependent(t *testing.T) {
	for _, test := range []struct {
		name         string
		slots, burst int
		perSecond    rate.Limit
	}{
		{"connections", enrollmentMaxConnections, enrollmentConnectionBurst, enrollmentConnectionsPerSecond},
		{"requests", enrollmentMaxRequests, enrollmentRequestBurst, enrollmentRequestsPerSecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			slots := make(chan struct{}, test.slots)
			limiter := rate.NewLimiter(test.perSecond, test.burst)
			now := time.Now()
			for range test.slots {
				require.True(t, admitEnrollmentWork(slots, limiter, now))
			}
			require.False(t, admitEnrollmentWork(slots, limiter, now))
			for range test.slots {
				<-slots
			}
			for range test.burst - test.slots {
				require.True(t, admitEnrollmentWork(slots, limiter, now))
				<-slots
			}
			require.False(t, admitEnrollmentWork(slots, limiter, now))
			require.Empty(t, slots)
			require.True(t, admitEnrollmentWork(slots, limiter, now.Add(time.Second/time.Duration(test.perSecond))))
			<-slots
			require.False(t, admitEnrollmentWork(slots, limiter, now.Add(time.Second/time.Duration(test.perSecond))))
		})
	}
}

func TestEnrollmentAdmissionRejectsSocketsBeforeTLSAndRecovers(t *testing.T) {
	for _, refusal := range []string{"concurrency", "rate"} {
		t.Run(refusal, func(t *testing.T) {
			e := budgetEnrollment(t)
			address := strings.TrimPrefix(e.Address(), "tcp://")
			var stalled []net.Conn
			t.Cleanup(func() {
				for _, conn := range stalled {
					_ = conn.Close()
				}
			})
			if refusal == "concurrency" {
				for range enrollmentMaxConnections {
					conn, err := net.DialTimeout("tcp", address, time.Second)
					require.NoError(t, err)
					stalled = append(stalled, conn)
				}
				require.Eventually(t, func() bool { return len(e.admission.connections) == enrollmentMaxConnections }, time.Second, time.Millisecond)
			} else {
				e.admission.connectionRate.SetLimit(0)
				require.True(t, e.admission.connectionRate.AllowN(time.Now(), enrollmentConnectionBurst))
			}
			rejected, err := net.DialTimeout("tcp", address, time.Second)
			require.NoError(t, err)
			defer rejected.Close()
			require.NoError(t, rejected.SetReadDeadline(time.Now().Add(time.Second)))
			_, err = rejected.Read(make([]byte, 1))
			require.Error(t, err)
			require.NotErrorIs(t, err, os.ErrDeadlineExceeded, "socket must be rejected without waiting for a TLS handshake timeout")
			requireNoEnrollmentFailures(t, e)
			for _, conn := range stalled {
				require.NoError(t, conn.Close())
			}
			require.Eventually(t, func() bool { return len(e.admission.connections) == 0 }, time.Second, time.Millisecond)
			e.admission.connectionRate.SetLimit(enrollmentConnectionsPerSecond)
			require.Eventually(t, func() bool { return e.admission.connectionRate.Tokens() >= 1 }, 2*time.Second, 10*time.Millisecond)
			_, err = Pair(t.Context(), "after-socket-pressure", e.SetupCode())
			require.NoError(t, err)
			result, err := e.Wait(t.Context())
			require.NoError(t, err)
			require.Equal(t, "after-socket-pressure", result.Name)
		})
	}
}

func TestEnrollmentAdmissionBoundsSlowHTTPSRequestsBeforeDecode(t *testing.T) {
	e := budgetEnrollment(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	var writers []*io.PipeWriter
	t.Cleanup(func() {
		for _, writer := range writers {
			_ = writer.Close()
		}
	})
	statuses := make(chan int, enrollmentMaxRequests)
	for range enrollmentMaxRequests {
		reader, writer := io.Pipe()
		writers = append(writers, writer)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+strings.TrimPrefix(e.Address(), "tcp://")+enrollmentPath, reader)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Crux-Enrollment "+e.setup.Token)
		go func() {
			response, err := client.Do(request)
			if err != nil {
				statuses <- 0
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			statuses <- response.StatusCode
		}()
		_, err = writer.Write([]byte("{"))
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return len(e.admission.requests) == enrollmentMaxRequests }, time.Second, time.Millisecond)
	status, err := enrollmentRequestStatus(t.Context(), e.setup, []byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusTooManyRequests, status)
	requireNoEnrollmentFailures(t, e)
	for _, writer := range writers {
		_, err := writer.Write([]byte("}"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
	}
	for range enrollmentMaxRequests {
		select {
		case status := <-statuses:
			require.Equal(t, http.StatusBadRequest, status)
		case <-time.After(5 * time.Second):
			t.Fatal("stalled request did not finish")
		}
	}
	require.Eventually(t, func() bool { return len(e.admission.requests) == 0 }, time.Second, time.Millisecond)
	_, err = Pair(t.Context(), "after-request-pressure", e.SetupCode())
	require.NoError(t, err)
}

func TestEnrollmentAdmissionRequestBurstKeepsWindowOpenAndRefills(t *testing.T) {
	e := budgetEnrollment(t)
	e.admission.requestRate.SetLimit(0)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}, Timeout: 2 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	for i := range enrollmentRequestBurst + 1 {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+strings.TrimPrefix(e.Address(), "tcp://")+"/not-enrollment", nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, response.Body)
		require.NoError(t, response.Body.Close())
		if i == enrollmentRequestBurst {
			require.Equal(t, http.StatusTooManyRequests, response.StatusCode)
			require.Equal(t, "1", response.Header.Get("Retry-After"))
		} else {
			require.Equal(t, http.StatusNotFound, response.StatusCode)
		}
		require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	}
	requireNoEnrollmentFailures(t, e)
	e.admission.requestRate.SetLimit(enrollmentRequestsPerSecond)
	require.Eventually(t, func() bool { return e.admission.requestRate.Tokens() >= 1 }, 2*time.Second, 10*time.Millisecond)
	_, err := Pair(t.Context(), "after-request-rate", e.SetupCode())
	require.NoError(t, err)
}

func TestEnrollmentFailureBudgetsAreSeparateAndPreserveAuthorizationStore(t *testing.T) {
	for _, category := range []string{"malformed", "authorization"} {
		t.Run(category, func(t *testing.T) {
			e := budgetEnrollment(t)
			before, err := os.ReadFile(storePath())
			require.NoError(t, err)
			body := []byte(`{}`)
			limit, ordinary, terminal := enrollmentMaxMalformed, http.StatusBadRequest, errEnrollmentMalformedLimit
			if category == "authorization" {
				identity, err := NewClientIdentity("denied")
				require.NoError(t, err)
				body = []byte(`{"name":"denied","certificate":"` + identity.Certificate + `"}`)
				e.authorizeClient = func(context.Context, string, string, authorizationCommit) error {
					return errors.New("fixture persistence denied")
				}
				limit, ordinary, terminal = enrollmentMaxAuthorizationFailures, http.StatusConflict, errEnrollmentAuthorizationLimit
			}
			for i := range limit {
				status, err := enrollmentRequestStatus(t.Context(), e.setup, body)
				require.NoError(t, err)
				if i == limit-1 {
					require.Equal(t, http.StatusTooManyRequests, status)
				} else {
					require.Equal(t, ordinary, status)
				}
			}
			_, err = e.Wait(t.Context())
			require.ErrorIs(t, err, terminal)
			after, err := os.ReadFile(storePath())
			require.NoError(t, err)
			require.Equal(t, before, after)
			e.mu.Lock()
			require.Zero(t, e.attempts)
			if category == "malformed" {
				require.Equal(t, limit, e.malformed)
				require.Zero(t, e.authorizationFailures)
			} else {
				require.Zero(t, e.malformed)
				require.Equal(t, limit, e.authorizationFailures)
			}
			e.mu.Unlock()
		})
	}
}
