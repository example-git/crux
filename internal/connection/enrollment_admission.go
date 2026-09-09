package connection

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	enrollmentMaxConnections           = 8
	enrollmentConnectionBurst          = 16
	enrollmentConnectionsPerSecond     = 4
	enrollmentMaxRequests              = 4
	enrollmentRequestBurst             = 8
	enrollmentRequestsPerSecond        = 2
	enrollmentMaxMalformed             = 5
	enrollmentMaxAuthorizationFailures = 3
)

// Enrollment is an explicitly opened, short-lived listener. These global
// bounds limit admitted work; they do not promise resistance to network floods.
// Rejection by rate or concurrency does not consume a failure-attempt budget.
type enrollmentAdmission struct {
	connections    chan struct{}
	requests       chan struct{}
	connectionRate *rate.Limiter
	requestRate    *rate.Limiter
}

func newEnrollmentAdmission() *enrollmentAdmission {
	return &enrollmentAdmission{
		connections:    make(chan struct{}, enrollmentMaxConnections),
		requests:       make(chan struct{}, enrollmentMaxRequests),
		connectionRate: rate.NewLimiter(enrollmentConnectionsPerSecond, enrollmentConnectionBurst),
		requestRate:    rate.NewLimiter(enrollmentRequestsPerSecond, enrollmentRequestBurst),
	}
}

func admitEnrollmentWork(slots chan struct{}, limiter *rate.Limiter, now time.Time) bool {
	select {
	case slots <- struct{}{}:
		if limiter.AllowN(now, 1) {
			return true
		}
		<-slots
	default:
	}
	return false
}

// The wrapper is outside tls.Listener so rejected sockets never start a TLS
// handshake. The slot remains held through keep-alive and is released once.
type enrollmentAdmissionListener struct {
	net.Listener
	admission *enrollmentAdmission
}

func (l *enrollmentAdmissionListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !admitEnrollmentWork(l.admission.connections, l.admission.connectionRate, time.Now()) {
			_ = conn.Close()
			continue
		}
		return &enrollmentAdmissionConn{Conn: conn, release: func() { <-l.admission.connections }}, nil
	}
}

type enrollmentAdmissionConn struct {
	net.Conn
	once     sync.Once
	release  func()
	closeErr error
}

func (c *enrollmentAdmissionConn) Close() error {
	c.once.Do(func() {
		c.closeErr = c.Conn.Close()
		c.release()
	})
	return c.closeErr
}

func (e *EnrollmentListener) serveEnrollmentHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		e.mu.Lock()
		closed := e.terminal || e.ctx.Err() != nil || !time.Now().Before(e.expiresAt)
		e.mu.Unlock()
		if closed {
			http.Error(response, "enrollment is closed", http.StatusGone)
			return
		}
		if !admitEnrollmentWork(e.admission.requests, e.admission.requestRate, time.Now()) {
			response.Header().Set("Retry-After", "1")
			http.Error(response, "enrollment request capacity exceeded", http.StatusTooManyRequests)
			return
		}
		defer func() { <-e.admission.requests }()
		next.ServeHTTP(response, request)
	})
}

func (e *EnrollmentListener) rejectMalformed(response http.ResponseWriter, request *http.Request, message string) {
	e.mu.Lock()
	if !e.terminal && request.Context().Err() == nil {
		e.malformed++
		if e.malformed >= enrollmentMaxMalformed {
			e.publishLocked(enrollmentOutcome{err: errEnrollmentMalformedLimit})
		}
	}
	exhausted := e.malformed >= enrollmentMaxMalformed
	e.mu.Unlock()
	if exhausted {
		http.Error(response, errEnrollmentMalformedLimit.Error(), http.StatusTooManyRequests)
		return
	}
	http.Error(response, message, http.StatusBadRequest)
}
