package connection

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	enrollmentVersion         = 2
	enrollmentPath            = "/v1/enroll"
	enrollmentTokenBytes      = 32
	enrollmentMaxAttempts     = 5
	enrollmentMaxRequestBytes = 32 << 10
)

type EnrollmentSetup struct {
	Version     int    `json:"v"`
	Address     string `json:"a"`
	Fingerprint string `json:"f"`
	Token       string `json:"t"`
	ExpiresAt   int64  `json:"e"`
}

type EnrollmentResult struct {
	Name        string
	Fingerprint string
}

type EnrollmentListener struct {
	setup                 EnrollmentSetup
	code                  string
	listener              net.Listener
	server                *http.Server
	ctx                   context.Context
	outcome               enrollmentOutcome
	terminal              bool
	done                  chan struct{}
	closed                chan struct{}
	shutdownErr           error
	mu                    sync.Mutex
	attempts              int
	malformed             int
	authorizationFailures int
	admission             *enrollmentAdmission
	tokenState            enrollmentTokenState
	expired               bool
	expiresAt             time.Time
	authorizeClient       func(context.Context, string, string, authorizationCommit) error
	approve               EnrollmentApprover
	cancel                context.CancelFunc
}

type enrollmentTokenState uint8

const (
	enrollmentTokenAvailable enrollmentTokenState = iota
	enrollmentTokenReserved
	enrollmentTokenUsed
)

type enrollmentOutcome struct {
	result EnrollmentResult
	err    error
}

type enrollmentRequest struct {
	Name        string `json:"name"`
	Certificate string `json:"certificate"`
}

func StartEnrollment(ctx context.Context, listenAddress, advertisedAddress string, ttl time.Duration, approve EnrollmentApprover) (*EnrollmentListener, error) {
	if approve == nil {
		return nil, errors.New("enrollment requires an explicit approver")
	}
	if ttl <= 0 {
		return nil, errors.New("enrollment expiry must be positive")
	}
	listenHost, listenPort, err := parseEnrollmentAddress(listenAddress, true)
	if err != nil {
		return nil, fmt.Errorf("invalid enrollment listen address: %w", err)
	}
	storagePath, err := filepath.Abs(storePath())
	if err != nil {
		return nil, err
	}
	stored, err := loadStoreAt(ctx, storagePath)
	if err != nil {
		return nil, err
	}
	if stored.Server == nil {
		return nil, errors.New("server identity is not initialized")
	}
	serverIdentity := *stored.Server
	serverCertificate, serverPrivateKey, err := parseIdentity(serverIdentity, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, fmt.Errorf("load server identity: %w", err)
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", net.JoinHostPort(listenHost, listenPort))
	if err != nil {
		return nil, fmt.Errorf("start enrollment listener: %w", err)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	if advertisedAddress == "" {
		ip := net.ParseIP(listenHost)
		if listenHost == "" || (ip != nil && ip.IsUnspecified()) {
			listener.Close()
			return nil, errors.New("--advertise is required when the enrollment listener uses a wildcard host")
		}
		advertisedAddress = "tcp://" + net.JoinHostPort(listenHost, strconv.Itoa(actualPort))
	} else {
		normalized, normalizeErr := NormalizeConnectionAddress(advertisedAddress)
		if normalizeErr != nil {
			listener.Close()
			return nil, fmt.Errorf("invalid advertised address: %w", normalizeErr)
		}
		advertisedAddress = normalized
	}
	tokenBytes := make([]byte, enrollmentTokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		listener.Close()
		return nil, fmt.Errorf("generate enrollment token: %w", err)
	}
	expiresAt := time.Now().Add(ttl)
	setup := EnrollmentSetup{
		Version:     enrollmentVersion,
		Address:     advertisedAddress,
		Fingerprint: certificateFingerprint(serverCertificate),
		Token:       base64.RawURLEncoding.EncodeToString(tokenBytes),
		ExpiresAt:   expiresAt.Unix(),
	}
	codeBytes, err := json.Marshal(setup)
	if err != nil {
		listener.Close()
		return nil, err
	}
	enrollmentContext, cancelEnrollment := context.WithCancel(ctx)
	enrollment := &EnrollmentListener{
		setup:     setup,
		code:      base64.RawURLEncoding.EncodeToString(codeBytes),
		listener:  listener,
		ctx:       enrollmentContext,
		approve:   approve,
		cancel:    cancelEnrollment,
		done:      make(chan struct{}),
		closed:    make(chan struct{}),
		expiresAt: expiresAt,
		admission: newEnrollmentAdmission(),
		authorizeClient: func(ctx context.Context, name, certificate string, commit authorizationCommit) error {
			return authorizeClientAt(ctx, storagePath, &serverIdentity, name, certificate, commit)
		},
	}
	enrollment.listener = &enrollmentAdmissionListener{Listener: listener, admission: enrollment.admission}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+enrollmentPath, enrollment.handleEnrollment)
	enrollment.server = &http.Server{
		Handler:           enrollment.serveEnrollmentHTTP(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{serverCertificate.Raw},
			PrivateKey:  serverPrivateKey,
		}},
	}
	go func() {
		err := enrollment.server.Serve(tls.NewListener(enrollment.listener, tlsConfig))
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			enrollment.finish(enrollmentOutcome{err: fmt.Errorf("enrollment listener failed: %w", err)})
		}
	}()
	go func() {
		timer := time.NewTimer(time.Until(expiresAt))
		defer timer.Stop()
		select {
		case <-timer.C:
			enrollment.expire()
		case <-ctx.Done():
			enrollment.finish(enrollmentOutcome{err: ctx.Err()})
		case <-enrollment.done:
		}
	}()
	return enrollment, nil
}

