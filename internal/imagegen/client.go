// Package imagegen calls image generation and editing endpoints.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providerregistry"
)

// CodexBaseURL is the ChatGPT backend base used for Codex account requests,
// matching the base Codex CLI uses for its own image_gen extension.
const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// OpenAIBaseURL is the standard OpenAI API base used for OPENAI_API_KEY
// requests.
const OpenAIBaseURL = "https://api.openai.com/v1"

// accountProvider is the account-store key for stored Codex credentials.
const accountProvider = accounts.ProviderCodex

// openAIAPIKeyEnv is the environment variable checked when no Codex account
// is signed in.
const openAIAPIKeyEnv = "OPENAI_API_KEY"

// ErrNoCredentials is returned when neither a signed-in Codex account nor
// OPENAI_API_KEY is available.
var ErrNoCredentials = fmt.Errorf(
	"no usable signed-in Codex account and no %s set; run `crux login codex` or export %s",
	openAIAPIKeyEnv, openAIAPIKeyEnv,
)

// ErrResponseTooLarge is returned when the image API response exceeds the
// client's configured response bound.
var ErrResponseTooLarge = errors.New("image API response too large")

const (
	minImageCount           = 1
	maxImageCount           = 10
	defaultMaxResponseBytes = int64(512 << 20)
)

var sizePattern = regexp.MustCompile(`^([1-9][0-9]*)x([1-9][0-9]*)$`)

// AuthMode identifies which credential a request used.
type AuthMode int

// Supported AuthMode values.
const (
	AuthCodex AuthMode = iota
	AuthAPIKey
	AuthFlow
)

type Backend string

const (
	BackendAuto Backend = "auto"
	BackendFlow Backend = "flow"
)

// Background controls output transparency behavior.
type Background string

// Supported Background values.
const (
	BackgroundAuto        Background = "auto"
	BackgroundOpaque      Background = "opaque"
	BackgroundTransparent Background = "transparent"
)

// Quality controls output rendering quality.
type Quality string

// Supported Quality values.
const (
	QualityAuto   Quality = "auto"
	QualityLow    Quality = "low"
	QualityMedium Quality = "medium"
	QualityHigh   Quality = "high"
)

// GenerateRequest is a request to create a new image.
type GenerateRequest struct {
	Backend    Backend    `json:"-"`
	Prompt     string     `json:"prompt"`
	Model      string     `json:"model"`
	N          int        `json:"n,omitempty"`
	Quality    Quality    `json:"quality,omitempty"`
	Size       string     `json:"size,omitempty"`
	Background Background `json:"background,omitempty"`
}

// EditImage is a single edit input image.
type EditImage struct {
	// Filename is used for the multipart upload when authenticating with
	// OPENAI_API_KEY; it is cosmetic only.
	Filename string
	MIMEType string
	Data     []byte
}

// EditRequest is a request to edit one or more existing images. Neither
// supported auth path accepts a mask parameter here.
type EditRequest struct {
	Backend    Backend
	Images     []EditImage
	Prompt     string
	Model      string
	N          int
	Quality    Quality
	Size       string
	Background Background
}

// Response is the decoded Images API response.
type Response struct {
	Created    int64       `json:"created"`
	Data       []ImageData `json:"data"`
	Background string      `json:"background,omitempty"`
	Quality    string      `json:"quality,omitempty"`
	Size       string      `json:"size,omitempty"`

	// AuthMode reports which credential served the request.
	AuthMode AuthMode              `json:"-"`
	Model    string                `json:"-"`
	Failures []ImageVariantFailure `json:"-"`
}

// ImageData holds one generated or edited image, base64-encoded.
type ImageData struct {
	B64JSON string `json:"b64_json"`
	Variant int    `json:"-"`
}

type ImageVariantFailure struct {
	Variant int    `json:"variant"`
	Error   string `json:"error"`
}

// APIError is returned when the image endpoint responds with a non-2xx
// status.
type APIError struct {
	AuthMode   AuthMode
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	source := "Codex account"
	switch e.AuthMode {
	case AuthAPIKey:
		source = "OpenAI API key"
	case AuthFlow:
		source = "Google Flow browser session"
	}
	return fmt.Sprintf("image API error (%s): status %d: %s", source, e.StatusCode, e.Body)
}

// Client calls the image generation/editing endpoints.
type Client struct {
	HTTPClient   *http.Client
	authResolver func(context.Context) (resolvedAuth, error)

	flowMu                 sync.Mutex
	flow                   *flowSession
	flowCookieJars         func(context.Context) ([]http.CookieJar, error)
	flowEndpoints          *flowEndpoints
	flowUploadRegistryPath string

	// MaxResponseBytes bounds a response body. Zero uses the 512 MiB default,
	// which accommodates up to ten documented high-resolution variants while
	// still bounding memory use.
	MaxResponseBytes int64
}

