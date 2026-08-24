// Package manifestflow executes finite declarative OAuth flows from trusted
// provider manifests. It does not load bundles, execute plugin code, or expose
// credentials outside the host process.
package manifestflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/callback"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

const defaultMaxBodyBytes int64 = 1 << 20

type Executor struct {
	providerName string
	flow         manifest.OAuthFlow
	endpoints    map[string]manifest.Endpoint
	client       *http.Client
}

func New(value manifest.Manifest, flow manifest.OAuthFlow) (*Executor, error) {
	endpoints := make(map[string]manifest.Endpoint, len(value.Capabilities.Endpoints))
	for _, endpoint := range value.Capabilities.Endpoints {
		endpoints[endpoint.ID] = endpoint
	}
	if _, ok := endpoints[flow.AuthorizationEndpoint]; !ok {
		return nil, fmt.Errorf("OAuth flow %q references missing authorization endpoint", flow.ID)
	}
	if _, ok := endpoints[flow.TokenEndpoint]; !ok {
		return nil, fmt.Errorf("OAuth flow %q references missing token endpoint", flow.ID)
	}
	return &Executor{providerName: value.Provider.Name, flow: flow, endpoints: endpoints}, nil
}

func (e *Executor) httpClient(endpoint manifest.Endpoint) *http.Client {
	client := &http.Client{}
	if e.client != nil {
		clone := *e.client
		client = &clone
	}
	if !endpoint.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client
}

func (e *Executor) endpoint(id string) (manifest.Endpoint, *url.URL, error) {
	endpoint, ok := e.endpoints[id]
	if !ok {
		return manifest.Endpoint{}, nil, fmt.Errorf("unknown endpoint %q", id)
	}
	u, err := url.Parse(endpoint.BaseURL)
	if err != nil || u.Hostname() == "" {
		return manifest.Endpoint{}, nil, fmt.Errorf("invalid endpoint %q", id)
	}
	if !containsFold(endpoint.AllowedSchemes, u.Scheme) || !containsFold(endpoint.AllowedHosts, u.Hostname()) {
		return manifest.Endpoint{}, nil, fmt.Errorf("endpoint %q violates its origin allowlist", id)
	}
	return endpoint, u, nil
}

func (e *Executor) Authorize(ctx context.Context, open func(string) error, readCode func() (string, error)) (*oauth.Token, error) {
	verifier, challenge, err := createPKCE(e.flow.PKCE)
	if err != nil {
		return nil, err
	}
	state := ""
	if e.flow.Redirect.StateRequired {
		state, err = randomString(32)
		if err != nil {
			return nil, err
		}
	}
	if e.flow.Redirect.Mode == "hosted-paste" {
		return e.authorizeHostedPaste(ctx, open, readCode, verifier, challenge, state)
	}
	if e.flow.Redirect.Mode != "loopback-dynamic" && e.flow.Redirect.Mode != "loopback-fixed" {
		return nil, fmt.Errorf("OAuth redirect mode %q requires another host adapter", e.flow.Redirect.Mode)
	}
	address := "localhost:0"
	if e.flow.Redirect.Mode == "loopback-fixed" {
		address = net.JoinHostPort("localhost", strconv.Itoa(e.flow.Redirect.Port))
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback server: %w", err)
	}
	defer listener.Close()
	path := e.flow.Redirect.CallbackPath
	if path == "" {
		path = "/callback"
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d%s", port, path)
	authorizationURL, err := e.authorizationURL(redirectURI, challenge, state)
	if err != nil {
		return nil, err
	}

	type result struct {
		token *oauth.Token
		err   error
	}
	results := make(chan result, 1)
	var once sync.Once
	finish := func(value result) { once.Do(func() { results <- value }) }
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if providerError := query.Get("error"); providerError != "" {
			detail := query.Get("error_description")
			_ = callback.Serve(w, callback.Result{Subject: e.providerName, ErrorCode: providerError, ErrorDescription: detail})
			finish(result{err: fmt.Errorf("OAuth authorization failed: %s", providerError)})
			return
		}
		if (e.flow.Redirect.StateRequired && query.Get("state") != state) || query.Get("code") == "" {
			_ = callback.Serve(w, callback.Result{Subject: e.providerName, ErrorCode: "invalid_request", ErrorDescription: "Invalid OAuth callback."})
			finish(result{err: errors.New("OAuth callback validation failed")})
			return
		}
		token, exchangeErr := e.exchange(ctx, e.flow.TokenRequest.Code, map[string]string{
			"oauth.code": query.Get("code"), "oauth.redirect_uri": redirectURI,
			"oauth.pkce_verifier": verifier, "oauth.state": state,
		}, "")
		if exchangeErr != nil {
			_ = callback.Serve(w, callback.Result{Subject: e.providerName, ErrorCode: "token_exchange_failed", ErrorDescription: exchangeErr.Error()})
			finish(result{err: exchangeErr})
			return
		}
		_ = callback.Serve(w, callback.Result{Subject: e.providerName})
		finish(result{token: token})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 15 * time.Second}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			finish(result{err: fmt.Errorf("OAuth callback server: %w", serveErr)})
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if open != nil {
		if err := open(authorizationURL); err != nil {
			return nil, fmt.Errorf("open authorization URL: %w", err)
		}
	}
	timeout := time.Duration(e.flow.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case value := <-results:
		return value.token, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("OAuth authorization timed out")
	}
}

