package providertransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func imageLiteral(value any) manifest.ImageValue {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return manifest.ImageValue{Literal: data}
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
	ownerErr := errors.New("owner replaced")
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
	var workflowErr *ImageWorkflowError
	require.False(t, errors.As(err, &workflowErr))
	require.Zero(t, requests.Load())
	require.EqualValues(t, 2, checks.Load())
}

func TestImageFramesAreBoundedAndExact(t *testing.T) {
	payload := `[["record","example",[47]]]`
	framed := fmt.Sprintf("prefix\n%d\n%s\n", len(payload), payload)
	frames, err := decodeImageFrames([]byte(framed), "prefix")
	require.NoError(t, err)
	require.Len(t, frames, 1)
	for _, invalid := range []string{"prefix", "prefix\n999\n[]", "wrong\n2\n[]", "prefix\n3\n[!]", "prefix\n5\n[] {}"} {
		_, err := decodeImageFrames([]byte(invalid), "prefix")
		require.Error(t, err)
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