// NewClient returns a Client with sane defaults.
func NewClient() *Client {
	return &Client{HTTPClient: &http.Client{Timeout: 5 * time.Minute}}
}

// Generate creates a new image from a prompt.
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (*Response, error) {
	if err := ValidateGenerateRequest(req); err != nil {
		return nil, err
	}
	if req.Backend == BackendFlow {
		return c.generateFlow(ctx, req.Prompt, req.Model, req.Size, req.N, nil)
	}
	auth, err := c.resolveCredential(ctx)
	if err != nil {
		return nil, err
	}
	if req.Model == "" {
		req.Model = defaultModelFor(auth.mode)
	}
	requested := req.N
	var response *Response
	if auth.mode == AuthCodex {
		req.N = 0
		response, err = doCodexImageRequests(ctx, requested, func(requestCtx context.Context) (*Response, error) {
			return c.doJSON(requestCtx, auth, "images/generations", req)
		})
	} else {
		response, err = c.doJSON(ctx, auth, "images/generations", req)
	}
	if response != nil {
		response.Model = req.Model
	}
	if err != nil {
		return response, err
	}
	return finalizeImageResponse(response, requested)
}

// Edit edits one or more existing images.
func (c *Client) Edit(ctx context.Context, req EditRequest) (*Response, error) {
	if err := ValidateEditRequest(req); err != nil {
		return nil, err
	}
	if req.Backend == BackendFlow {
		return c.generateFlow(ctx, req.Prompt, req.Model, req.Size, req.N, req.Images)
	}
	auth, err := c.resolveCredential(ctx)
	if err != nil {
		return nil, err
	}
	if req.Model == "" {
		req.Model = defaultModelFor(auth.mode)
	}

	var response *Response
	if auth.mode == AuthCodex {
		payload := codexEditPayload{
			Prompt:     req.Prompt,
			Model:      req.Model,
			Quality:    req.Quality,
			Size:       req.Size,
			Background: req.Background,
		}
		for _, image := range req.Images {
			payload.Images = append(payload.Images, imageURL{ImageURL: dataURL(image.MIMEType, image.Data)})
		}
		response, err = doCodexImageRequests(ctx, req.N, func(requestCtx context.Context) (*Response, error) {
			return c.doJSON(requestCtx, auth, "images/edits", payload)
		})
	} else {
		response, err = c.doEditMultipart(ctx, auth, req)
	}
	if response != nil {
		response.Model = req.Model
	}
	if err != nil {
		return response, err
	}
	return finalizeImageResponse(response, req.N)
}

func doCodexImageRequests(ctx context.Context, count int, request func(context.Context) (*Response, error)) (*Response, error) {
	responses := make([]*Response, count)
	errorsByVariant := make([]error, count)
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			response, err := request(ctx)
			switch {
			case err != nil:
				errorsByVariant[index] = err
			case response == nil:
				errorsByVariant[index] = errors.New("returned no response")
			case len(response.Data) != 1:
				errorsByVariant[index] = fmt.Errorf("returned %d images, expected 1", len(response.Data))
			default:
				responses[index] = response
			}
		})
	}
	group.Wait()

	combined := &Response{AuthMode: AuthCodex}
	for index, response := range responses {
		if response == nil {
			message := "image request failed"
			if errorsByVariant[index] != nil {
				message = errorsByVariant[index].Error()
			}
			combined.Failures = append(combined.Failures, ImageVariantFailure{Variant: index + 1, Error: message})
			continue
		}
		if len(combined.Data) == 0 {
			combined.Created = response.Created
			combined.Background = response.Background
			combined.Quality = response.Quality
			combined.Size = response.Size
			combined.AuthMode = response.AuthMode
		}
		image := response.Data[0]
		image.Variant = index + 1
		combined.Data = append(combined.Data, image)
	}
	return finalizeImageResponse(combined, count)
}