func (e *Executor) authorizeHostedPaste(ctx context.Context, open func(string) error, readCode func() (string, error), verifier, challenge, state string) (*oauth.Token, error) {
	if open == nil {
		return nil, errors.New("hosted OAuth flow requires an authorization URL opener")
	}
	if readCode == nil {
		return nil, errors.New("hosted OAuth flow requires pasted callback input")
	}
	redirectURI := e.flow.Redirect.URI
	if redirectURI == "" {
		return nil, errors.New("hosted OAuth flow has no redirect URI")
	}
	authorizationURL, err := e.authorizationURL(redirectURI, challenge, state)
	if err != nil {
		return nil, err
	}
	if err := open(authorizationURL); err != nil {
		return nil, fmt.Errorf("open authorization URL: %w", err)
	}
	type pastedResult struct {
		value string
		err   error
	}
	pasted := make(chan pastedResult, 1)
	go func() {
		value, readErr := readCode()
		pasted <- pastedResult{value: value, err: readErr}
	}()
	timeout := time.Duration(e.flow.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var input string
	select {
	case result := <-pasted:
		if result.err != nil {
			return nil, result.err
		}
		input = result.value
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("OAuth authorization timed out")
	}
	code, returnedState, providerError, err := parseHostedCallback(input)
	if err != nil {
		return nil, err
	}
	if providerError != "" {
		return nil, fmt.Errorf("OAuth authorization failed: %s", providerError)
	}
	// Hosted providers commonly display only the authorization code. Validate
	// state whenever the pasted value carries it; a mismatched value always
	// fails, while a bare code retains compatibility with those providers.
	if returnedState != "" && returnedState != state {
		return nil, errors.New("OAuth state mismatch — possible CSRF, please try again")
	}
	return e.exchange(ctx, e.flow.TokenRequest.Code, map[string]string{
		"oauth.code": code, "oauth.redirect_uri": redirectURI,
		"oauth.pkce_verifier": verifier, "oauth.state": state,
	}, "")
}

func parseHostedCallback(input string) (code, state, providerError string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", "", errors.New("no authorization code provided")
	}
	var values url.Values
	if parsed, parseErr := url.Parse(input); parseErr == nil && parsed.IsAbs() {
		values = parsed.Query()
		if len(values) == 0 && parsed.Fragment != "" {
			values, _ = url.ParseQuery(parsed.Fragment)
		}
	} else if strings.Contains(input, "=") {
		raw := strings.TrimPrefix(strings.TrimPrefix(input, "?"), "#")
		values, _ = url.ParseQuery(raw)
	}
	if values != nil {
		if providerError := values.Get("error"); providerError != "" {
			return "", values.Get("state"), providerError, nil
		}
		if code := values.Get("code"); code != "" {
			return code, values.Get("state"), "", nil
		}
		return "", "", "", errors.New("pasted OAuth callback did not include an authorization code")
	}
	if strings.ContainsAny(input, " \t\r\n") {
		return "", "", "", errors.New("could not parse an authorization code from the pasted value")
	}
	return input, "", "", nil
}

func (e *Executor) Refresh(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	return e.exchange(ctx, e.flow.TokenRequest.Refresh, map[string]string{"oauth.refresh_token": refreshToken}, refreshToken)
}

