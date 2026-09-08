package providertransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func imageLiteral(value any) manifest.ImageValue {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return manifest.ImageValue{Literal: data}
}

func TestImageBoundaryErrorExplainsFailure(t *testing.T) {
	redact.Register("synthetic-boundary-diagnostic-secret")
	for _, test := range []struct {
		name  string
		cause error
		want  string
	}{
		{name: "no cause", want: "image plugin execution boundary rejected"},
		{name: "reason", cause: errors.New("image redirect limit exceeded"), want: "image plugin execution boundary rejected: image redirect limit exceeded"},
		{name: "secret", cause: errors.New("credential changed: synthetic-boundary-diagnostic-secret"), want: "image plugin execution boundary rejected: credential changed: " + redact.Replacement},
		{name: "bounded", cause: errors.New(strings.Repeat("界", MaxMappedErrorTextRunes+20)), want: "image plugin execution boundary rejected: " + strings.Repeat("界", MaxMappedErrorTextRunes)},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &imageBoundaryError{cause: test.cause}
			require.Equal(t, test.want, err.Error())
			require.Equal(t, test.cause, errors.Unwrap(err))
		})
	}
}

func TestImageWorkflowRedirectBoundaryExplainsFailure(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "https://outside.example.test/image", http.StatusFound)
	}))
	defer server.Close()
	request := manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: "binary", Phase: "download", MaxBytes: 1024, TimeoutSeconds: 5}
	host := ImageWorkflowHost{Client: server.Client(), ValidateOwner: func() error { return nil }, Manifest: manifest.ImageManifest{Origins: []manifest.ImageOrigin{{URL: server.URL}}, Limits: manifest.ImageLimits{ResponseBytes: 1024}, Workflows: map[string]manifest.ImageWorkflow{"read": {Steps: []manifest.ImageStep{{ID: "send", Request: &request}}, Result: manifest.ImageValue{Ref: "/steps/send/body"}}}}}
	_, err := host.Execute(t.Context(), "read", nil)
	require.ErrorContains(t, err, "image plugin execution boundary rejected: image redirect origin is not permitted by the plugin")
	var boundary *imageBoundaryError
	require.ErrorAs(t, err, &boundary)
	require.EqualValues(t, 1, requests.Load())
}

func TestImageWorkflowRejectedResponseExplainsFailure(t *testing.T) {
	redact.Register("synthetic-image-error-secret")
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"json", `{"error":{"message":"The requested size is unsupported","code":"invalid_size","param":"size"},"access_token":"synthetic-image-error-secret"}`, "The requested size is unsupported (code: invalid_size) (param: size)"},
		{"detail", `{"detail":"Generation quota exhausted"}`, "Generation quota exhausted"},
		{"plain", "Request rejected: synthetic-image-error-secret", "Request rejected: [REDACTED]"},
		{"empty", "", "without an error message"},
		{"bounded", strings.Repeat("x", MaxMappedErrorTextRunes+100), strings.Repeat("x", 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			request := manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: "json", Phase: "generation", MaxBytes: 8192, TimeoutSeconds: 5}
			host := ImageWorkflowHost{Client: server.Client(), ValidateOwner: func() error { return nil }, Manifest: manifest.ImageManifest{Origins: []manifest.ImageOrigin{{URL: server.URL}}, Limits: manifest.ImageLimits{ResponseBytes: 8192}, Workflows: map[string]manifest.ImageWorkflow{"read": {Steps: []manifest.ImageStep{{ID: "send", Request: &request}}, Result: manifest.ImageValue{Ref: "/steps/send/body"}}}}}
			_, err := host.Execute(t.Context(), "read", nil)
			require.ErrorContains(t, err, "HTTP 400")
			require.ErrorContains(t, err, test.want)
			require.NotContains(t, err.Error(), "synthetic-image-error-secret")
			var failure *ImageWorkflowError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, "generation", failure.Phase)
			require.Equal(t, http.StatusBadRequest, failure.Status)
			require.LessOrEqual(t, len([]rune(failure.Cause.Error())), MaxMappedErrorTextRunes)
		})
	}
}