func finalizeImageResponse(response *Response, requested int) (*Response, error) {
	if response == nil {
		return nil, errors.New("image API returned no response")
	}
	if requested < 1 {
		return nil, fmt.Errorf("invalid requested image count %d", requested)
	}
	if len(response.Data) > requested {
		return nil, fmt.Errorf("image response contained %d images, expected at most %d", len(response.Data), requested)
	}

	seen := make([]bool, requested)
	failures := make([]ImageVariantFailure, 0, requested)
	for _, failure := range response.Failures {
		if failure.Variant < 1 || failure.Variant > requested {
			return nil, fmt.Errorf("image response reported invalid failed variant %d", failure.Variant)
		}
		if seen[failure.Variant-1] {
			return nil, fmt.Errorf("image response reported duplicate variant %d", failure.Variant)
		}
		seen[failure.Variant-1] = true
		if strings.TrimSpace(failure.Error) == "" {
			failure.Error = "image request failed"
		}
		failures = append(failures, failure)
	}
	for index := range response.Data {
		variant := response.Data[index].Variant
		if variant == 0 {
			for candidate := 1; candidate <= requested; candidate++ {
				if !seen[candidate-1] {
					variant = candidate
					break
				}
			}
		}
		if variant < 1 || variant > requested {
			return nil, fmt.Errorf("image response reported invalid successful variant %d", variant)
		}
		if seen[variant-1] {
			return nil, fmt.Errorf("image response reported duplicate variant %d", variant)
		}
		response.Data[index].Variant = variant
		seen[variant-1] = true
	}
	for index, present := range seen {
		if !present {
			failures = append(failures, ImageVariantFailure{Variant: index + 1, Error: "image API returned no image"})
		}
	}
	sort.SliceStable(response.Data, func(left, right int) bool {
		return response.Data[left].Variant < response.Data[right].Variant
	})
	sort.SliceStable(failures, func(left, right int) bool {
		return failures[left].Variant < failures[right].Variant
	})
	response.Failures = failures
	if len(response.Data) == 0 {
		return nil, imageVariantFailuresError(failures)
	}
	return response, nil
}

func imageVariantFailuresError(failures []ImageVariantFailure) error {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, fmt.Sprintf("variant %d: %s", failure.Variant, strings.TrimSpace(failure.Error)))
	}
	if len(parts) == 0 {
		return errors.New("no image variants completed")
	}
	return fmt.Errorf("no image variants completed: %s", strings.Join(parts, "; "))
}

// resolvedAuth is the credential selected for a request.
type resolvedAuth struct {
	mode           AuthMode
	token          string
	accountID      string
	baseURL        string
	owner          providerregistry.RegistrationOwner
	ownerValidator func(providerregistry.RegistrationOwner) error
}

func (a resolvedAuth) validateOwner() error {
	if a.owner.ProviderID == "" {
		if a.ownerValidator != nil {
			return errors.New("image credential owner is missing")
		}
		return nil
	}
	if a.ownerValidator == nil {
		return fmt.Errorf("image credential owner validator for provider %s is unavailable", a.owner.ProviderID)
	}
	return a.ownerValidator(a.owner)
}

type ownerValidatingTransport struct {
	transport http.RoundTripper
	auth      resolvedAuth
}

func (t ownerValidatingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.auth.validateOwner(); err != nil {
		return nil, fmt.Errorf("image credential owner changed before request: %w", err)
	}
	return t.transport.RoundTrip(request)
}

func (c *Client) resolveCredential(ctx context.Context) (resolvedAuth, error) {
	if c.authResolver != nil {
		return c.authResolver(ctx)
	}
	return resolveAuth(ctx)
}

// resolveAuth prefers a usable signed-in Codex account, refreshing its token
// through the shared account store when expired. It falls back to
// OPENAI_API_KEY when the account is absent, incomplete, or cannot refresh.
func resolveAuth(ctx context.Context) (resolvedAuth, error) {
	entry, accountErr := accounts.Active(ctx, accountProvider)
	if accountErr == nil && entry != nil && entry.AccessToken != "" {
		if entry.Expired() {
			if entry.RefreshToken == "" {
				accountErr = errors.New("signed-in Codex account is expired and has no refresh token")
			} else {
				entry, accountErr = accounts.EnsureFreshWithRefresher(ctx, accountProvider, entry, codex.RefreshToken)
			}
		}
		if accountErr == nil && entry != nil && entry.AccessToken != "" && !entry.Expired() {
			return resolvedAuth{
				mode:      AuthCodex,
				token:     entry.AccessToken,
				accountID: accountIDFor(ctx, entry.AccessToken),
			}, nil
		}
	}

	if key := strings.TrimSpace(os.Getenv(openAIAPIKeyEnv)); key != "" {
		return resolvedAuth{mode: AuthAPIKey, token: key}, nil
	}
	if accountErr != nil {
		return resolvedAuth{}, fmt.Errorf("resolve Codex account token: %w", accountErr)
	}
	return resolvedAuth{}, ErrNoCredentials
}