func (e *EnrollmentListener) SetupCode() string {
	return e.code
}

func (e *EnrollmentListener) Address() string {
	return e.setup.Address
}

func (e *EnrollmentListener) Wait(ctx context.Context) (EnrollmentResult, error) {
	select {
	case <-e.done:
	case <-ctx.Done():
		// Cancellation ends enrollment, not just this waiter. If persistence
		// already won the commit boundary, report that success to every waiter.
		e.finish(enrollmentOutcome{err: ctx.Err()})
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.outcome.result, e.outcome.err
}

func (e *EnrollmentListener) Close() error {
	e.finish(enrollmentOutcome{err: errEnrollmentClosed})
	<-e.closed
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownErr
}

func (e *EnrollmentListener) handleEnrollment(response http.ResponseWriter, request *http.Request) {
	token := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Crux-Enrollment "))
	if subtle.ConstantTimeCompare([]byte(token), []byte(e.setup.Token)) != 1 {
		if e.failedAttempt() {
			http.Error(response, "enrollment attempt limit exceeded", http.StatusTooManyRequests)
			e.finish(enrollmentOutcome{err: errors.New("enrollment attempt limit exceeded")})
			return
		}
		http.Error(response, "invalid enrollment token", http.StatusUnauthorized)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, enrollmentMaxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload enrollmentRequest
	if err := decoder.Decode(&payload); err != nil {
		e.rejectMalformed(response, request, "invalid enrollment request")
		return
	}
	if err := ensureJSONEnd(decoder); err != nil {
		e.rejectMalformed(response, request, "invalid enrollment request")
		return
	}
	payload.Name = strings.TrimSpace(payload.Name)
	if payload.Name == "" || len(payload.Name) > 128 || strings.IndexFunc(payload.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		e.rejectMalformed(response, request, "invalid client name")
		return
	}
	certificate, err := parseCertificate(payload.Certificate, x509.ExtKeyUsageClientAuth)
	if err != nil {
		e.rejectMalformed(response, request, "invalid client certificate")
		return
	}
	if err := e.authorizeWithResponse(request.Context(), payload.Name, payload.Certificate, func(deadline time.Time) error {
		return http.NewResponseController(response).SetWriteDeadline(deadline)
	}); err != nil {
		if errors.Is(err, ErrEnrollmentApprovalDenied) {
			// The local operator can inspect the approver error through Wait;
			// arbitrary local reader or callback details do not cross the wire.
			http.Error(response, ErrEnrollmentApprovalDenied.Error(), http.StatusForbidden)
			return
		}
		if errors.Is(err, errEnrollmentAuthorizationLimit) {
			http.Error(response, err.Error(), http.StatusTooManyRequests)
			return
		}
		if errors.Is(err, errEnrollmentExpired) {
			http.Error(response, err.Error(), http.StatusGone)
			e.finish(enrollmentOutcome{err: err})
			return
		}
		http.Error(response, err.Error(), http.StatusConflict)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusCreated)
	_, _ = response.Write([]byte(`{"status":"authorized"}`))
	e.finish(enrollmentOutcome{result: EnrollmentResult{Name: payload.Name, Fingerprint: certificateFingerprint(certificate)}})
}

func (e *EnrollmentListener) authorize(ctx context.Context, name, certificate string) error {
	return e.authorizeWithResponse(ctx, name, certificate, nil)
}

func (e *EnrollmentListener) authorizeWithResponse(ctx context.Context, name, certificate string, ready func(time.Time) error) error {
	parsed, err := parseCertificate(certificate, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.terminal {
		err := e.terminalErrorLocked()
		e.mu.Unlock()
		return err
	}
	if e.expired || !time.Now().Before(e.expiresAt) {
		e.expired = true
		e.mu.Unlock()
		e.finish(enrollmentOutcome{err: enrollmentExpiredError()})
		return enrollmentExpiredError()
	}
	if e.tokenState != enrollmentTokenAvailable {
		e.mu.Unlock()
		return errors.New("enrollment token already used or in use")
	}
	e.tokenState = enrollmentTokenReserved
	e.mu.Unlock()

	// Advertised expiry is the protocol boundary. Keep this exact context
	// through approval, staging and final commit; the listener's finer-grained
	// timer must not extend the setup code's advertised lifetime.
	deadline := time.Unix(e.setup.ExpiresAt, 0)
	approvalContext, cancelApproval := context.WithDeadline(ctx, deadline)
	stop := context.AfterFunc(e.ctx, cancelApproval)
	defer func() { stop(); cancelApproval() }()
	ctx = approvalContext
	candidate := EnrollmentCandidate{
		ClientName:        name,
		ClientFingerprint: certificateFingerprint(parsed),
		ServerFingerprint: e.setup.Fingerprint,
		Endpoint:          e.setup.Address,
		ExpiresAt:         deadline,
	}
	result := EnrollmentResult{Name: name, Fingerprint: candidate.ClientFingerprint}
	err = e.approveReserved(ctx, candidate, ready)
	if err != nil {
		e.finish(enrollmentOutcome{err: err})
		return err
	}
	ctx = context.WithValue(ctx, explicitApprovalKey{}, true)
	err = e.authorizeClient(ctx, name, certificate, func(persist func() error) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.terminal {
			return e.terminalErrorLocked()
		}
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.expired || !time.Now().Before(e.expiresAt) || !time.Now().Before(deadline) {
			return enrollmentExpiredError()
		}
		if err := persist(); err != nil {
			return err
		}
		e.tokenState = enrollmentTokenUsed
		// The file replacement and its successful outcome share the same
		// lock. No failure can be announced between those two observations.
		e.publishLocked(enrollmentOutcome{result: result})
		return nil
	})
	if err != nil && !time.Now().Before(deadline) {
		err = enrollmentExpiredError()
	}
	e.mu.Lock()
	if err != nil {
		if !e.terminal {
			e.tokenState = enrollmentTokenAvailable
		}
		expired := e.expired || !time.Now().Before(e.expiresAt) || !time.Now().Before(deadline)
		e.expired = expired
		if !e.terminal && !expired && e.ctx.Err() == nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			e.authorizationFailures++
			if e.authorizationFailures >= enrollmentMaxAuthorizationFailures {
				err = errEnrollmentAuthorizationLimit
				e.publishLocked(enrollmentOutcome{err: err})
			}
		}
		e.mu.Unlock()
		if expired {
			e.finish(enrollmentOutcome{err: enrollmentExpiredError()})
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			e.finish(enrollmentOutcome{err: err})
		}
		return err
	}
	e.mu.Unlock()
	return nil
}

func (e *EnrollmentListener) expire() {
	e.mu.Lock()
	e.expired = true
	e.publishLocked(enrollmentOutcome{err: enrollmentExpiredError()})
	e.mu.Unlock()
}

func (e *EnrollmentListener) failedAttempt() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempts++
	if e.attempts >= enrollmentMaxAttempts {
		e.publishLocked(enrollmentOutcome{err: errors.New("enrollment attempt limit exceeded")})
		return true
	}
	return false
}