func TestImageWorkflowRedactsRotatedCookies(t *testing.T) {
	for _, delivery := range []string{"error", "success", "retry", "redirect"} {
		for _, format := range []string{"json", "plain"} {
			t.Run(delivery+"/"+format, func(t *testing.T) {
				secret := "synthetic-image-rotated-" + delivery + "-" + format
				var calls atomic.Int64
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					call := calls.Add(1)
					cookie, err := r.Cookie("session")
					if err != nil {
						t.Error(err)
						http.Error(w, "missing session cookie", http.StatusUnauthorized)
						return
					}
					if call == 1 {
						if cookie.Value != "synthetic-initial-session" {
							t.Error("initial session cookie was changed")
						}
						http.SetCookie(w, &http.Cookie{Name: "session", Value: secret, Path: "/", Secure: true, HttpOnly: true})
						switch delivery {
						case "success":
							_, _ = io.WriteString(w, "ready")
							return
						case "retry":
							http.Error(w, "try again", http.StatusTooManyRequests)
							return
						case "redirect":
							http.Redirect(w, r, "/generation", http.StatusFound)
							return
						}
					} else if cookie.Value != secret {
						t.Error("rotated session cookie was not reused")
					}
					w.WriteHeader(http.StatusBadRequest)
					if format == "json" {
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "Session rejected: " + secret, "code": "invalid_session", "param": "session"}})
					} else {
						_, _ = io.WriteString(w, "Session rejected: "+secret)
					}
				}))
				defer server.Close()
				address, err := url.Parse(server.URL)
				require.NoError(t, err)
				jar, err := cookiejar.New(nil)
				require.NoError(t, err)
				jar.SetCookies(address, []*http.Cookie{{Name: "session", Value: "synthetic-initial-session", Path: "/", Secure: true}})
				request := manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: "text", Phase: "generation", MaxBytes: 8192, TimeoutSeconds: 5}
				if delivery == "retry" {
					request.Retry = &manifest.ImageRetry{Attempts: 2, Statuses: []int{http.StatusTooManyRequests}}
				}
				host := ImageWorkflowHost{Client: server.Client(), CookieJars: map[string]http.CookieJar{"browser": jar}, ValidateOwner: func() error { return nil }, Manifest: manifest.ImageManifest{
					Credentials: []manifest.ImageCredential{{ID: "browser", Source: "browser", Domains: []string{address.Hostname()}}},
					Origins:     []manifest.ImageOrigin{{URL: server.URL, Credentials: []string{"browser"}}},
					Limits:      manifest.ImageLimits{ResponseBytes: 8192},
					Workflows:   map[string]manifest.ImageWorkflow{"read": {Steps: []manifest.ImageStep{{ID: "send", Request: &request}}, Result: manifest.ImageValue{Ref: "/steps/send/body"}}},
				}}
				if delivery == "success" {
					result, err := host.Execute(t.Context(), "read", nil)
					require.NoError(t, err)
					require.Equal(t, "ready", ImageWorkflowValue(result))
				}
				_, err = host.Execute(t.Context(), "read", nil)
				require.ErrorContains(t, err, "HTTP 400")
				require.NotContains(t, err.Error(), secret)
				var failure *ImageWorkflowError
				require.ErrorAs(t, err, &failure)
				want := "Session rejected: " + redact.Replacement
				if format == "json" {
					want += " (code: invalid_session) (param: session)"
				}
				require.Equal(t, want, failure.Cause.Error())
				require.Equal(t, "generation", failure.Phase)
				require.Equal(t, http.StatusBadRequest, failure.Status)
				stored := jar.Cookies(address)
				require.Len(t, stored, 1)
				require.Equal(t, secret, stored[0].Value)
				wantCalls := int64(2)
				if delivery == "error" {
					wantCalls = 1
				}
				require.Equal(t, wantCalls, calls.Load())
			})
		}
	}
}