// accountIDFor resolves the ChatGPT account id paired with the given
// access token, preferring the stored entry's own value.
func accountIDFor(ctx context.Context, token string) string {
	entry, err := accounts.Active(ctx, accountProvider)
	if err == nil && entry != nil {
		if accountID := accountIDFromRaw(entry.Raw); accountID != "" {
			return accountID
		}
	}
	return codex.AccountID(token)
}

// imageURL is a single edit input image, encoded as a data URL, used only
// for Codex account requests.
type imageURL struct {
	ImageURL string `json:"image_url"`
}

// codexEditPayload is the Codex account edit request body.
type codexEditPayload struct {
	Images     []imageURL `json:"images"`
	Prompt     string     `json:"prompt"`
	Model      string     `json:"model"`
	N          int        `json:"n,omitempty"`
	Quality    Quality    `json:"quality,omitempty"`
	Size       string     `json:"size,omitempty"`
	Background Background `json:"background,omitempty"`
}

func (c *Client) doJSON(ctx context.Context, auth resolvedAuth, path string, payload any) (*Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode image request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURLFor(auth)+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build image request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuthHeaders(httpReq, auth)

	return c.send(httpReq, auth)
}

func (c *Client) doEditMultipart(ctx context.Context, auth resolvedAuth, req EditRequest) (*Response, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	writeField := func(name, value string) error {
		if value == "" {
			return nil
		}
		return writer.WriteField(name, value)
	}
	if err := writeField("prompt", req.Prompt); err != nil {
		return nil, fmt.Errorf("build edit request: %w", err)
	}
	if err := writeField("model", req.Model); err != nil {
		return nil, fmt.Errorf("build edit request: %w", err)
	}
	if req.N > 0 {
		if err := writeField("n", strconv.Itoa(req.N)); err != nil {
			return nil, fmt.Errorf("build edit request: %w", err)
		}
	}
	if err := writeField("quality", string(req.Quality)); err != nil {
		return nil, fmt.Errorf("build edit request: %w", err)
	}
	if err := writeField("size", req.Size); err != nil {
		return nil, fmt.Errorf("build edit request: %w", err)
	}
	if err := writeField("background", string(req.Background)); err != nil {
		return nil, fmt.Errorf("build edit request: %w", err)
	}
	for i, img := range req.Images {
		filename := img.Filename
		if filename == "" {
			filename = fmt.Sprintf("image-%d", i+1)
		}
		part, err := writer.CreateFormFile("image[]", filename)
		if err != nil {
			return nil, fmt.Errorf("build edit request: %w", err)
		}
		if _, err := part.Write(img.Data); err != nil {
			return nil, fmt.Errorf("build edit request: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("build edit request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURLFor(auth)+"/images/edits", &buf)
	if err != nil {
		return nil, fmt.Errorf("build image request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	applyAuthHeaders(httpReq, auth)

	return c.send(httpReq, auth)
}

func applyAuthHeaders(httpReq *http.Request, auth resolvedAuth) {
	httpReq.Header.Set("Authorization", "Bearer "+auth.token)
	if auth.mode == AuthCodex {
		if auth.accountID != "" {
			httpReq.Header.Set("ChatGPT-Account-ID", auth.accountID)
		}
		httpReq.Header.Set("User-Agent", useragent.Codex())
		httpReq.Header.Set("originator", useragent.CodexOriginator())
		httpReq.Header.Set("version", useragent.CodexVersion())
	}
}

func baseURLFor(auth resolvedAuth) string {
	if auth.mode == AuthCodex {
		if codexBaseURLOverride != "" {
			return codexBaseURLOverride
		}
		return CodexBaseURL
	}
	if openAIBaseURLOverride != "" {
		return openAIBaseURLOverride
	}
	if auth.baseURL != "" {
		return strings.TrimRight(auth.baseURL, "/")
	}
	return OpenAIBaseURL
}

// Test-only base URL overrides, set via SetBaseURLOverridesForTest.
var (
	codexBaseURLOverride  string
	openAIBaseURLOverride string
)

// Default models per auth mode. gpt-image-2 is Codex's own default and is
// only available through the ChatGPT-backend Codex endpoint; the public
// OpenAI API currently serves gpt-image-1.
const (
	defaultCodexModel  = "gpt-image-2"
	defaultAPIKeyModel = "gpt-image-1"
)

func defaultModelFor(mode AuthMode) string {
	if mode == AuthCodex {
		return defaultCodexModel
	}
	return defaultAPIKeyModel
}

// ValidateGenerateRequest rejects request values that are known to be invalid
// without consulting credentials or making an HTTP request.
func ValidateGenerateRequest(req GenerateRequest) error {
	if err := validateBackend(req.Backend); err != nil {
		return err
	}
	if err := validateRequest(req.Backend, req.Prompt, req.N, req.Quality, req.Size, req.Background); err != nil {
		return err
	}
	return validateFlowOptions(req.Backend, req.Model, req.Quality, req.Background)
}

// ValidateEditRequest rejects request values and image inputs that are known
// to be invalid without consulting credentials or making an HTTP request.
func ValidateEditRequest(req EditRequest) error {
	if err := validateBackend(req.Backend); err != nil {
		return err
	}
	if err := validateRequest(req.Backend, req.Prompt, req.N, req.Quality, req.Size, req.Background); err != nil {
		return err
	}
	if err := validateFlowOptions(req.Backend, req.Model, req.Quality, req.Background); err != nil {
		return err
	}
	if len(req.Images) == 0 {
		return errors.New("at least one input image is required")
	}
	for i, img := range req.Images {
		if len(img.Data) == 0 {
			return fmt.Errorf("input image %d is empty", i+1)
		}
	}
	return nil
}

func validateBackend(backend Backend) error {
	switch backend {
	case "", BackendAuto, BackendFlow:
		return nil
	default:
		return fmt.Errorf("unsupported image backend %q (use auto or flow)", backend)
	}
}

func validateFlowOptions(backend Backend, model string, quality Quality, background Background) error {
	if backend != BackendFlow {
		return nil
	}
	if _, err := selectFlowModel(model); err != nil {
		return err
	}
	if quality != "" && quality != QualityAuto {
		return fmt.Errorf("Google Flow does not support explicit quality %q; use auto or omit it", quality)
	}
	if background != "" && background != BackgroundAuto {
		return fmt.Errorf("Google Flow does not support explicit background %q; use auto or omit it", background)
	}
	return nil
}

func validateRequest(backend Backend, prompt string, n int, quality Quality, size string, background Background) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("prompt must not be empty")
	}
	if n < minImageCount || n > maxImageCount {
		return fmt.Errorf("n must be between %d and %d", minImageCount, maxImageCount)
	}
	switch quality {
	case "", QualityAuto, QualityLow, QualityMedium, QualityHigh:
	default:
		return fmt.Errorf("unsupported quality %q (use low, medium, high, or auto)", quality)
	}
	switch background {
	case "", BackgroundAuto, BackgroundOpaque, BackgroundTransparent:
	default:
		return fmt.Errorf("unsupported background %q (use transparent, opaque, or auto)", background)
	}
	if backend == BackendFlow {
		return validateFlowSize(size)
	}
	return validateSize(size)
}

func validateFlowSize(size string) error {
	_, err := flowAspectRatio(size)
	return err
}

func validateSize(size string) error {
	if size == "" || size == "auto" {
		return nil
	}
	matches := sizePattern.FindStringSubmatch(size)
	if matches == nil {
		return fmt.Errorf("unsupported size %q (use auto or WIDTHxHEIGHT)", size)
	}
	width, _ := strconv.ParseInt(matches[1], 10, 64)
	height, _ := strconv.ParseInt(matches[2], 10, 64)
	if width > 3840 || height > 3840 || width%16 != 0 || height%16 != 0 {
		return fmt.Errorf("unsupported size %q (edges must be multiples of 16 and at most 3840)", size)
	}
	short, long := min(width, height), max(width, height)
	if long > 3*short {
		return fmt.Errorf("unsupported size %q (aspect ratio must not exceed 3:1)", size)
	}
	pixels := width * height
	if pixels < 655_360 || pixels > 8_294_400 {
		return fmt.Errorf("unsupported size %q (pixel count must be between 655360 and 8294400)", size)
	}
	return nil
}

func (c *Client) send(httpReq *http.Request, auth resolvedAuth) (*Response, error) {
	mode := auth.mode
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	requestClient := *httpClient
	transport := requestClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	requestClient.Transport = ownerValidatingTransport{transport: transport, auth: auth}
	resp, err := requestClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send image request: %w", err)
	}
	defer resp.Body.Close()

	limit := c.MaxResponseBytes
	if limit == 0 {
		limit = defaultMaxResponseBytes
	}
	if limit < 0 || limit == math.MaxInt64 {
		return nil, fmt.Errorf("invalid image response limit %d", limit)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read image response: %w", err)
	}
	if int64(len(respBody)) > limit {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrResponseTooLarge, limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{AuthMode: mode, StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var out Response
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode image response: %w", err)
	}
	out.AuthMode = mode
	return &out, nil
}

func dataURL(mimeType string, data []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}
