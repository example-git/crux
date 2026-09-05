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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	enrollmentVersion         = 1
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
	setup           EnrollmentSetup
	code            string
	listener        net.Listener
	server          *http.Server
	result          chan enrollmentOutcome
	done            chan struct{}
	once            sync.Once
	mu              sync.Mutex
	attempts        int
	tokenState      enrollmentTokenState
	expired         bool
	expiresAt       time.Time
	authorizeClient func(context.Context, string, string) error
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

func StartEnrollment(ctx context.Context, listenAddress, advertisedAddress string, ttl time.Duration) (*EnrollmentListener, error) {
	if ttl <= 0 {
		return nil, errors.New("enrollment expiry must be positive")
	}
	listenHost, listenPort, err := parseEnrollmentAddress(listenAddress, true)
	if err != nil {
		return nil, fmt.Errorf("invalid enrollment listen address: %w", err)
	}
	serverIdentity, exists, err := ServerIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("server identity is not initialized")
	}
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
	enrollment := &EnrollmentListener{
		setup:           setup,
		code:            base64.RawURLEncoding.EncodeToString(codeBytes),
		listener:        listener,
		result:          make(chan enrollmentOutcome, 1),
		done:            make(chan struct{}),
		expiresAt:       expiresAt,
		authorizeClient: AuthorizeClient,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+enrollmentPath, enrollment.handleEnrollment)
	enrollment.server = &http.Server{
		Handler:           mux,
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
		err := enrollment.server.Serve(tls.NewListener(listener, tlsConfig))
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
	case outcome := <-e.result:
		return outcome.result, outcome.err
	case <-ctx.Done():
		return EnrollmentResult{}, ctx.Err()
	}
}

func (e *EnrollmentListener) Close() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return e.server.Shutdown(shutdownCtx)
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
		http.Error(response, "invalid enrollment request", http.StatusBadRequest)
		return
	}
	if err := ensureJSONEnd(decoder); err != nil {
		http.Error(response, "invalid enrollment request", http.StatusBadRequest)
		return
	}
	payload.Name = strings.TrimSpace(payload.Name)
	if payload.Name == "" || len(payload.Name) > 128 || strings.IndexFunc(payload.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		http.Error(response, "invalid client name", http.StatusBadRequest)
		return
	}
	certificate, err := parseCertificate(payload.Certificate, x509.ExtKeyUsageClientAuth)
	if err != nil {
		http.Error(response, "invalid client certificate", http.StatusBadRequest)
		return
	}
	if err := e.authorize(request.Context(), payload.Name, payload.Certificate); err != nil {
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
	e.mu.Lock()
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

	err := e.authorizeClient(ctx, name, certificate)
	e.mu.Lock()
	if err != nil {
		e.tokenState = enrollmentTokenAvailable
		expired := e.expired || !time.Now().Before(e.expiresAt)
		e.expired = expired
		e.mu.Unlock()
		if expired {
			e.finish(enrollmentOutcome{err: enrollmentExpiredError()})
		}
		return err
	}
	e.tokenState = enrollmentTokenUsed
	e.mu.Unlock()
	return nil
}

func (e *EnrollmentListener) expire() {
	e.mu.Lock()
	e.expired = true
	idle := e.tokenState == enrollmentTokenAvailable
	e.mu.Unlock()
	if idle {
		e.finish(enrollmentOutcome{err: enrollmentExpiredError()})
	}
}

func (e *EnrollmentListener) failedAttempt() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempts++
	return e.attempts >= enrollmentMaxAttempts
}

func (e *EnrollmentListener) finish(outcome enrollmentOutcome) {
	e.once.Do(func() {
		e.result <- outcome
		close(e.done)
		go func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = e.server.Shutdown(shutdownCtx)
		}()
	})
}

var errEnrollmentExpired = errors.New("enrollment code expired")

func enrollmentExpiredError() error {
	return fmt.Errorf("%w; rerun `crux server setup` to generate a new code", errEnrollmentExpired)
}

func Pair(ctx context.Context, name, setupCode string) (Connection, error) {
	setup, err := DecodeEnrollmentSetup(setupCode)
	if err != nil {
		return Connection{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Connection{}, errors.New("connection name cannot be empty")
	}
	if _, exists, err := Get(ctx, name); err != nil {
		return Connection{}, err
	} else if exists {
		return Connection{}, fmt.Errorf("connection already exists: %s", name)
	}
	clientIdentity, err := NewClientIdentity(name)
	if err != nil {
		return Connection{}, err
	}
	var pinnedCertificate string
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("enrollment server presented an unexpected certificate chain")
			}
			certificate := state.PeerCertificates[0]
			code := base64.RawURLEncoding.EncodeToString(certificate.Raw)
			validated, err := parseCertificate(code, x509.ExtKeyUsageServerAuth)
			if err != nil {
				return fmt.Errorf("validate enrollment server certificate: %w", err)
			}
			expected, _ := hex.DecodeString(setup.Fingerprint)
			actual, _ := hex.DecodeString(certificateFingerprint(validated))
			if subtle.ConstantTimeCompare(actual, expected) != 1 {
				return errors.New("enrollment server certificate fingerprint does not match the setup code")
			}
			pinnedCertificate = code
			return nil
		},
	}
	address, err := url.Parse(setup.Address)
	if err != nil {
		return Connection{}, err
	}
	body, err := json.Marshal(enrollmentRequest{Name: name, Certificate: clientIdentity.Certificate})
	if err != nil {
		return Connection{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+address.Host+enrollmentPath, bytes.NewReader(body))
	if err != nil {
		return Connection{}, err
	}
	request.Header.Set("Authorization", "Crux-Enrollment "+setup.Token)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return Connection{}, fmt.Errorf("enroll client: %w", err)
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if readErr != nil {
		return Connection{}, fmt.Errorf("read enrollment response: %w", readErr)
	}
	if response.StatusCode != http.StatusCreated {
		message := strings.TrimSpace(string(responseBody))
		if message == "" {
			message = response.Status
		}
		return Connection{}, fmt.Errorf("enrollment failed: %s", message)
	}
	if pinnedCertificate == "" {
		return Connection{}, errors.New("enrollment server certificate was not captured")
	}
	created := Connection{Name: name, Address: setup.Address, ServerCertificate: pinnedCertificate, Client: clientIdentity}
	if err := SaveConnection(ctx, created); err != nil {
		return Connection{}, err
	}
	return created, nil
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
	if time.Now().Unix() > setup.ExpiresAt {
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