func (e *EnrollmentListener) finish(outcome enrollmentOutcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.publishLocked(outcome)
}

func (e *EnrollmentListener) terminalErrorLocked() error {
	if e.outcome.err != nil {
		return e.outcome.err
	}
	return errors.New("enrollment token already used")
}

func (e *EnrollmentListener) publishLocked(outcome enrollmentOutcome) {
	if e.terminal {
		return
	}
	e.terminal = true
	if e.cancel != nil {
		e.cancel()
	}
	e.outcome = outcome
	close(e.done)
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := e.server.Shutdown(shutdownCtx)
		if err != nil {
			_ = e.server.Close()
		}
		e.mu.Lock()
		e.shutdownErr = err
		close(e.closed)
		e.mu.Unlock()
	}()
}

var errEnrollmentExpired = errors.New("enrollment code expired")
var errEnrollmentClosed = errors.New("enrollment closed")
var errEnrollmentMalformedLimit = errors.New("enrollment malformed submission limit exceeded")
var errEnrollmentAuthorizationLimit = errors.New("enrollment authorization failure limit exceeded")

func enrollmentExpiredError() error {
	return fmt.Errorf("%w; rerun `crux server setup` to generate a new code", errEnrollmentExpired)
}

func Pair(ctx context.Context, name, setupCode string) (Connection, error) {
	setup, err := DecodeEnrollmentSetup(setupCode)
	if err != nil {
		return Connection{}, err
	}
	name = strings.TrimSpace(name)
	if !validPairingName(name) {
		return Connection{}, errors.New("connection name must be 1-128 bytes without control characters")
	}
	path, err := filepath.Abs(storePath())
	if err != nil {
		return Connection{}, err
	}
	if err := pairingNameAvailable(ctx, path, name); err != nil {
		return Connection{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, time.Unix(setup.ExpiresAt, 0))
	defer cancel()
	address, err := url.Parse(setup.Address)
	if err != nil {
		return Connection{}, err
	}
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // The setup code supplies the exact certificate pin.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("enrollment server presented an unexpected certificate chain")
			}
			code := base64.RawURLEncoding.EncodeToString(state.PeerCertificates[0].Raw)
			validated, err := parseCertificate(code, x509.ExtKeyUsageServerAuth)
			if err != nil {
				return fmt.Errorf("validate enrollment server certificate: %w", err)
			}
			expected, _ := hex.DecodeString(setup.Fingerprint)
			actual, _ := hex.DecodeString(certificateFingerprint(validated))
			if subtle.ConstantTimeCompare(actual, expected) != 1 {
				return errors.New("enrollment server certificate fingerprint does not match the setup code")
			}
			return nil
		},
	}
	// Pin the full certificate without sending the setup token or an enrollment
	// request. Persistence must finish before the authorization-bearing POST.
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: tlsConfig}
	preflight, err := dialer.DialContext(ctx, "tcp", address.Host)
	if err != nil {
		return Connection{}, fmt.Errorf("verify enrollment server: %w", err)
	}
	state := preflight.(*tls.Conn).ConnectionState()
	_ = preflight.Close()
	if len(state.PeerCertificates) != 1 {
		return Connection{}, errors.New("enrollment server certificate was not captured")
	}
	identity, err := NewClientIdentity(name)
	if err != nil {
		return Connection{}, err
	}
	created := Connection{Name: name, Address: setup.Address, ServerCertificate: base64.RawURLEncoding.EncodeToString(state.PeerCertificates[0].Raw), Client: identity}
	entry, err := stagePendingPairing(ctx, path, created)
	if err != nil {
		return Connection{}, err
	}
	pending := func(err error) (Connection, error) {
		return Connection{}, &PairingPendingError{OperationID: entry.OperationID, Cause: err}
	}
	body, err := json.Marshal(enrollmentRequest{Name: name, Certificate: identity.Certificate})
	if err != nil {
		return pending(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+address.Host+enrollmentPath, bytes.NewReader(body))
	if err != nil {
		return pending(err)
	}
	request.Header.Set("Authorization", "Crux-Enrollment "+setup.Token)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 10 * time.Second, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return pending(fmt.Errorf("enroll client: %w", err))
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil || len(responseBody) > 16<<10 {
		return pending(errors.New("could not read bounded enrollment response"))
	}
	if response.StatusCode != http.StatusCreated {
		return pending(fmt.Errorf("enrollment did not confirm authorization (HTTP %d)", response.StatusCode))
	}
	if !validEnrollmentReply(responseBody) {
		return pending(errors.New("enrollment returned an invalid authorization receipt"))
	}
	return promotePendingPairing(ctx, path, entry, "")
}

