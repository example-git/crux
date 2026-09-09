package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderGenerateKeepsCapturedRedirectDestination(t *testing.T) {
	t.Setenv("COPILOT_CLI_VERSION", "1.2.3")
	t.Setenv("COPILOT_ADVERTISE_MODE", "cli")
	t.Setenv("ANTIGRAVITY_CLI_VERSION", "1.2.3")
	for _, kind := range []string{"generic-preset", "copilot", "copilot-operation", "anthropic-native", "anthropic-operation", "openai-operation", "gemini-native", "gemini-operation"} {
		declared := strings.HasSuffix(kind, "-operation")
		for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			for _, mode := range []string{"relative", "other-port", "other-host", "owner-changed", "disabled", "allowed-port"} {
				if !declared && (mode == "disabled" || mode == "allowed-port") {
					continue
				}
				t.Run(fmt.Sprintf("%s/%d/%s", kind, status, mode), func(t *testing.T) {
					var starts, sameOrigin, otherOrigin atomic.Int32
					var ownerChanged atomic.Bool
					check := func(r *http.Request) {
						assert.Equal(t, http.MethodPost, r.Method)
						if strings.HasPrefix(kind, "anthropic") {
							assert.Equal(t, "synthetic-provider-key", r.Header.Get("x-api-key"))
						} else {
							assert.Equal(t, "Bearer synthetic-provider-key", r.Header.Get("Authorization"))
						}
						assert.Equal(t, "synthetic-private-header", r.Header.Get("X-Private"))
						body, err := io.ReadAll(r.Body)
						assert.NoError(t, err)
						assert.Contains(t, string(body), "captured prompt")
						if kind == "openai-operation" {
							assert.Contains(t, string(body), `"configured":"synthetic-transform"`)
						}
					}
					finish := func(w http.ResponseWriter, r *http.Request) {
						check(r)
						w.Header().Set("Content-Type", "application/json")
						switch {
						case strings.HasPrefix(kind, "anthropic"):
							_, _ = io.WriteString(w, `{"id":"synthetic","type":"message","role":"assistant","model":"fixture","content":[{"type":"text","text":"redirect accepted"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
						case strings.HasPrefix(kind, "gemini"):
							_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"redirect accepted"}]},"finishReason":"STOP"}]}}`)
						case kind == "openai-operation":
							_, _ = io.WriteString(w, `{"id":"resp_synthetic","object":"response","created_at":1,"status":"completed","model":"fixture","output":[{"id":"msg_synthetic","type":"message","role":"assistant","content":[{"type":"output_text","text":"redirect accepted"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
						default:
							_, _ = io.WriteString(w, `{"id":"synthetic","object":"chat.completion","model":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"redirect accepted"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
						}
					}
					sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherOrigin.Add(1); finish(w, r) }))
					defer sink.Close()
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/next") {
							sameOrigin.Add(1)
							assert.Equal(t, "selected", r.URL.Query().Get("source"))
							if kind == "openai-operation" {
								assert.Equal(t, "/wire/next", r.URL.Path, "relative redirects resolve from the actual operation path")
							}
							finish(w, r)
							return
						}
						starts.Add(1)
						check(r)
						location := "next?source=selected"
						switch mode {
						case "other-port", "allowed-port":
							location = sink.URL + "/next"
						case "other-host":
							location = strings.Replace(sink.URL, "127.0.0.1", "localhost", 1) + "/next"
						case "owner-changed":
							ownerChanged.Store(true)
						}
						http.Redirect(w, r, location, status)
					}))
					defer server.Close()
					previousClient, previousTransport := http.DefaultClient, http.DefaultTransport
					http.DefaultClient, http.DefaultTransport = server.Client(), server.Client().Transport
					defer func() { http.DefaultClient, http.DefaultTransport = previousClient, previousTransport }()
					validate := func() error {
						if ownerChanged.Load() {
							return fmt.Errorf("synthetic owner changed")
						}
						return nil
					}
					headers := map[string]string{"X-Private": "synthetic-private-header"}
					var operation *providertransport.Operation
					if declared {
						operation = &providertransport.Operation{
							Endpoint: manifest.Endpoint{BaseURL: server.URL, AllowedSchemes: []string{"https"}, AllowedHosts: []string{"127.0.0.1"}, Override: "same-origin", FollowRedirects: mode != "disabled"},
							ID:       "inference", Path: "/wire/start", Method: http.MethodPost, Retry: manifest.RetryPolicy{MaxAttempts: 1},
						}
						if mode == "allowed-port" {
							operation.Endpoint.Override = "allowed-hosts"
						}
					}
					c := &coordinator{}
					var provider fantasy.Provider
					var err error
					switch kind {
					case "generic-preset", "copilot", "copilot-operation":
						construction := providerregistry.Construction("")
						if strings.HasPrefix(kind, "copilot") {
							construction = providerregistry.ConstructionCopilot
						}
						provider, err = c.buildOpenaiCompatProvider(false, server.URL+"/v1", "synthetic-provider-key", headers, nil, construction, operation, false, validate)
					case "anthropic-native", "anthropic-operation":
						provider, err = c.buildAnthropicProvider(false, server.URL, "synthetic-provider-key", headers, nil, operation, validate)
					case "openai-operation":
						operation.RequestTransform = &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "set", Path: "/configured", Value: &manifest.Template{Kind: "literal", Value: "synthetic-transform"}}}}
						operation.ResponseTransform = &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "set", Path: "/normalized", Value: &manifest.Template{Kind: "literal", Value: true}}}}
						provider, err = c.buildOpenaiProvider(false, &config.Options{}, providerregistry.Registration{ProviderID: "fixture", Operation: operation}, server.URL, "synthetic-provider-key", headers, providertransport.TemplateValues{}, validate)
					case "gemini-native", "gemini-operation":
						provider, err = gemini.NewProviderWithProjectSource(server.URL, func() string { return "synthetic-provider-key" }, headers, operation, validate, func(context.Context, string) string { return "synthetic-project" })
					}
					require.NoError(t, err)
					model, err := provider.LanguageModel(t.Context(), "fixture")
					require.NoError(t, err)
					result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("captured prompt")}})
					if mode == "relative" || mode == "allowed-port" {
						require.NoError(t, err)
						require.Contains(t, fmt.Sprint(result.Content), "redirect accepted")
						if mode == "relative" {
							require.EqualValues(t, 1, sameOrigin.Load())
							require.Zero(t, otherOrigin.Load())
						} else {
							require.EqualValues(t, 1, otherOrigin.Load())
						}
					} else {
						require.Error(t, err)
						if mode == "other-port" || mode == "other-host" {
							require.ErrorContains(t, err, "provider redirect refused")
						}
						if mode == "owner-changed" {
							require.ErrorContains(t, err, "synthetic owner changed")
						}
						require.Zero(t, sameOrigin.Load())
						require.Zero(t, otherOrigin.Load(), "no selected credential, prompt body or custom header may reach an undeclared destination")
					}
					require.EqualValues(t, 1, starts.Load(), "a refused redirect must not replay the original operation")
				})
			}
		}
	}
}