func TestImageWorkflowErrorLocationsDoNotExposeDeclarations(t *testing.T) {
	value := manifest.ImageValue{Op: "coalesce", Args: []manifest.ImageValue{imageLiteral(nil)}}
	host := ImageWorkflowHost{ValidateOwner: func() error { return nil }, Manifest: manifest.ImageManifest{Workflows: map[string]manifest.ImageWorkflow{
		"private-workflow": {Steps: []manifest.ImageStep{{ID: "private-step", Value: &value}}},
	}}}
	_, err := host.Execute(t.Context(), "private-workflow", nil)
	require.ErrorContains(t, err, "depth 0 step 1")
	require.ErrorContains(t, err, "no available value")
	require.NotContains(t, err.Error(), "private-workflow")
	require.NotContains(t, err.Error(), "private-step")
}

func TestImageWorkflowBudgetsSpanCalls(t *testing.T) {
	for _, kind := range []string{"requests", "steps", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, "12345678")
			}))
			defer server.Close()
			limits := manifest.ImageLimits{ResponseBytes: 1024}
			switch kind {
			case "requests":
				limits.Requests = 1
			case "steps":
				limits.Steps = 1
			case "bytes":
				limits.TotalResponseBytes = 12
			}
			request := manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: "text", Phase: "generation", MaxBytes: 1024, TimeoutSeconds: 5}
			host := ImageWorkflowHost{Client: server.Client(), ValidateOwner: func() error { return nil }, Manifest: manifest.ImageManifest{Origins: []manifest.ImageOrigin{{URL: server.URL}}, Limits: limits, Workflows: map[string]manifest.ImageWorkflow{"read": {Steps: []manifest.ImageStep{{ID: "send", Request: &request}}, Result: manifest.ImageValue{Ref: "/steps/send/body"}}}}}
			_, err := host.Execute(t.Context(), "read", nil)
			require.NoError(t, err)
			_, err = host.Execute(t.Context(), "read", nil)
			var workflowErr *ImageWorkflowError
			require.ErrorAs(t, err, &workflowErr)
			require.ErrorContains(t, workflowErr, "budget")
			count := calls.Load()
			_, err = host.Execute(t.Context(), "read", nil)
			require.ErrorContains(t, err, "budget")
			require.Equal(t, count, calls.Load())
			if kind == "bytes" {
				require.EqualValues(t, 2, count)
			} else {
				require.EqualValues(t, 1, count)
			}
		})
	}
}

func TestImageWorkflowEnforcesResponseMediaTypes(t *testing.T) {
	for _, test := range []struct {
		media  string
		status int
		phase  string
	}{
		{"Image/PNG; example=value", 200, ""},
		{"text/html", 200, "validation"},
		{"", 200, "validation"},
		{"image/png; malformed", 200, "validation"},
		{"text/html", 500, "generation"},
	} {
		t.Run(fmt.Sprintf("%s-%d", test.media, test.status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header()["Content-Type"] = []string{test.media}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte("synthetic image bytes"))
			}))
			defer server.Close()
			request := manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: "binary", Phase: "generation", MaxBytes: 1024, TimeoutSeconds: 5, AcceptedMediaTypes: []string{"image/png"}}
			host := ImageWorkflowHost{Client: server.Client(), ValidateOwner: func() error { return nil }, Manifest: manifest.ImageManifest{Origins: []manifest.ImageOrigin{{URL: server.URL}}, Limits: manifest.ImageLimits{ResponseBytes: 1024}, Workflows: map[string]manifest.ImageWorkflow{"read": {Steps: []manifest.ImageStep{{ID: "send", Request: &request}}, Result: manifest.ImageValue{Ref: "/steps/send/body"}}}}}
			result, err := host.Execute(t.Context(), "read", nil)
			if test.phase == "" {
				require.NoError(t, err)
				require.Equal(t, []byte("synthetic image bytes"), result)
			} else {
				var workflowErr *ImageWorkflowError
				require.ErrorAs(t, err, &workflowErr)
				require.Equal(t, test.phase, workflowErr.Phase)
			}
		})
	}
}