func (e *Executor) authorizationURL(redirectURI, challenge, state string) (string, error) {
	_, u, err := e.endpoint(e.flow.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	values := u.Query()
	clientID, err := eval(e.flow.ClientID, nil)
	if err != nil {
		return "", err
	}
	values.Set("client_id", clientID)
	values.Set("response_type", "code")
	values.Set("redirect_uri", redirectURI)
	values.Set("scope", strings.Join(e.flow.Scopes, " "))
	if challenge != "" {
		values.Set("code_challenge", challenge)
		values.Set("code_challenge_method", "S256")
	}
	if state != "" {
		values.Set("state", state)
	}
	contextValues := map[string]string{"oauth.redirect_uri": redirectURI, "oauth.pkce_challenge": challenge, "oauth.state": state}
	for _, rule := range e.flow.AuthorizationParams {
		value, err := eval(rule.Value, contextValues)
		if err != nil {
			return "", fmt.Errorf("authorization parameter %q: %w", rule.Name, err)
		}
		values.Set(rule.Name, value)
	}
	u.RawQuery = values.Encode()
	return u.String(), nil
}

func (e *Executor) exchange(ctx context.Context, rules []manifest.FieldRule, values map[string]string, previousRefresh string) (*oauth.Token, error) {
	if timeout := time.Duration(e.flow.TimeoutSeconds) * time.Second; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	endpoint, target, err := e.endpoint(e.flow.TokenEndpoint)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]string, len(rules))
	for _, rule := range rules {
		value, err := eval(rule.Value, values)
		if err != nil {
			return nil, fmt.Errorf("token field %q: %w", rule.Name, err)
		}
		if rule.OmitEmpty && value == "" {
			continue
		}
		fields[rule.Name] = value
	}
	var body io.Reader
	contentType := "application/json"
	if e.flow.TokenRequest.Encoding == "form" {
		form := make(url.Values, len(fields))
		for key, value := range fields {
			form.Set(key, value)
		}
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	} else {
		payload, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentType)
	for _, rule := range e.flow.TokenRequest.Headers {
		if err := applyHeader(request.Header, rule, values); err != nil {
			return nil, err
		}
	}
	response, err := e.httpClient(endpoint).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	maximum := e.flow.TokenResponse.MaxBodyBytes
	if maximum <= 0 {
		maximum = defaultMaxBodyBytes
	}
	data, err := readBounded(response.Body, maximum)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(data))
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return nil, &oauth.TokenExchangeError{StatusCode: response.StatusCode, Body: detail}
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	access, _ := pointer(document, e.flow.TokenResponse.AccessTokenPointer).(string)
	if access == "" {
		return nil, errors.New("OAuth response did not include an access token")
	}
	refresh, _ := pointer(document, e.flow.TokenResponse.RefreshTokenPointer).(string)
	if refresh == "" && e.flow.TokenResponse.PreserveRefreshToken {
		refresh = previousRefresh
	}
	expires := int64Value(pointer(document, e.flow.TokenResponse.ExpiresInPointer))
	if expires <= 0 {
		expires = e.flow.TokenResponse.DefaultExpiresIn
	}
	clientID, _ := eval(e.flow.ClientID, nil)
	_, authorizationURL, _ := e.endpoint(e.flow.AuthorizationEndpoint)
	token := &oauth.Token{AccessToken: access, RefreshToken: refresh, ExpiresIn: int(expires), Client: &oauth.OAuthClient{ClientID: clientID, AuthURL: authorizationURL.String(), TokenURL: target.String()}}
	token.SetExpiresAt()
	return token, nil
}

func applyHeader(headers http.Header, rule manifest.HeaderRule, values map[string]string) error {
	if rule.Operation == "delete" {
		headers.Del(rule.Name)
		return nil
	}
	if rule.Value == nil {
		return fmt.Errorf("header %q has no value", rule.Name)
	}
	value, err := eval(*rule.Value, values)
	if err != nil {
		return err
	}
	switch rule.Operation {
	case "set":
		headers.Set(rule.Name, value)
	case "set-if-absent":
		if headers.Get(rule.Name) == "" {
			headers.Set(rule.Name, value)
		}
	case "append":
		headers.Add(rule.Name, value)
	case "append-unique":
		for _, current := range strings.Split(headers.Get(rule.Name), ",") {
			if strings.TrimSpace(current) == value {
				return nil
			}
		}
		if current := headers.Get(rule.Name); current != "" {
			headers.Set(rule.Name, current+","+value)
		} else {
			headers.Set(rule.Name, value)
		}
	default:
		return fmt.Errorf("unsupported header operation %q", rule.Operation)
	}
	return nil
}

func eval(value manifest.Template, contextValues map[string]string) (string, error) {
	switch value.Kind {
	case "literal":
		switch typed := value.Value.(type) {
		case string:
			return typed, nil
		case nil:
			return "", nil
		default:
			data, err := json.Marshal(typed)
			return string(data), err
		}
	case "context":
		return contextValues[value.Ref], nil
	case "concat":
		var result strings.Builder
		for _, part := range value.Parts {
			text, err := eval(part, contextValues)
			if err != nil {
				return "", err
			}
			result.WriteString(text)
		}
		return result.String(), nil
	default:
		return "", fmt.Errorf("template kind %q is unavailable in OAuth context", value.Kind)
	}
}

func createPKCE(mode string) (string, string, error) {
	if mode == "disabled" {
		return "", "", nil
	}
	verifier, err := randomString(32)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func randomString(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	return data, nil
}

func containsFold(values []string, sought string) bool {
	for _, value := range values {
		if strings.EqualFold(value, sought) {
			return true
		}
	}
	return false
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case json.Number:
		result, _ := typed.Int64()
		return result
	case int64:
		return typed
	}
	return 0
}

func pointer(value any, path string) any {
	if path == "" {
		return nil
	}
	current := value
	for _, raw := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}
