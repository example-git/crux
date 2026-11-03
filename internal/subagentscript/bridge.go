package subagentscript

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type Bridge struct {
	server      *http.Server
	environment []string
}

func NewBridge(service Caller, sessionID, parentSessionID string) (*Bridge, error) {
	if sessionID == "" {
		return nil, errors.New("subagent scripting requires a session")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(secret)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/subagents", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 || r.Header.Get("X-Crux-Session-ID") != sessionID {
			http.Error(w, "invalid session binding", http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(r.Body)
		var request Request
		if err == nil {
			err = Decode(data, &request)
		}
		if err == nil {
			err = request.Validate()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := service.Call(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		data, err = json.Marshal(result)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(data, '\n'))
	})
	server := &http.Server{Handler: mux}
	bridge := &Bridge{server: server, environment: []string{
		SessionEnv + "=" + sessionID,
		ParentEnv + "=" + parentSessionID,
		URLEnv + "=http://" + listener.Addr().String() + "/v1/subagents",
		TokenEnv + "=" + token,
	}}
	go func() { _ = server.Serve(listener) }()
	return bridge, nil
}

func (b *Bridge) Environment() []string {
	return append([]string(nil), b.environment...)
}

func (b *Bridge) Close() error {
	return b.server.Close()
}

type Client struct {
	SessionID       string
	ParentSessionID string
	endpoint        string
	token           string
	http            *http.Client
}

func ClientFromEnvironment(getenv func(string) string) (*Client, error) {
	sessionID, endpoint, token := getenv(SessionEnv), getenv(URLEnv), getenv(TokenEnv)
	if sessionID == "" || endpoint == "" || token == "" {
		return nil, errors.New("subagent commands require the session environment supplied by Crux")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" || parsed.Path != "/v1/subagents" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid Crux subagent endpoint")
	}
	return &Client{
		SessionID: sessionID, ParentSessionID: getenv(ParentEnv), endpoint: endpoint, token: token,
		http: &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *Client) Call(ctx context.Context, request Request) (any, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Crux-Session-ID", c.SessionID)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subagent session API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("subagent session API: %s", strings.TrimSpace(string(data)))
	}
	var result any
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) Close() {
	c.http.CloseIdleConnections()
}