func validEnrollmentReply(body []byte) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return false
	}
	t, err = d.Token()
	if err != nil || t != "status" {
		return false
	}
	t, err = d.Token()
	if err != nil || t != "authorized" {
		return false
	}
	t, err = d.Token()
	if err != nil || t != json.Delim('}') {
		return false
	}
	_, err = d.Token()
	return errors.Is(err, io.EOF)
}

func DecodeEnrollmentSetup(setupCode string) (EnrollmentSetup, error) {
	setupCode = strings.TrimSpace(setupCode)
	if setupCode == "" || len(setupCode) > 4096 {
		return EnrollmentSetup{}, errors.New("invalid enrollment setup code")
	}
	content, err := base64.RawURLEncoding.DecodeString(setupCode)
	if err != nil {
		return EnrollmentSetup{}, errors.New("invalid enrollment setup code")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var setup EnrollmentSetup
	if err := decoder.Decode(&setup); err != nil {
		return EnrollmentSetup{}, errors.New("invalid enrollment setup code")
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return EnrollmentSetup{}, errors.New("invalid enrollment setup code")
	}
	if setup.Version != enrollmentVersion {
		return EnrollmentSetup{}, fmt.Errorf("unsupported enrollment setup version: %d", setup.Version)
	}
	address, err := NormalizeConnectionAddress(setup.Address)
	if err != nil || address != setup.Address {
		return EnrollmentSetup{}, errors.New("enrollment setup contains an invalid server address")
	}
	token, err := base64.RawURLEncoding.DecodeString(setup.Token)
	if err != nil || len(token) != enrollmentTokenBytes {
		return EnrollmentSetup{}, errors.New("enrollment setup contains an invalid token")
	}
	fingerprint, err := hex.DecodeString(setup.Fingerprint)
	if err != nil || len(fingerprint) != 32 || setup.Fingerprint != strings.ToLower(setup.Fingerprint) {
		return EnrollmentSetup{}, errors.New("enrollment setup contains an invalid server fingerprint")
	}
	if !time.Now().Before(time.Unix(setup.ExpiresAt, 0)) {
		return EnrollmentSetup{}, errors.New("enrollment setup has expired")
	}
	return setup, nil
}

func parseEnrollmentAddress(address string, allowZero bool) (string, string, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "tcp" || parsed.User != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("address must use tcp://host:port: %s", address)
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host == "" || port == "" {
		return "", "", fmt.Errorf("address must include a host and port: %s", address)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 || (!allowZero && portNumber == 0) {
		return "", "", fmt.Errorf("address has an invalid port: %s", address)
	}
	return host, strconv.Itoa(portNumber), nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request contains multiple JSON values")
		}
		return err
	}
	return nil
}