func TestImageWorkflowUsesDeclaredRequestAndResponse(t *testing.T) {
	var calls atomic.Int64
	var revoked atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/render" || r.URL.Query().Get("variant") != "3" || r.Header.Get("X-Example") != "synthetic" {
			http.Error(w, "unexpected request", 400)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["description"] != "paper bird" || body["model"] != float64(47) {
			http.Error(w, "unexpected body", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"result":{"images":["aW1hZ2U="],"model":47}}`)
	}))
	defer server.Close()
	request := manifest.ImageRequest{Method: "POST", URL: imageLiteral(server.URL + "/render"), Headers: map[string]manifest.ImageValue{"X-Example": imageLiteral("synthetic")}, Query: map[string]manifest.ImageValue{"variant": imageLiteral(3)}, Encoding: "json", Body: &manifest.ImageValue{Object: map[string]manifest.ImageValue{"description": {Ref: "/request/prompt"}, "model": {Ref: "/model/code"}}}, Response: "json", Phase: "generation", MaxBytes: 4096, TimeoutSeconds: 5}
	host := ImageWorkflowHost{Client: server.Client(), ValidateOwner: func() error {
		if revoked.Load() {
			return errors.New("owner revoked")
		}
		return nil
	}, Manifest: manifest.ImageManifest{Origins: []manifest.ImageOrigin{{URL: server.URL}}, Limits: manifest.ImageLimits{ResponseBytes: 4096}, Workflows: map[string]manifest.ImageWorkflow{"render": {
		Steps: []manifest.ImageStep{{ID: "send", Request: &request}, {ID: "check", Assert: &manifest.ImageValue{Op: "equal", Args: []manifest.ImageValue{{Ref: "/steps/send/body/result/model"}, imageLiteral(47)}}}}, Result: manifest.ImageValue{Ref: "/steps/send/body/result/images"},
	}}}}
	values := map[string]any{"request": map[string]any{"prompt": "paper bird"}, "model": map[string]any{"code": 47}}
	result, err := host.Execute(t.Context(), "render", values)
	require.NoError(t, err)
	require.Equal(t, []any{"aW1hZ2U="}, result)
	require.EqualValues(t, 1, calls.Load())
	revoked.Store(true)
	_, err = host.Execute(t.Context(), "render", values)
	require.ErrorContains(t, err, "owner revoked")
	require.EqualValues(t, 1, calls.Load())
}

func TestImageWorkflowRejectsOwnerAtTransportBoundary(t *testing.T) {
	var checks atomic.Int64
	var requests atomic.Int64
	redact.Register("synthetic-image-boundary-secret")
	ownerErr := errors.New("owner replaced: synthetic-image-boundary-secret")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	host := ImageWorkflowHost{Client: server.Client(), ValidateOwner: func() error {
		if checks.Add(1) > 1 {
			return ownerErr
		}
		return nil
	}, Manifest: manifest.ImageManifest{Origins: []manifest.ImageOrigin{{URL: server.URL}}, Limits: manifest.ImageLimits{ResponseBytes: 1024}, Workflows: map[string]manifest.ImageWorkflow{"read": {Steps: []manifest.ImageStep{{ID: "send", Request: &manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: "text", Phase: "setup", MaxBytes: 1024, TimeoutSeconds: 5}}}, Result: manifest.ImageValue{Ref: "/steps/send/body"}}}}}
	_, err := host.Execute(context.Background(), "read", nil)
	require.ErrorIs(t, err, ownerErr)
	require.ErrorContains(t, err, "image plugin execution boundary rejected: owner replaced: "+redact.Replacement)
	require.NotContains(t, err.Error(), "synthetic-image-boundary-secret")
	var workflowErr *ImageWorkflowError
	require.False(t, errors.As(err, &workflowErr))
	require.Zero(t, requests.Load())
	require.EqualValues(t, 2, checks.Load())
}

func TestImageFramesAreBoundedAndExact(t *testing.T) {
	payload := `[["record","日本語",[47]]]`
	framed := fmt.Sprintf("prefix\n%d\n%s\n", len(payload), payload)
	frames, err := decodeImageFrames([]byte(framed), "prefix")
	require.NoError(t, err)
	require.Len(t, frames, 1)
	for _, invalid := range []string{"prefix", "prefix\n999\n[]", "wrong\n2\n[]", "prefix\n3\n[!]", "prefix\n5\n[] {}", "prefix\n10\n[] garbage"} {
		_, err := decodeImageFrames([]byte(invalid), "prefix")
		require.Error(t, err)
	}
	_, err = decodeImageFrames([]byte("prefix\n"+strings.Repeat("2\n[]\n", 4097)), "prefix")
	require.ErrorContains(t, err, "frame count exceeds limit")
}

func TestImageLineFrames(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(fmt.Sprintf("newline=%q", newline), func(t *testing.T) {
			framed := strings.Join([]string{"prefix", "", "24678", `[["record","日本語 🎨",[47]]]`, "27", `[["e",4]]`}, newline)
			frames, err := decodeImageLineFrames([]byte(framed), "prefix")
			require.NoError(t, err)
			require.Equal(t, []any{[]any{[]any{"record", "日本語 🎨", []any{json.Number("47")}}}, []any{[]any{"e", json.Number("4")}}}, frames)
			_, err = decodeImageFrames([]byte(framed), "prefix")
			require.Error(t, err)
		})
	}
	for _, invalid := range []string{"prefix", "prefix\n999\n", "wrong\n2\n[]", "prefix\n3\n[!]", "prefix\n5\n[] {}", "prefix\n10\n[] garbage", "prefix\n2\n[", "prefix\ninvalid\n[]"} {
		_, err := decodeImageLineFrames([]byte(invalid), "prefix")
		require.Error(t, err)
	}
	payload := strings.Repeat("日本語 🎨", 20000)
	encoded, err := json.Marshal([]string{payload})
	require.NoError(t, err)
	frames, err := decodeImageLineFrames(append([]byte("prefix\n1\n"), encoded...), "prefix")
	require.NoError(t, err)
	require.Equal(t, []any{[]any{payload}}, frames)
	_, err = decodeImageLineFrames([]byte("prefix\n"+strings.Repeat("2\n[]\n", 4097)), "prefix")
	require.ErrorContains(t, err, "frame count exceeds limit")
}

func TestImageWorkflowUsesDeclaredResponseMode(t *testing.T) {
	payload := `[["record","日本語 🎨",[47]]]`
	for _, test := range []struct {
		name     string
		response string
		prefix   string
		body     string
		want     any
		invalid  bool
	}{
		{name: "json", response: "json", body: `{"images":["synthetic"]}`, want: map[string]any{"images": []any{"synthetic"}}},
		{name: "binary", response: "binary", body: "synthetic image bytes", want: []byte("synthetic image bytes")},
		{name: "text", response: "text", body: "synthetic text", want: "synthetic text"},
		{name: "byte framing", response: "framed-json", prefix: "prefix", body: fmt.Sprintf("prefix\n%d\n%s\n", len(payload), payload), want: []any{[]any{[]any{"record", "日本語 🎨", []any{json.Number("47")}}}}},
		{name: "line framing", response: "line-framed-json", prefix: "prefix", body: "prefix\n24678\n" + payload + "\n27\n[[\"e\",4]]\n", want: []any{[]any{[]any{"record", "日本語 🎨", []any{json.Number("47")}}}, []any{[]any{"e", json.Number("4")}}}},
		{name: "no implicit line fallback", response: "framed-json", prefix: "prefix", body: "prefix\n24678\n" + payload + "\n", invalid: true},
		{name: "json rejects framing", response: "json", body: "prefix\n24678\n" + payload + "\n", invalid: true},
		{name: "json rejects trailing values", response: "json", body: `{} {}`, invalid: true},
		{name: "lines reject trailing values", response: "line-framed-json", prefix: "prefix", body: "prefix\n5\n[] {}\n", invalid: true},
		{name: "unknown mode", response: "unknown", body: `[]`, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			request := manifest.ImageRequest{Method: "GET", URL: imageLiteral(server.URL), Encoding: "none", Response: test.response, FramePrefix: test.prefix, Phase: "setup", MaxBytes: 4096, TimeoutSeconds: 5}
			host := ImageWorkflowHost{Client: server.Client(), ValidateOwner: func() error { return nil }, Manifest: manifest.ImageManifest{Origins: []manifest.ImageOrigin{{URL: server.URL}}, Limits: manifest.ImageLimits{ResponseBytes: 4096}, Workflows: map[string]manifest.ImageWorkflow{"read": {Steps: []manifest.ImageStep{{ID: "send", Request: &request}}, Result: manifest.ImageValue{Ref: "/steps/send/body"}}}}}
			result, err := host.Execute(t.Context(), "read", nil)
			if test.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.want, result)
			}
		})
	}
}

func TestImageExpressionsRejectMalformedFallbackCandidates(t *testing.T) {
	for _, input := range []string{`{} {}`, `{} ]`, `[] garbage`} {
		value := manifest.ImageValue{Op: "parse-json", Args: []manifest.ImageValue{imageLiteral(input)}}
		_, err := EvaluateImageValue(value, nil)
		require.Error(t, err)
		_, err = EvaluateImageValue(manifest.ImageValue{Op: "coalesce", Args: []manifest.ImageValue{value, imageLiteral("fallback")}}, nil)
		require.Error(t, err)
		_, err = EvaluateImageValue(manifest.ImageValue{Literal: json.RawMessage(input)}, nil)
		require.Error(t, err)
	}
	result, err := EvaluateImageValue(manifest.ImageValue{Op: "coalesce", Args: []manifest.ImageValue{{Ref: "/missing"}, imageLiteral(nil), imageLiteral(""), imageLiteral("fallback")}}, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "fallback", result)
	_, err = EvaluateImageValue(manifest.ImageValue{Op: "coalesce", Args: []manifest.ImageValue{{Op: "get", Args: []manifest.ImageValue{imageLiteral([]any{1}), imageLiteral("/+0")}}, imageLiteral("fallback")}}, nil)
	require.Error(t, err)
}

func TestImageExpressionsOperateOnLiteralWireValues(t *testing.T) {
	value := manifest.ImageValue{Op: "map", Args: []manifest.ImageValue{imageLiteral([]any{[]any{"model-one", 47}, []any{"model-two", 51}}), {Op: "get", Args: []manifest.ImageValue{{Ref: "/item"}, imageLiteral("/0")}}}}
	result, err := EvaluateImageValue(value, nil)
	require.NoError(t, err)
	require.Equal(t, []any{"model-one", "model-two"}, result)
	_, err = EvaluateImageValue(manifest.ImageValue{Op: "random", Args: []manifest.ImageValue{imageLiteral(0)}}, nil)
	require.Error(t, err)
	_, err = EvaluateImageValue(manifest.ImageValue{Op: "regexp", Args: []manifest.ImageValue{imageLiteral("private response"), imageLiteral("(absent)"), imageLiteral(1)}}, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private response")
}
