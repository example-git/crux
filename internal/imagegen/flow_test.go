package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	flowTestProject = "test-project"
	flowTestSiteKey = "test-site-key"
)

func TestFlowGenerateUsesDirectFlowAndCachesBrowserSession(t *testing.T) {
	var imports atomic.Int64
	var bootstraps atomic.Int64
	var projects atomic.Int64
	var models atomic.Int64
	var generations atomic.Int64
	var challenges atomic.Int64
	var mu sync.Mutex
	var payloads [][]any
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/app":
			bootstraps.Add(1)
			if cookie, err := request.Cookie("__Secure-1PSID"); err != nil || cookie.Value != "browser-session" {
				http.Error(writer, "missing session", http.StatusUnauthorized)
				return
			}
			writeFlowBootstrap(writer, "token")
		case "/recaptcha.js":
			_, _ = io.WriteString(writer, `/recaptcha/releases/test-version/recaptcha__en.js`)
		case "/anchor":
			if action := request.URL.Query().Get("sa"); action != flowImageGenerationAction {
				http.Error(writer, "wrong action", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
		case "/reload":
			challenges.Add(1)
			if err := request.ParseForm(); err != nil || request.FormValue("sa") != flowImageGenerationAction {
				http.Error(writer, "wrong action", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
		case "/rpc":
			rpcID := request.URL.Query().Get("rpcids")
			switch rpcID {
			case flowGetProjectsRPC:
				projects.Add(1)
				writeFlowRPCResponse(writer, rpcID, flowProjectsPayload(flowTestProject))
			case flowGetModelsRPC:
				models.Add(1)
				payload, err := decodeFlowRPCRequest(request)
				if err != nil || len(payload) != 0 || request.URL.Query().Get("source-path") != "/project/"+flowTestProject {
					http.Error(writer, "wrong project scope", http.StatusBadRequest)
					return
				}
				writeFlowRPCResponse(writer, rpcID, flowModelsPayload("GEM_PIX_2", "NARWHAL"))
			case flowGenerateRPC:
				index := generations.Add(1)
				payload, err := decodeFlowRPCRequest(request)
				if err != nil {
					http.Error(writer, "bad request", http.StatusBadRequest)
					return
				}
				mu.Lock()
				payloads = append(payloads, payload)
				mu.Unlock()
				mediaURL := fmt.Sprintf("%s/media/%d", server.URL, index)
				usage := "GEM_PIX_2"
				if index == 3 {
					usage = "NARWHAL"
				}
				writeFlowRPCResponse(writer, rpcID, flowGeneratedPayload(fmt.Sprintf("media-%d", index), flowTestProject, mediaURL, usage))
			default:
				http.Error(writer, "unexpected RPC", http.StatusBadRequest)
			}
		case "/media/1", "/media/2", "/media/3":
			writer.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(writer, "generated-image")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverURL, _ := url.Parse(server.URL)
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "__Secure-1PSID", Value: "browser-session", Path: "/"}})
	client := newFlowTestClient(t, server, jar)
	client.flowCookieJars = func(context.Context) ([]http.CookieJar, error) {
		imports.Add(1)
		return []http.CookieJar{jar}, nil
	}
	client.authResolver = func(context.Context) (resolvedAuth, error) {
		t.Fatal("Google Flow request called the configured credential resolver")
		return resolvedAuth{}, nil
	}

	response, err := client.Generate(t.Context(), GenerateRequest{
		Backend: BackendFlow,
		Prompt:  "a paper fox",
		Model:   flowModelNanoBananaPro,
		N:       2,
		Size:    "800x600",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.AuthMode != AuthFlow || response.Model != flowProductBananaPro || len(response.Data) != 2 {
		t.Fatalf("response = %+v", response)
	}
	for _, image := range response.Data {
		decoded, decodeErr := base64.StdEncoding.DecodeString(image.B64JSON)
		if decodeErr != nil || string(decoded) != "generated-image" {
			t.Fatalf("decoded image = %q, %v", decoded, decodeErr)
		}
	}
	second, err := client.Generate(t.Context(), GenerateRequest{
		Backend: BackendFlow,
		Prompt:  "second",
		Model:   flowModelNanoBanana2,
		N:       1,
		Size:    "auto",
	})
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if second.Model != flowProductBanana2 {
		t.Fatalf("second model = %q", second.Model)
	}
	if imports.Load() != 1 || bootstraps.Load() != 1 || projects.Load() != 1 || models.Load() != 1 || generations.Load() != 3 || challenges.Load() != 3 {
		t.Fatalf("imports=%d bootstraps=%d projects=%d models=%d generations=%d challenges=%d", imports.Load(), bootstraps.Load(), projects.Load(), models.Load(), generations.Load(), challenges.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 3 {
		t.Fatalf("payload count = %d", len(payloads))
	}
	for index, payload := range payloads {
		usage := "GEM_PIX_2"
		aspect := 5
		prompt := "a paper fox"
		if index == 2 {
			usage = "NARWHAL"
			aspect = 0
			prompt = "second"
		}
		assertFlowGenerationPayload(t, payload, prompt, usage, aspect, nil)
	}
}

func TestFlowFallsBackToBanana2AfterProGenerationRPCFailure(t *testing.T) {
	var generations atomic.Int64
	var challenges atomic.Int64
	var downloads atomic.Int64
	var payloadsMu sync.Mutex
	payloads := make([][]any, 4)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/app":
			writeFlowBootstrap(writer, "token")
		case "/recaptcha.js":
			_, _ = io.WriteString(writer, `/recaptcha/releases/test-version/recaptcha__en.js`)
		case "/anchor":
			_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
		case "/reload":
			challenges.Add(1)
			_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
		case "/rpc":
			rpcID := request.URL.Query().Get("rpcids")
			switch rpcID {
			case flowGetProjectsRPC:
				writeFlowRPCResponse(writer, rpcID, flowProjectsPayload(flowTestProject))
			case flowGetModelsRPC:
				writeFlowRPCResponse(writer, rpcID, flowModelsPayload("GEM_PIX_2", "NARWHAL"))
			case flowGenerateRPC:
				generation := int(generations.Add(1))
				payload, err := decodeFlowRPCRequest(request)
				if err != nil || generation > len(payloads) {
					http.Error(writer, "bad request", http.StatusBadRequest)
					return
				}
				payloadsMu.Lock()
				payloads[generation-1] = payload
				payloadsMu.Unlock()
				if generation <= 2 {
					http.Error(writer, "pro exhausted", http.StatusTooManyRequests)
					return
				}
				writeFlowRPCResponse(writer, rpcID, flowGeneratedPayload(fmt.Sprintf("media-%d", generation), flowTestProject, fmt.Sprintf("%s/media/%d", server.URL, generation), flowUsageBanana2))
			default:
				http.Error(writer, "unexpected RPC", http.StatusBadRequest)
			}
		case "/media/3", "/media/4":
			downloads.Add(1)
			writer.Header().Set("Content-Type", "image/jpeg")
			_, _ = io.WriteString(writer, "generated-image")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := newFlowTestClient(t, server, jar)
	response, err := client.Generate(t.Context(), GenerateRequest{
		Backend: BackendFlow,
		Prompt:  "fallback",
		Model:   flowModelNanoBananaPro,
		N:       2,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Model != flowProductBanana2 || len(response.Data) != 2 || len(response.Failures) != 0 {
		t.Fatalf("response = %+v", response)
	}
	if generations.Load() != 4 || challenges.Load() != 4 || downloads.Load() != 2 {
		t.Fatalf("generations=%d challenges=%d downloads=%d", generations.Load(), challenges.Load(), downloads.Load())
	}
	payloadsMu.Lock()
	defer payloadsMu.Unlock()
	for index, payload := range payloads {
		usage := flowUsageBananaPro
		if index >= 2 {
			usage = flowUsageBanana2
		}
		assertFlowGenerationPayload(t, payload, "fallback", usage, 0, nil)
	}
}

func TestFlowFallsBackToBanana2WhenProIsUnavailable(t *testing.T) {
	var payload []any
	var generations atomic.Int64
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/anchor":
			_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
		case "/reload":
			_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
		case "/rpc":
			if request.URL.Query().Get("rpcids") != flowGenerateRPC {
				http.Error(writer, "unexpected RPC", http.StatusBadRequest)
				return
			}
			generations.Add(1)
			var err error
			payload, err = decodeFlowRPCRequest(request)
			if err != nil {
				http.Error(writer, "bad request", http.StatusBadRequest)
				return
			}
			writeFlowRPCResponse(writer, flowGenerateRPC, flowGeneratedPayload("media", flowTestProject, server.URL+"/media", flowUsageBanana2))
		case "/media":
			writer.Header().Set("Content-Type", "image/jpeg")
			_, _ = io.WriteString(writer, "generated-image")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.flow = flowTestSession(server)
	client.flow.availableUsage = map[string]bool{flowUsageBanana2: true}
	response, err := client.Generate(t.Context(), GenerateRequest{Backend: BackendFlow, Prompt: "fallback", N: 1})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Model != flowProductBanana2 || len(response.Data) != 1 || generations.Load() != 1 {
		t.Fatalf("response=%+v generations=%d", response, generations.Load())
	}
	assertFlowGenerationPayload(t, payload, "fallback", flowUsageBanana2, 0, nil)
}

func TestFlowFallbackRequiresOnlyProGenerationRPCFailures(t *testing.T) {
	pro, err := selectFlowModel(flowModelNanoBananaPro)
	if err != nil {
		t.Fatal(err)
	}
	banana2, err := selectFlowModel(flowModelNanoBanana2)
	if err != nil {
		t.Fatal(err)
	}
	available := map[string]bool{flowUsageBananaPro: true, flowUsageBanana2: true}
	generationError := fmt.Errorf("%w: exhausted", errFlowGenerationRPC)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name      string
		ctx       context.Context
		selected  flowModelPreset
		available map[string]bool
		outputs   [][]byte
		errors    []error
		want      bool
	}{
		{name: "all generation RPC failures", ctx: t.Context(), selected: pro, available: available, outputs: [][]byte{nil, nil}, errors: []error{generationError, generationError}, want: true},
		{name: "partial success", ctx: t.Context(), selected: pro, available: available, outputs: [][]byte{[]byte("image"), nil}, errors: []error{nil, generationError}},
		{name: "mixed failure phase", ctx: t.Context(), selected: pro, available: available, outputs: [][]byte{nil, nil}, errors: []error{generationError, errors.New("download failed")}},
		{name: "fallback unavailable", ctx: t.Context(), selected: pro, available: map[string]bool{flowUsageBananaPro: true}, outputs: [][]byte{nil}, errors: []error{generationError}},
		{name: "already banana 2", ctx: t.Context(), selected: banana2, available: available, outputs: [][]byte{nil}, errors: []error{generationError}},
		{name: "canceled", ctx: canceled, selected: pro, available: available, outputs: [][]byte{nil}, errors: []error{generationError}},
		{name: "missing error", ctx: t.Context(), selected: pro, available: available, outputs: [][]byte{nil}, errors: []error{nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldFallbackFlowGeneration(test.ctx, test.selected, test.available, test.outputs, test.errors); got != test.want {
				t.Fatalf("shouldFallbackFlowGeneration() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestFlowGenerateReturnsSuccessfulVariantsWhenOneFails(t *testing.T) {
	var generations atomic.Int64
	var downloads atomic.Int64
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/app":
			writeFlowBootstrap(writer, "token")
		case "/recaptcha.js":
			_, _ = io.WriteString(writer, `/recaptcha/releases/test-version/recaptcha__en.js`)
		case "/anchor":
			_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
		case "/reload":
			_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
		case "/rpc":
			rpcID := request.URL.Query().Get("rpcids")
			switch rpcID {
			case flowGetProjectsRPC:
				writeFlowRPCResponse(writer, rpcID, flowProjectsPayload(flowTestProject))
			case flowGetModelsRPC:
				writeFlowRPCResponse(writer, rpcID, flowModelsPayload("GEM_PIX_2", "NARWHAL"))
			case flowGenerateRPC:
				generation := generations.Add(1)
				if generation == 2 {
					http.Error(writer, "failed variant", http.StatusBadGateway)
					return
				}
				writeFlowRPCResponse(writer, rpcID, flowGeneratedPayload(fmt.Sprintf("media-%d", generation), flowTestProject, fmt.Sprintf("%s/media/%d", server.URL, generation), flowUsageBananaPro))
			default:
				http.Error(writer, "unexpected RPC", http.StatusBadRequest)
			}
		case "/media/1", "/media/3":
			downloads.Add(1)
			writer.Header().Set("Content-Type", "image/jpeg")
			_, _ = io.WriteString(writer, "generated-image")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := newFlowTestClient(t, server, jar)
	response, err := client.Generate(t.Context(), GenerateRequest{
		Backend: BackendFlow,
		Prompt:  "three variants",
		N:       3,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if generations.Load() != 3 || downloads.Load() != 2 {
		t.Fatalf("generations=%d downloads=%d", generations.Load(), downloads.Load())
	}
	if len(response.Data) != 2 || len(response.Failures) != 1 {
		t.Fatalf("response = %+v", response)
	}
	seen := make(map[int]bool, 3)
	for _, image := range response.Data {
		seen[image.Variant] = true
	}
	for _, failure := range response.Failures {
		seen[failure.Variant] = true
		if !strings.Contains(failure.Error, "status 502") {
			t.Fatalf("failure = %+v", failure)
		}
	}
	if !seen[1] || !seen[2] || !seen[3] {
		t.Fatalf("variant identities = %+v", seen)
	}
}

func TestFlowRebootstrapsAfterFailureWithoutReplayingJob(t *testing.T) {
	var imports atomic.Int64
	var bootstraps atomic.Int64
	var generations atomic.Int64
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/app":
			writeFlowBootstrap(writer, fmt.Sprintf("token-%d", bootstraps.Add(1)))
		case "/recaptcha.js":
			_, _ = io.WriteString(writer, `/recaptcha/releases/test-version/recaptcha__en.js`)
		case "/anchor":
			_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
		case "/reload":
			_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
		case "/rpc":
			rpcID := request.URL.Query().Get("rpcids")
			if rpcID == flowGetProjectsRPC {
				writeFlowRPCResponse(writer, rpcID, flowProjectsPayload(flowTestProject))
				return
			}
			if rpcID == flowGetModelsRPC {
				writeFlowRPCResponse(writer, rpcID, flowModelsPayload(flowUsageBananaPro))
				return
			}
			if rpcID != flowGenerateRPC {
				http.Error(writer, "unexpected RPC", http.StatusBadRequest)
				return
			}
			generation := generations.Add(1)
			if generation == 2 {
				http.Error(writer, "expired", http.StatusUnauthorized)
				return
			}
			wantToken := "token-1"
			if generation == 3 {
				wantToken = "token-2"
			}
			if request.FormValue("at") != wantToken {
				http.Error(writer, "stale", http.StatusUnauthorized)
				return
			}
			writeFlowRPCResponse(writer, rpcID, flowGeneratedPayload(fmt.Sprintf("media-%d", generation), flowTestProject, server.URL+"/media", flowUsageBananaPro))
		case "/media":
			writer.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(writer, "generated-image")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := newFlowTestClient(t, server, jar)
	client.flowCookieJars = func(context.Context) ([]http.CookieJar, error) {
		imports.Add(1)
		return []http.CookieJar{jar}, nil
	}
	request := GenerateRequest{Backend: BackendFlow, Prompt: "recover", N: 1}
	if _, err := client.Generate(t.Context(), request); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if _, err := client.Generate(t.Context(), request); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("failed Generate error = %v", err)
	}
	if _, err := client.Generate(t.Context(), request); err != nil {
		t.Fatalf("recovered Generate: %v", err)
	}
	if imports.Load() != 2 || bootstraps.Load() != 2 || generations.Load() != 3 {
		t.Fatalf("imports=%d bootstraps=%d generations=%d", imports.Load(), bootstraps.Load(), generations.Load())
	}
}

func TestFlowEditPersistsAndReusesVerifiedFlowUpload(t *testing.T) {
	var uploads atomic.Int64
	var mediaChecks atomic.Int64
	var generations atomic.Int64
	var uploadPayload []any
	var generationReferences [][]string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/app":
			writeFlowBootstrap(writer, "token")
		case "/recaptcha.js":
			_, _ = io.WriteString(writer, `/recaptcha/releases/test-version/recaptcha__en.js`)
		case "/anchor":
			_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
		case "/reload":
			_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
		case "/rpc":
			rpcID := request.URL.Query().Get("rpcids")
			payload, err := decodeFlowRPCRequest(request)
			if err != nil {
				http.Error(writer, "bad request", http.StatusBadRequest)
				return
			}
			switch rpcID {
			case flowGetProjectsRPC:
				writeFlowRPCResponse(writer, rpcID, flowProjectsPayload(flowTestProject))
			case flowGetModelsRPC:
				writeFlowRPCResponse(writer, rpcID, flowModelsPayload(flowUsageBananaPro))
			case flowUploadRPC:
				uploads.Add(1)
				uploadPayload = payload
				writeFlowRPCResponse(writer, rpcID, []any{flowTestMedia("uploaded-media", flowTestProject, "", 0)})
			case flowGetMediaRPC:
				mediaChecks.Add(1)
				if len(payload) != 1 || payload[0] != "uploaded-media" {
					http.Error(writer, "wrong media", http.StatusBadRequest)
					return
				}
				writeFlowRPCResponse(writer, rpcID, flowTestMedia("uploaded-media", flowTestProject, "", 0))
			case flowGenerateRPC:
				index := generations.Add(1)
				generationReferences = append(generationReferences, flowReferenceIDs(t, payload))
				writeFlowRPCResponse(writer, rpcID, flowGeneratedPayload(fmt.Sprintf("generated-%d", index), flowTestProject, server.URL+"/media", flowUsageBananaPro))
			default:
				http.Error(writer, "unexpected RPC", http.StatusBadRequest)
			}
		case "/media":
			writer.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(writer, "edited-image")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := newFlowTestClient(t, server, jar)
	registryPath := filepath.Join(t.TempDir(), "flow-uploads.json")
	client.flowUploadRegistryPath = registryPath
	request := EditRequest{
		Backend: BackendFlow,
		Images:  []EditImage{{Filename: "input.png", MIMEType: "image/png", Data: []byte("input-image")}},
		Prompt:  "make it blue",
		N:       1,
	}
	if _, err := client.Edit(t.Context(), request); err != nil {
		t.Fatalf("first Edit: %v", err)
	}
	if _, err := client.Edit(t.Context(), request); err != nil {
		t.Fatalf("second Edit: %v", err)
	}
	if uploads.Load() != 1 || mediaChecks.Load() != 1 || generations.Load() != 2 {
		t.Fatalf("uploads=%d media checks=%d generations=%d", uploads.Load(), mediaChecks.Load(), generations.Load())
	}
	if len(uploadPayload) != 12 || uploadPayload[2] != "image/png" || uploadPayload[3] != float64(1) || uploadPayload[7] != nil || uploadPayload[8] != "input.png" {
		t.Fatalf("upload payload = %#v", uploadPayload)
	}
	encoded, _ := uploadPayload[1].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != "input-image" {
		t.Fatalf("upload bytes = %q, %v", decoded, err)
	}
	for _, references := range generationReferences {
		if len(references) != 1 || references[0] != "uploaded-media" {
			t.Fatalf("generation references = %#v", references)
		}
	}
	content, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, forbidden := range []string{"input-image", encoded, "challenge-token", server.URL} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("registry contains forbidden request material")
		}
	}
	if !strings.Contains(text, `"sha256"`) || !strings.Contains(text, `"media_id": "uploaded-media"`) {
		t.Fatalf("registry omitted safe reuse metadata: %s", text)
	}
	info, err := os.Stat(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatal("registry is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("registry permissions = %o", info.Mode().Perm())
	}
}

func TestFlowReplacesStaleCachedUploadWithoutSubstitution(t *testing.T) {
	registryPath := filepath.Join(t.TempDir(), "flow-uploads.json")
	input, err := prepareFlowUpload(EditImage{Filename: "input.png", MIMEType: "image/png", Data: []byte("same-image")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFlowUploadRegistry(registryPath, flowUploadRegistry{
		Version: flowUploadRegistryVersion,
		Entries: []flowUploadRegistryEntry{{ProjectScope: flowTestProject, SHA256: input.hash, Label: input.label, MediaType: input.mediaType, Size: input.size, MediaID: "stale-media"}},
	}); err != nil {
		t.Fatal(err)
	}
	var checks atomic.Int64
	var uploads atomic.Int64
	var generatedReference string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/media" {
			writer.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(writer, "edited")
			return
		}
		if request.URL.Path != "/rpc" {
			if request.URL.Path == "/anchor" {
				_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
				return
			}
			if request.URL.Path == "/reload" {
				_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
				return
			}
			http.NotFound(writer, request)
			return
		}
		rpcID := request.URL.Query().Get("rpcids")
		payload, decodeErr := decodeFlowRPCRequest(request)
		if decodeErr != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		switch rpcID {
		case flowGetMediaRPC:
			checks.Add(1)
			writeFlowRPCResponse(writer, rpcID, flowTestMedia("stale-media", flowTestProject, "", 4))
		case flowUploadRPC:
			uploads.Add(1)
			writeFlowRPCResponse(writer, rpcID, []any{flowTestMedia("replacement-media", flowTestProject, "", flowMediaStatusSucceeded)})
		case flowGenerateRPC:
			references := flowReferenceIDs(t, payload)
			if len(references) == 1 {
				generatedReference = references[0]
			}
			writeFlowRPCResponse(writer, rpcID, flowGeneratedPayload("generated-media", flowTestProject, server.URL+"/media", flowUsageBananaPro))
		default:
			http.Error(writer, "unexpected RPC", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	session := flowTestSession(server)
	session.uploadRegistryPath = registryPath
	client := NewClient()
	client.flow = session
	if _, err := client.Edit(t.Context(), EditRequest{
		Backend: BackendFlow,
		Images:  []EditImage{{Filename: "input.png", MIMEType: "image/png", Data: []byte("same-image")}},
		Prompt:  "edit",
		N:       1,
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if checks.Load() != 1 || uploads.Load() != 1 || generatedReference != "replacement-media" {
		t.Fatalf("checks=%d uploads=%d reference=%q", checks.Load(), uploads.Load(), generatedReference)
	}
	registry, err := loadFlowUploadRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Entries) != 1 || registry.Entries[0].MediaID != "replacement-media" {
		t.Fatalf("registry = %+v", registry)
	}
}

func TestFlowRejectsCorruptUploadRegistryBeforeNetwork(t *testing.T) {
	registryPath := filepath.Join(t.TempDir(), "flow-uploads.json")
	if err := os.WriteFile(registryPath, []byte(`{"version":1,"entries":[],"secret":"unexpected"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	client := NewClient()
	client.flow = &flowSession{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("unexpected request")
		})},
		project:            flowTestProject,
		availableUsage:     map[string]bool{flowUsageBananaPro: true},
		uploadRegistryPath: registryPath,
		maxBytes:           defaultMaxResponseBytes,
	}
	_, err := client.Edit(t.Context(), EditRequest{
		Backend: BackendFlow,
		Images:  []EditImage{{Filename: "input.png", MIMEType: "image/png", Data: []byte("input")}},
		Prompt:  "edit",
		N:       1,
	})
	if err == nil || !strings.Contains(err.Error(), "registry is invalid") {
		t.Fatalf("Edit error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestFlowRejectsUnavailableExplicitModelBeforeGeneration(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch request.URL.Path {
		case "/app":
			writeFlowBootstrap(writer, "token")
		case "/recaptcha.js":
			_, _ = io.WriteString(writer, `/recaptcha/releases/test-version/recaptcha__en.js`)
		case "/rpc":
			rpcID := request.URL.Query().Get("rpcids")
			switch rpcID {
			case flowGetProjectsRPC:
				writeFlowRPCResponse(writer, rpcID, flowProjectsPayload(flowTestProject))
			case flowGetModelsRPC:
				writeFlowRPCResponse(writer, rpcID, flowModelsPayload(flowUsageBananaPro))
			default:
				http.Error(writer, "generation must not run", http.StatusBadRequest)
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := newFlowTestClient(t, server, jar)
	_, err := client.Generate(t.Context(), GenerateRequest{Backend: BackendFlow, Prompt: "test", Model: flowModelNanoBanana2, N: 1})
	if err == nil || !strings.Contains(err.Error(), "is unavailable") {
		t.Fatalf("Generate error = %v", err)
	}
	if requests.Load() != 4 {
		t.Fatalf("requests = %d, want bootstrap, challenge script, projects, models", requests.Load())
	}
}

func TestFlowReportsSafeBootstrapFailureWithoutResponseValues(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/app" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, `{"xZbWve":"site-key","private":"never-expose-this"}`)
	}))
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := newFlowTestClient(t, server, jar)
	_, err := client.Generate(t.Context(), GenerateRequest{Backend: BackendFlow, Prompt: "test", N: 1})
	if err == nil || !strings.Contains(err.Error(), "browser session is not authenticated") {
		t.Fatalf("Generate error = %v", err)
	}
	if strings.Contains(err.Error(), "never-expose-this") {
		t.Fatalf("Generate error exposed bootstrap content: %v", err)
	}
}

func TestFlowRejectsUnsupportedExplicitOptionsBeforeAuthentication(t *testing.T) {
	var imports atomic.Int64
	client := NewClient()
	client.flowCookieJars = func(context.Context) ([]http.CookieJar, error) {
		imports.Add(1)
		return nil, errors.New("must not be called")
	}
	for _, request := range []GenerateRequest{
		{Backend: "unknown", Prompt: "test", N: 1},
		{Backend: "gemini-web", Prompt: "test", N: 1},
		{Backend: BackendFlow, Prompt: "test", Model: "gemini-3-pro-image", N: 1},
		{Backend: BackendFlow, Prompt: "test", N: 1, Quality: QualityHigh},
		{Backend: BackendFlow, Prompt: "test", N: 1, Background: BackgroundTransparent},
		{Backend: BackendFlow, Prompt: "test", N: 1, Size: "800x500"},
		{Backend: BackendAuto, Prompt: "test", N: 1, Size: "800x600"},
	} {
		if _, err := client.Generate(t.Context(), request); err == nil {
			t.Fatalf("Generate(%+v) succeeded", request)
		}
	}
	if imports.Load() != 0 {
		t.Fatalf("browser imports = %d", imports.Load())
	}
}

func TestFlowGenerationDoesNotReplayFailedDirectRPC(t *testing.T) {
	var generations atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/anchor":
			_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
		case "/reload":
			_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
		case "/rpc":
			generations.Add(1)
			http.Error(writer, "failed", http.StatusServiceUnavailable)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := NewClient()
	client.flow = flowTestSession(server)
	_, err := client.Generate(t.Context(), GenerateRequest{Backend: BackendFlow, Prompt: "one request", Model: flowModelNanoBanana2, N: 1})
	if err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("Generate error = %v", err)
	}
	if generations.Load() != 1 {
		t.Fatalf("generation requests = %d", generations.Load())
	}
}

func TestFlowDownloadRejectsUntrustedRedirectBeforeDispatch(t *testing.T) {
	var requests atomic.Int64
	session := &flowSession{
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://attacker.example/image"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		})},
		endpoints: defaultFlowEndpoints(),
		maxBytes:  defaultMaxResponseBytes,
	}
	_, err := session.downloadImage(t.Context(), "https://lh3.googleusercontent.com/generated")
	if !errors.Is(err, errUntrustedFlowMediaRedirect) {
		t.Fatalf("downloadImage error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("transport requests = %d, want 1", requests.Load())
	}
}

func TestFlowMediaParserUsesOnlyExactGeneratedImageSlot(t *testing.T) {
	wanted := "https://flow-content.google/generated"
	payload := flowGeneratedPayload("generated-media", flowTestProject, wanted, flowUsageBananaPro)
	results, _ := testSliceAt(payload, 0)
	generated, _ := results[0].([]any)
	image, _ := testSliceAt(generated, 6)
	detail, _ := testSliceAt(image, 0)
	image[0] = append(detail, "https://attacker.example/not-generated")
	preset, err := selectFlowModel(flowModelNanoBananaPro)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseFlowGeneratedMedia(payload, flowTestProject, preset)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.url != wanted || parsed.id != "generated-media" || parsed.project != flowTestProject || !parsed.image || parsed.status != flowMediaStatusSucceeded {
		t.Fatalf("parsed media = %+v", parsed)
	}
	if !trustedFlowMediaURL(wanted) || trustedFlowMediaURL("https://flow-content.google.attacker.example/image") {
		t.Fatal("trusted media host validation failed")
	}
	wrongPreset, err := selectFlowModel(flowModelNanoBanana2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlowGeneratedMedia(payload, flowTestProject, wrongPreset); err == nil {
		t.Fatal("mismatched generated image model was accepted")
	}
}

func TestFlowLimitsJobsGlobally(t *testing.T) {
	var active atomic.Int64
	var maximum atomic.Int64
	var requests atomic.Int64
	entered := make(chan int64, 2)
	release := make(chan struct{})
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/anchor":
			_, _ = io.WriteString(writer, `<input id="recaptcha-token" value="anchor-token">`)
		case "/reload":
			_, _ = io.WriteString(writer, `["rresp","challenge-token"]`)
		case "/rpc":
			index := requests.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			entered <- index
			if index == 1 {
				<-release
			}
			writeFlowRPCResponse(writer, flowGenerateRPC, flowGeneratedPayload(fmt.Sprintf("media-%d", index), flowTestProject, server.URL+"/media", flowUsageBananaPro))
		case "/media":
			writer.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(writer, "generated")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	newClient := func() *Client {
		client := NewClient()
		client.flow = flowTestSession(server)
		return client
	}
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() {
		_, err := newClient().Generate(t.Context(), GenerateRequest{Backend: BackendFlow, Prompt: "first", N: 1})
		firstResult <- err
	}()
	if first := <-entered; first != 1 {
		t.Fatalf("first request = %d", first)
	}
	go func() {
		_, err := newClient().Generate(t.Context(), GenerateRequest{Backend: BackendFlow, Prompt: "second", N: 1})
		secondResult <- err
	}()
	select {
	case second := <-entered:
		close(release)
		<-firstResult
		<-secondResult
		t.Fatalf("second job dispatched concurrently as request %d", second)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if second := <-entered; second != 2 {
		t.Fatalf("second request = %d", second)
	}
	if err := <-secondResult; err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent requests = %d", maximum.Load())
	}
}

func newFlowTestClient(t *testing.T, server *httptest.Server, jar http.CookieJar) *Client {
	t.Helper()
	client := NewClient()
	client.HTTPClient = server.Client()
	client.flowCookieJars = func(context.Context) ([]http.CookieJar, error) { return []http.CookieJar{jar}, nil }
	endpoints := flowTestEndpoints(server.URL)
	client.flowEndpoints = &endpoints
	client.flowUploadRegistryPath = filepath.Join(t.TempDir(), "flow-uploads.json")
	return client
}

func flowTestSession(server *httptest.Server) *flowSession {
	return &flowSession{
		httpClient:       server.Client(),
		endpoints:        flowTestEndpoints(server.URL),
		at:               "token",
		language:         "en",
		siteKey:          flowTestSiteKey,
		recaptchaVersion: "test-version",
		project:          flowTestProject,
		availableUsage:   map[string]bool{flowUsageBananaPro: true, flowUsageBanana2: true},
		maxBytes:         defaultMaxResponseBytes,
	}
}

func flowTestEndpoints(baseURL string) flowEndpoints {
	return flowEndpoints{
		app:                 baseURL + "/app",
		rpc:                 baseURL + "/rpc",
		recaptchaScript:     baseURL + "/recaptcha.js",
		recaptchaAnchor:     baseURL + "/anchor",
		recaptchaReload:     baseURL + "/reload",
		origin:              baseURL,
		recaptchaOrigin:     baseURL,
		allowUntrustedMedia: true,
	}
}

func writeFlowBootstrap(writer http.ResponseWriter, token string) {
	_, _ = fmt.Fprintf(writer, `{"SNlM0e":"%s","cfb2h":"build","FdrFJe":"session","TuX5cc":"en","xZbWve":"%s"}`, token, flowTestSiteKey)
}

func writeFlowRPCResponse(writer http.ResponseWriter, rpcID string, payload []any) {
	payloadJSON, _ := json.Marshal(payload)
	frame, _ := json.Marshal([]any{[]any{"wrb.fr", rpcID, string(payloadJSON)}})
	_, _ = fmt.Fprintf(writer, ")]}'\n%d\n%s\n", len(frame), frame)
}

func decodeFlowRPCRequest(request *http.Request) ([]any, error) {
	if err := request.ParseForm(); err != nil {
		return nil, err
	}
	var envelope []any
	if err := json.Unmarshal([]byte(request.FormValue("f.req")), &envelope); err != nil {
		return nil, err
	}
	first, ok := testSliceAt(envelope, 0)
	if !ok {
		return nil, errors.New("missing request batch")
	}
	call, ok := testSliceAt(first, 0)
	if !ok || len(call) < 2 {
		return nil, errors.New("missing request call")
	}
	encoded, ok := call[1].(string)
	if !ok {
		return nil, errors.New("missing request payload")
	}
	var payload []any
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func flowProjectsPayload(project string) []any {
	return []any{[]any{[]any{project}}}
}

func flowModelsPayload(usages ...string) []any {
	usageValues := make([]any, len(usages))
	for index, usage := range usages {
		usageValues[index] = []any{usage}
	}
	family := []any{"Image", usageValues, nil, 25}
	config := make([]any, 6)
	config[5] = []any{family}
	return []any{config}
}

func flowGeneratedPayload(id, project, mediaURL, usage string) []any {
	workflowID := "workflow-" + id
	detail := make([]any, 15)
	detail[8] = map[string]int{"GEM_PIX": 23, "GEM_PIX_2": 25, "NARWHAL": 29}[usage]
	detail[11] = workflowID
	detail[13] = mediaURL
	image := make([]any, 3)
	image[0] = detail
	image[2] = []any{768, 1376}
	generated := make([]any, 7)
	generated[0] = id
	generated[2] = workflowID
	generated[6] = image
	workflowState := make([]any, 5)
	workflowState[4] = id
	workflow := make([]any, 5)
	workflow[0] = workflowID
	workflow[3] = workflowState
	workflow[4] = project
	return []any{[]any{generated}, []any{workflow}}
}

func flowTestMedia(id, project, mediaURL string, status int) []any {
	alternate := make([]any, 5)
	if mediaURL != "" {
		alternate[3] = mediaURL
	}
	image := make([]any, 3)
	image[1] = alternate
	media := make([]any, 7)
	media[0] = id
	media[1] = project
	media[6] = image
	if status != 0 {
		metadata := make([]any, 9)
		metadata[8] = []any{status}
		media[5] = metadata
	}
	return media
}

func assertFlowGenerationPayload(t *testing.T, payload []any, prompt, usage string, aspect int, references []string) {
	t.Helper()
	if len(payload) != 5 || payload[2] != float64(1) {
		t.Fatalf("outer payload = %#v", payload)
	}
	contextValue, ok := testSliceAt(payload, 3)
	if !ok || len(contextValue) != 11 || contextValue[1] != float64(22) || contextValue[4] != nil || contextValue[5] != flowTestProject {
		t.Fatalf("project context = %#v", contextValue)
	}
	challenge, ok := testSliceAt(contextValue, 10)
	if !ok || len(challenge) != 2 || challenge[0] != "challenge-token" || challenge[1] != float64(1) {
		t.Fatalf("challenge = %#v", challenge)
	}
	requests, ok := testSliceAt(payload, 1)
	if !ok || len(requests) != 1 {
		t.Fatalf("generated requests = %#v", requests)
	}
	generated, ok := requests[0].([]any)
	if !ok || len(generated) != 14 || generated[5] != usage {
		t.Fatalf("generated request = %#v", requests[0])
	}
	if aspect == 0 {
		if generated[4] != nil {
			t.Fatalf("auto aspect = %#v", generated[4])
		}
	} else if generated[4] != float64(aspect) {
		t.Fatalf("aspect = %#v", generated[4])
	}
	structured, ok := testSliceAt(generated, 8)
	if !ok {
		t.Fatalf("structured prompt = %#v", generated[8])
	}
	parts, ok := testSliceAt(structured, 0)
	if !ok || len(parts) != 1 {
		t.Fatalf("prompt parts = %#v", structured)
	}
	part, ok := testSliceAt(parts, 0)
	if !ok || len(part) != 1 || part[0] != prompt {
		t.Fatalf("prompt part = %#v", parts)
	}
	if got := flowReferenceIDs(t, payload); len(got) != len(references) {
		t.Fatalf("references = %#v", got)
	} else {
		for index := range got {
			if got[index] != references[index] {
				t.Fatalf("references = %#v", got)
			}
		}
	}
}

func flowReferenceIDs(t *testing.T, payload []any) []string {
	t.Helper()
	requests, ok := testSliceAt(payload, 1)
	if !ok || len(requests) != 1 {
		t.Fatalf("generated requests = %#v", requests)
	}
	generated, ok := requests[0].([]any)
	if !ok {
		t.Fatalf("generated request = %#v", requests[0])
	}
	if generated[2] == nil {
		return nil
	}
	references, ok := testSliceAt(generated, 2)
	if !ok {
		t.Fatalf("reference list = %#v", generated)
	}
	ids := make([]string, len(references))
	for index, value := range references {
		reference, referenceOK := value.([]any)
		if !referenceOK || len(reference) != 5 || reference[4] != float64(1) {
			t.Fatalf("reference = %#v", value)
		}
		ids[index], _ = reference[0].(string)
	}
	return ids
}

func testSliceAt(value []any, index int) ([]any, bool) {
	if index < 0 || index >= len(value) {
		return nil, false
	}
	nested, ok := value[index].([]any)
	return nested, ok
}
