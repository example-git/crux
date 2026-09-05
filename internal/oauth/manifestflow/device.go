package manifestflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

type DeviceAuthorization struct {
	UserCode        string
	VerificationURL string
	state           deviceState
}

type deviceState struct {
	deviceCode string
	interval   time.Duration
	expiresAt  time.Time
}

func (e *Executor) RequestDeviceCode(ctx context.Context) (*DeviceAuthorization, error) {
	declaration := e.flow.DeviceCode
	if e.flow.Redirect.Mode != "device-code" || declaration == nil {
		return nil, fmt.Errorf("OAuth flow %q does not declare device-code authorization", e.flow.ID)
	}
	values := map[string]string{"oauth.scopes": strings.Join(e.flow.Scopes, " ")}
	clientID, err := e.eval(e.flow.ClientID, values)
	if err != nil {
		return nil, err
	}
	values["oauth.client_id"] = clientID
	document, err := e.deviceRequest(ctx, declaration.Endpoint, declaration.Request, declaration.Headers, values, declaration.MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	deviceCode, _ := pointer(document, declaration.DeviceCodePointer).(string)
	userCode, _ := pointer(document, declaration.UserCodePointer).(string)
	verificationURL, _ := pointer(document, declaration.VerificationURLPointer).(string)
	if deviceCode == "" || userCode == "" || verificationURL == "" {
		return nil, errors.New("OAuth device authorization response is incomplete")
	}
	interval := int64Value(pointer(document, declaration.IntervalPointer))
	if interval <= 0 {
		interval = int64(declaration.DefaultIntervalSeconds)
	}
	expires := int64Value(pointer(document, declaration.ExpiresInPointer))
	if expires <= 0 {
		expires = int64(e.flow.TimeoutSeconds)
	}
	if expires <= 0 {
		return nil, errors.New("OAuth device authorization has no expiry")
	}
	return &DeviceAuthorization{UserCode: userCode, VerificationURL: verificationURL, state: deviceState{deviceCode: deviceCode, interval: time.Duration(interval) * time.Second, expiresAt: time.Now().Add(time.Duration(expires) * time.Second)}}, nil
}

func (e *Executor) PollDeviceCode(ctx context.Context, authorization *DeviceAuthorization) (*oauth.Token, error) {
	declaration := e.flow.DeviceCode
	if declaration == nil || authorization == nil || authorization.state.deviceCode == "" {
		return nil, errors.New("OAuth device authorization state is invalid")
	}
	interval := authorization.state.interval
	for {
		remaining := time.Until(authorization.state.expiresAt)
		if remaining <= 0 {
			return nil, errors.New("OAuth device authorization expired")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		values := map[string]string{"oauth.device_code": authorization.state.deviceCode}
		clientID, err := e.eval(e.flow.ClientID, values)
		if err != nil {
			return nil, err
		}
		values["oauth.client_id"] = clientID
		document, err := e.deviceRequest(ctx, e.flow.TokenEndpoint, declaration.Poll, e.flow.TokenRequest.Headers, values, declaration.MaxBodyBytes)
		if exchangeError, ok := err.(*oauth.TokenExchangeError); ok {
			var failure any
			if json.Unmarshal([]byte(exchangeError.Body), &failure) == nil {
				code, _ := pointer(failure, declaration.ErrorPointer).(string)
				switch code {
				case "authorization_pending":
					continue
				case "slow_down":
					interval += 5 * time.Second
					continue
				case "expired_token":
					return nil, errors.New("OAuth device authorization expired")
				case "access_denied":
					return nil, errors.New("OAuth device authorization was denied")
				}
			}
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		return e.tokenFromDocument(document, "")
	}
}

func (e *Executor) deviceRequest(ctx context.Context, endpointID string, rules []manifest.FieldRule, headers []manifest.HeaderRule, values map[string]string, maximum int64) (any, error) {
	endpoint, target, err := e.endpoint(endpointID)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]string, len(rules))
	for _, rule := range rules {
		value, err := e.eval(rule.Value, values)
		if err != nil {
			return nil, err
		}
		if !rule.OmitEmpty || value != "" {
			fields[rule.Name] = value
		}
	}
	var body io.Reader
	contentType := "application/json"
	if e.flow.TokenRequest.Encoding == "form" {
		form := make(url.Values, len(fields))
		for name, value := range fields {
			form.Set(name, value)
		}
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	} else {
		data, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentType)
	for _, rule := range headers {
		if err := e.applyHeader(request.Header, rule, values); err != nil {
			return nil, err
		}
	}
	response, err := providertransport.ClientWithContextOwnerValidator(ctx, e.httpClient(endpoint)).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if maximum <= 0 {
		maximum = defaultMaxBodyBytes
	}
	data, err := readBounded(response.Body, maximum)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &oauth.TokenExchangeError{StatusCode: response.StatusCode, Body: string(data)}
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	return document, nil
}

func (e *Executor) tokenFromDocument(document any, previousRefresh string) (*oauth.Token, error) {
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
	clientID, _ := e.eval(e.flow.ClientID, nil)
	_, authorizationURL, _ := e.endpoint(e.flow.AuthorizationEndpoint)
	_, tokenURL, _ := e.endpoint(e.flow.TokenEndpoint)
	token := &oauth.Token{AccessToken: access, RefreshToken: refresh, ExpiresIn: int(expires), Client: &oauth.OAuthClient{ClientID: clientID, AuthURL: authorizationURL.String(), TokenURL: tokenURL.String()}}
	token.SetExpiresAt()
	return token, nil
}
