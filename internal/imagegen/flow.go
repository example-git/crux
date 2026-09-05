package imagegen

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/cookieutil"

	"github.com/google/uuid"
)

const (
	flowAppURL                = "https://flow.google.com/"
	flowRPCURL                = "https://flow.google.com/_/AiSandboxAngularFrontend/data/batchexecute"
	flowRecaptchaScriptURL    = "https://www.google.com/recaptcha/enterprise.js"
	flowRecaptchaAnchorURL    = "https://www.google.com/recaptcha/enterprise/anchor"
	flowRecaptchaReloadURL    = "https://www.google.com/recaptcha/enterprise/reload"
	flowOrigin                = "https://flow.google.com"
	flowRecaptchaOrigin       = "https://flow.google.com:443"
	flowUserAgent             = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	flowEnvelopeLimit         = int64(16 << 20)
	flowModelNanoBanana2      = "nano-banana-2"
	flowModelNanoBananaPro    = "nano-banana-pro"
	flowProductBanana2        = "Nano Banana 2"
	flowProductBananaPro      = "Nano Banana Pro"
	flowUsageBanana2          = "NARWHAL"
	flowUsageBananaPro        = "GEM_PIX_2"
	flowGetProjectsRPC        = "UpteDb"
	flowGetModelsRPC          = "HTrJv"
	flowGenerateRPC           = "ogiZ0b"
	flowUploadRPC             = "maseQ"
	flowGetMediaRPC           = "as29s"
	flowImageGenerationAction = "IMAGE_GENERATION"
	flowUploadImageAction     = "UPLOAD_IMAGE"
	flowMediaStatusSucceeded  = 3
)

var (
	flowTokenPatterns = []*regexp.Regexp{
		regexp.MustCompile(`"SNlM0e":"([^"]+)"`),
		regexp.MustCompile(`\["SNlM0e","([^"]+)"\]`),
	}
	flowSiteKeyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`"xZbWve":"([^"]+)"`),
		regexp.MustCompile(`\["xZbWve","([^"]+)"\]`),
	}
	flowBuildPattern              = regexp.MustCompile(`"cfb2h":"([^"]+)"`)
	flowSessionPattern            = regexp.MustCompile(`"FdrFJe":"([^"]+)"`)
	flowLanguagePattern           = regexp.MustCompile(`"TuX5cc":"([^"]+)"`)
	flowRecaptchaRelease          = regexp.MustCompile(`/recaptcha/releases/([^/"']+)/`)
	flowRecaptchaAnchorToken      = regexp.MustCompile(`(?s)(?:id|name)="recaptcha-token"[^>]*value="([^"]+)"`)
	flowRecaptchaResponseToken    = regexp.MustCompile(`"rresp","([^"]+)"`)
	flowProjectIDPattern          = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	errUntrustedFlowMediaRedirect = errors.New("Google Flow redirected to an untrusted image URL")
	errFlowRPCNoPayload           = errors.New("Google Flow RPC returned no payload")
	errFlowGenerationRPC          = errors.New("Google Flow image generation RPC failed")
	errFlowMediaUnavailable       = errors.New("Google Flow upload is unavailable")
	flowJobLimit                  = make(chan struct{}, 1)
)

type flowEndpoints struct {
	app                 string
	rpc                 string
	recaptchaScript     string
	recaptchaAnchor     string
	recaptchaReload     string
	origin              string
	recaptchaOrigin     string
	allowUntrustedMedia bool
}

type flowSession struct {
	httpClient         *http.Client
	endpoints          flowEndpoints
	at                 string
	buildLabel         string
	sessionID          string
	language           string
	siteKey            string
	recaptchaVersion   string
	project            string
	availableUsage     map[string]bool
	uploadRegistryPath string
	maxBytes           int64
}

type flowModelPreset struct {
	id      string
	product string
	usage   string
}

type flowMedia struct {
	id            string
	project       string
	url           string
	image         bool
	status        int
	statusPresent bool
}

func defaultFlowEndpoints() flowEndpoints {
	return flowEndpoints{
		app:             flowAppURL,
		rpc:             flowRPCURL,
		recaptchaScript: flowRecaptchaScriptURL,
		recaptchaAnchor: flowRecaptchaAnchorURL,
		recaptchaReload: flowRecaptchaReloadURL,
		origin:          flowOrigin,
		recaptchaOrigin: flowRecaptchaOrigin,
	}
}

func (c *Client) generateFlow(ctx context.Context, prompt, model, size string, count int, images []EditImage) (*Response, error) {
	preset, err := selectFlowModel(model)
	if err != nil {
		return nil, err
	}
	aspect, err := flowAspectRatio(size)
	if err != nil {
		return nil, err
	}
	preparedImages := make([]flowUploadInput, len(images))
	for index, image := range images {
		preparedImages[index], err = prepareFlowUpload(image, index)
		if err != nil {
			return nil, err
		}
	}
	select {
	case flowJobLimit <- struct{}{}:
		defer func() { <-flowJobLimit }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	session, err := c.flowSession(ctx)
	if err != nil {
		return nil, err
	}
	completed := false
	defer func() {
		if !completed {
			c.invalidateFlowSession(session)
		}
	}()
	if !session.availableUsage[preset.usage] {
		fallback, ok := flowFallbackPreset(preset, session.availableUsage)
		if !ok {
			return nil, fmt.Errorf("Google Flow image model %q is unavailable", preset.id)
		}
		preset = fallback
	}
	references := make([]string, len(preparedImages))
	for index, image := range preparedImages {
		mediaID, resolveErr := session.resolveUpload(ctx, image)
		if resolveErr != nil {
			return nil, fmt.Errorf("Google Flow input image %d: %w", index+1, resolveErr)
		}
		references[index] = mediaID
	}
	outputs, errorsByVariant := session.generateVariants(ctx, strings.TrimSpace(prompt), preset, aspect, references, count)
	if shouldFallbackFlowGeneration(ctx, preset, session.availableUsage, outputs, errorsByVariant) {
		preset, _ = flowFallbackPreset(preset, session.availableUsage)
		outputs, errorsByVariant = session.generateVariants(ctx, strings.TrimSpace(prompt), preset, aspect, references, count)
	}

	response := &Response{
		Created:  time.Now().Unix(),
		AuthMode: AuthFlow,
		Model:    preset.product,
		Data:     make([]ImageData, 0, count),
		Failures: make([]ImageVariantFailure, 0, count),
	}
	var aggregateBytes int64
	for index, imageData := range outputs {
		if errorsByVariant[index] != nil {
			response.Failures = append(response.Failures, ImageVariantFailure{Variant: index + 1, Error: errorsByVariant[index].Error()})
			continue
		}
		if aggregateBytes+int64(len(imageData)) > session.maxBytes {
			response.Failures = append(response.Failures, ImageVariantFailure{
				Variant: index + 1,
				Error:   fmt.Sprintf("%v: aggregate limit is %d bytes", ErrResponseTooLarge, session.maxBytes),
			})
			continue
		}
		aggregateBytes += int64(len(imageData))
		response.Data = append(response.Data, ImageData{
			B64JSON: base64.StdEncoding.EncodeToString(imageData),
			Variant: index + 1,
		})
		outputs[index] = nil
	}
	response, err = finalizeImageResponse(response, count)
	if err != nil {
		return nil, err
	}
	completed = len(response.Failures) == 0
	return response, nil
}

func (s *flowSession) generateVariants(
	ctx context.Context,
	prompt string,
	preset flowModelPreset,
	aspect int,
	references []string,
	count int,
) ([][]byte, []error) {
	outputs := make([][]byte, count)
	errorsByVariant := make([]error, count)
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			imageData, err := s.generateOne(ctx, prompt, preset, aspect, references)
			if err != nil {
				errorsByVariant[index] = err
				return
			}
			outputs[index] = imageData
		})
	}
	group.Wait()
	return outputs, errorsByVariant
}

func flowFallbackPreset(selected flowModelPreset, available map[string]bool) (flowModelPreset, bool) {
	if selected.usage != flowUsageBananaPro || !available[flowUsageBanana2] {
		return flowModelPreset{}, false
	}
	return flowModelPreset{
		id:      flowModelNanoBanana2,
		product: flowProductBanana2,
		usage:   flowUsageBanana2,
	}, true
}

func shouldFallbackFlowGeneration(
	ctx context.Context,
	selected flowModelPreset,
	available map[string]bool,
	outputs [][]byte,
	errorsByVariant []error,
) bool {
	if ctx.Err() != nil || len(outputs) == 0 || len(outputs) != len(errorsByVariant) {
		return false
	}
	if _, ok := flowFallbackPreset(selected, available); !ok {
		return false
	}
	for index, err := range errorsByVariant {
		if len(outputs[index]) > 0 || err == nil || !errors.Is(err, errFlowGenerationRPC) {
			return false
		}
	}
	return true
}

func (c *Client) flowSession(ctx context.Context) (*flowSession, error) {
	c.flowMu.Lock()
	defer c.flowMu.Unlock()
	if c.flow != nil {
		return c.flow, nil
	}
	loadJars := c.flowCookieJars
	if loadJars == nil {
		loadJars = func(ctx context.Context) ([]http.CookieJar, error) {
			jars, err := cookieutil.ImportBrowserJars(ctx, []string{"google.com"})
			if err != nil {
				return nil, err
			}
			selected := make([]http.CookieJar, 0, len(jars))
			for _, jar := range jars {
				if jarHasGoogleSessionCookie(jar) {
					selected = append(selected, jar)
				}
			}
			return selected, nil
		}
	}
	jars, err := loadJars(ctx)
	if err != nil {
		return nil, fmt.Errorf("import Google Flow browser session: %w", err)
	}
	endpoints := defaultFlowEndpoints()
	if c.flowEndpoints != nil {
		endpoints = *c.flowEndpoints
	}
	limit := c.MaxResponseBytes
	if limit == 0 {
		limit = defaultMaxResponseBytes
	}
	if limit < 1 {
		return nil, fmt.Errorf("invalid image response limit %d", limit)
	}
	registryPath := c.flowUploadRegistryPath
	if registryPath == "" {
		registryPath = defaultFlowUploadRegistryPath()
	}
	var lastBootstrapErr error
	for _, jar := range jars {
		session, bootstrapErr := bootstrapFlow(ctx, c.HTTPClient, jar, endpoints, registryPath, limit)
		if bootstrapErr == nil {
			c.flow = session
			return session, nil
		}
		lastBootstrapErr = bootstrapErr
	}
	if lastBootstrapErr != nil {
		return nil, fmt.Errorf("no imported browser profile has a usable Google Flow session: %w", lastBootstrapErr)
	}
	return nil, errors.New("no imported browser profile has a usable Google Flow session")
}

func (c *Client) invalidateFlowSession(session *flowSession) {
	c.flowMu.Lock()
	defer c.flowMu.Unlock()
	if c.flow == session {
		c.flow = nil
	}
}

func bootstrapFlow(
	ctx context.Context,
	baseClient *http.Client,
	jar http.CookieJar,
	endpoints flowEndpoints,
	registryPath string,
	maxBytes int64,
) (*flowSession, error) {
	if jar == nil {
		return nil, errors.New("browser cookie jar is unavailable")
	}
	var client *http.Client
	if baseClient != nil {
		clone := *baseClient
		client = &clone
	} else {
		clone := *http.DefaultClient
		client = &clone
	}
	client.Jar = jar
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoints.app, nil)
	if err != nil {
		return nil, err
	}
	applyFlowHeaders(request, endpoints)
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("Google Flow bootstrap failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &APIError{AuthMode: AuthFlow, StatusCode: response.StatusCode, Body: http.StatusText(response.StatusCode)}
	}
	body, err := readBounded(response.Body, flowEnvelopeLimit)
	if err != nil {
		return nil, fmt.Errorf("read Google Flow bootstrap: %w", err)
	}
	text := string(body)
	session := &flowSession{
		httpClient:         client,
		endpoints:          endpoints,
		at:                 firstPatternValue(text, flowTokenPatterns...),
		buildLabel:         firstPatternValue(text, flowBuildPattern),
		sessionID:          firstPatternValue(text, flowSessionPattern),
		language:           firstPatternValue(text, flowLanguagePattern),
		siteKey:            firstPatternValue(text, flowSiteKeyPatterns...),
		uploadRegistryPath: registryPath,
		maxBytes:           maxBytes,
	}
	if session.at == "" {
		return nil, errors.New("Google Flow browser session is not authenticated")
	}
	if session.siteKey == "" {
		return nil, errors.New("Google Flow bootstrap omitted its challenge configuration")
	}
	if session.language == "" {
		session.language = "en"
	}
	session.recaptchaVersion, err = session.loadRecaptchaVersion(ctx)
	if err != nil {
		return nil, err
	}
	projects, err := session.callRPC(ctx, flowGetProjectsRPC, []any{"projects/*", 21})
	if err != nil {
		return nil, fmt.Errorf("discover Google Flow projects: %w", err)
	}
	session.project, err = parseFlowProject(projects)
	if err != nil {
		return nil, err
	}
	models, err := session.callRPC(ctx, flowGetModelsRPC, []any{})
	if err != nil {
		return nil, fmt.Errorf("discover Google Flow image models: %w", err)
	}
	session.availableUsage, err = parseFlowImageUsages(models)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func selectFlowModel(requested string) (flowModelPreset, error) {
	switch strings.TrimSpace(requested) {
	case "", flowModelNanoBananaPro:
		return flowModelPreset{id: flowModelNanoBananaPro, product: flowProductBananaPro, usage: flowUsageBananaPro}, nil
	case flowModelNanoBanana2:
		return flowModelPreset{id: flowModelNanoBanana2, product: flowProductBanana2, usage: flowUsageBanana2}, nil
	default:
		return flowModelPreset{}, fmt.Errorf(
			"unsupported Google Flow image model %q; use %q or %q",
			strings.TrimSpace(requested),
			flowModelNanoBananaPro,
			flowModelNanoBanana2,
		)
	}
}

func flowAspectRatio(size string) (int, error) {
	if size == "" || size == "auto" {
		return 0, nil
	}
	matches := sizePattern.FindStringSubmatch(size)
	if matches == nil {
		return 0, fmt.Errorf("unsupported Google Flow size %q", size)
	}
	width, widthErr := strconv.ParseInt(matches[1], 10, 64)
	height, heightErr := strconv.ParseInt(matches[2], 10, 64)
	if widthErr != nil || heightErr != nil {
		return 0, fmt.Errorf("unsupported Google Flow size %q", size)
	}
	divisor := greatestCommonDivisor(width, height)
	switch [2]int64{width / divisor, height / divisor} {
	case [2]int64{16, 9}:
		return 3, nil
	case [2]int64{4, 3}:
		return 5, nil
	case [2]int64{1, 1}:
		return 1, nil
	case [2]int64{3, 4}:
		return 4, nil
	case [2]int64{9, 16}:
		return 2, nil
	default:
		return 0, fmt.Errorf("unsupported Google Flow aspect ratio %q (use 1:1, 4:3, 3:4, 16:9, or 9:16 dimensions)", size)
	}
}

func greatestCommonDivisor(left, right int64) int64 {
	for right != 0 {
		left, right = right, left%right
	}
	return left
}

func (s *flowSession) generateOne(
	ctx context.Context,
	prompt string,
	preset flowModelPreset,
	aspect int,
	references []string,
) ([]byte, error) {
	challenge, err := s.challenge(ctx, flowImageGenerationAction)
	if err != nil {
		return nil, err
	}
	seed, err := rand.Int(rand.Reader, big.NewInt(2147483647))
	if err != nil {
		return nil, errors.New("create Google Flow image seed failed")
	}
	contextValue := flowProjectContext(s.project, challenge)
	generated := make([]any, 14)
	if len(references) > 0 {
		generated[2] = flowImageReferences(references)
	}
	generated[3] = seed.Int64()
	if aspect != 0 {
		generated[4] = aspect
	}
	generated[5] = preset.usage
	generated[7] = contextValue
	generated[8] = []any{[]any{[]any{prompt}}}
	generated[12] = uuid.NewString()
	generated[13] = uuid.NewString()
	outer := make([]any, 5)
	outer[1] = []any{generated}
	outer[2] = 1
	outer[3] = contextValue
	outer[4] = []any{uuid.NewString()}
	payload, err := s.callRPC(ctx, flowGenerateRPC, outer)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", errFlowGenerationRPC, err)
	}
	media, err := parseFlowGeneratedMedia(payload, s.project, preset)
	if err != nil {
		return nil, err
	}
	return s.downloadImage(ctx, media.url)
}

func flowProjectContext(project, challenge string) []any {
	value := make([]any, 11)
	value[1] = 22
	value[5] = project
	value[10] = []any{challenge, 1}
	return value
}

func flowImageReferences(references []string) []any {
	values := make([]any, len(references))
	for index, reference := range references {
		values[index] = []any{reference, nil, nil, nil, 1}
	}
	return values
}

func (s *flowSession) upload(ctx context.Context, image flowUploadInput) (flowMedia, error) {
	challenge, err := s.challenge(ctx, flowUploadImageAction)
	if err != nil {
		return flowMedia{}, err
	}
	payload := make([]any, 12)
	payload[0] = flowProjectContext(s.project, challenge)
	payload[1] = base64.StdEncoding.EncodeToString(image.data)
	payload[2] = image.mediaType
	payload[3] = 1
	payload[8] = image.label
	payload[10] = uuid.NewString()
	payload[11] = uuid.NewString()
	response, err := s.callRPC(ctx, flowUploadRPC, payload)
	if err != nil {
		return flowMedia{}, err
	}
	mediaValue, ok := flowSliceAt(response, 0)
	if !ok {
		return flowMedia{}, errors.New("Google Flow upload response omitted media")
	}
	media, err := parseFlowMedia(mediaValue)
	if err != nil {
		return flowMedia{}, fmt.Errorf("decode Google Flow upload response: %w", err)
	}
	if !validFlowMediaReference(media, s.project) {
		return flowMedia{}, errors.New("Google Flow upload response returned invalid media")
	}
	return media, nil
}

func (s *flowSession) getMedia(ctx context.Context, mediaID string) (flowMedia, error) {
	response, err := s.callRPC(ctx, flowGetMediaRPC, []any{mediaID})
	if err != nil {
		if errors.Is(err, errFlowRPCNoPayload) {
			return flowMedia{}, errFlowMediaUnavailable
		}
		return flowMedia{}, err
	}
	media, err := parseFlowMedia(response)
	if err != nil || !validFlowMediaReference(media, s.project) || media.id != mediaID {
		return flowMedia{}, errFlowMediaUnavailable
	}
	return media, nil
}

func validFlowMediaReference(media flowMedia, project string) bool {
	return media.image && media.id != "" && media.project == project &&
		(!media.statusPresent || media.status == flowMediaStatusSucceeded)
}

func parseFlowGeneratedMedia(payload []any, project string, preset flowModelPreset) (flowMedia, error) {
	if len(payload) != 2 {
		return flowMedia{}, errors.New("Google Flow returned malformed generated media")
	}
	values, ok := flowSliceAt(payload, 0)
	if !ok || len(values) != 1 {
		return flowMedia{}, errors.New("Google Flow returned an unexpected generated-image count")
	}
	generated, ok := values[0].([]any)
	if !ok {
		return flowMedia{}, errors.New("Google Flow returned malformed generated media")
	}
	image, imageOK := flowSliceAt(generated, 6)
	detail, detailOK := flowSliceAt(image, 0)
	dimensions, dimensionsOK := flowSliceAt(image, 2)
	mediaID, mediaIDOK := flowStringAt(generated, 0, 512)
	workflowID, workflowIDOK := flowStringAt(generated, 2, 512)
	if !imageOK || !detailOK || !mediaIDOK || !workflowIDOK || !dimensionsOK || len(dimensions) != 2 {
		return flowMedia{}, errors.New("Google Flow returned malformed generated media")
	}
	width, widthOK := flowIntAt(dimensions, 0)
	height, heightOK := flowIntAt(dimensions, 1)
	if !widthOK || !heightOK || width < 1 || height < 1 || width > 32768 || height > 32768 {
		return flowMedia{}, errors.New("Google Flow returned invalid image dimensions")
	}
	detailWorkflowID, detailWorkflowIDOK := flowStringAt(detail, 11, 512)
	mediaURL, mediaURLOK := flowStringAt(detail, 13, 8192)
	usageCode, usageCodeOK := flowIntAt(detail, 8)
	if !detailWorkflowIDOK || detailWorkflowID != workflowID || !mediaURLOK || !usageCodeOK || usageCode != flowUsageCode(preset.usage) {
		return flowMedia{}, errors.New("Google Flow returned mismatched image details")
	}
	workflows, ok := flowSliceAt(payload, 1)
	if !ok || len(workflows) != 1 {
		return flowMedia{}, errors.New("Google Flow returned malformed workflow data")
	}
	workflow, ok := workflows[0].([]any)
	if !ok {
		return flowMedia{}, errors.New("Google Flow returned malformed workflow data")
	}
	echoedWorkflowID, echoedWorkflowIDOK := flowStringAt(workflow, 0, 512)
	echoedProject, echoedProjectOK := flowStringAt(workflow, 4, 512)
	workflowState, workflowStateOK := flowSliceAt(workflow, 3)
	workflowMediaID, workflowMediaIDOK := flowStringAt(workflowState, 4, 512)
	if !echoedWorkflowIDOK || echoedWorkflowID != workflowID || !echoedProjectOK || echoedProject != project || !workflowStateOK || !workflowMediaIDOK || workflowMediaID != mediaID {
		return flowMedia{}, errors.New("Google Flow returned mismatched workflow data")
	}
	return flowMedia{
		id:            mediaID,
		project:       project,
		url:           mediaURL,
		image:         true,
		status:        flowMediaStatusSucceeded,
		statusPresent: true,
	}, nil
}

func flowUsageCode(usage string) int {
	switch usage {
	case flowUsageBanana2:
		return 29
	case flowUsageBananaPro:
		return 25
	default:
		return 0
	}
}

func parseFlowMedia(value []any) (flowMedia, error) {
	media := flowMedia{}
	media.id, _ = flowStringAt(value, 0, 512)
	media.project, _ = flowStringAt(value, 1, 512)
	image, ok := flowSliceAt(value, 6)
	if !ok {
		return flowMedia{}, errors.New("media is not an image")
	}
	if generated, generatedOK := flowSliceAt(image, 0); generatedOK {
		media.url, _ = flowStringAt(generated, 13, 8192)
	} else if alternate, alternateOK := flowSliceAt(image, 1); alternateOK {
		media.url, _ = flowStringAt(alternate, 3, 8192)
	} else {
		return flowMedia{}, errors.New("media image details are missing")
	}
	media.image = true
	if metadata, metadataOK := flowSliceAt(value, 5); metadataOK {
		if status, statusOK := flowSliceAt(metadata, 8); statusOK {
			if value, valueOK := flowIntAt(status, 0); valueOK {
				media.status = value
				media.statusPresent = true
			}
		}
	}
	return media, nil
}

func parseFlowProject(payload []any) (string, error) {
	projects, ok := flowSliceAt(payload, 0)
	if !ok || len(projects) == 0 {
		return "", errors.New("Google Flow account has no available project")
	}
	project, ok := projects[0].([]any)
	if !ok {
		return "", errors.New("Google Flow returned malformed project data")
	}
	projectID, ok := flowStringAt(project, 0, 512)
	if !ok || !flowProjectIDPattern.MatchString(projectID) {
		return "", errors.New("Google Flow returned an invalid project")
	}
	return projectID, nil
}

func parseFlowImageUsages(payload []any) (map[string]bool, error) {
	config, ok := flowSliceAt(payload, 0)
	if !ok {
		return nil, errors.New("Google Flow returned malformed model configuration")
	}
	families, ok := flowSliceAt(config, 5)
	if !ok {
		return nil, errors.New("Google Flow returned no image model configuration")
	}
	available := make(map[string]bool)
	for _, familyValue := range families {
		family, familyOK := familyValue.([]any)
		if !familyOK {
			continue
		}
		usages, usagesOK := flowSliceAt(family, 1)
		if !usagesOK {
			continue
		}
		for _, usageValue := range usages {
			usage, usageOK := usageValue.([]any)
			if !usageOK {
				continue
			}
			key, keyOK := flowStringAt(usage, 0, 128)
			if keyOK && (key == flowUsageBanana2 || key == flowUsageBananaPro) {
				available[key] = true
			}
		}
	}
	if len(available) == 0 {
		return nil, errors.New("Google Flow returned no supported image model")
	}
	return available, nil
}

func (s *flowSession) callRPC(ctx context.Context, rpcID string, payload []any) ([]any, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("encode Google Flow request failed")
	}
	envelopeJSON, err := json.Marshal([]any{[]any{[]any{rpcID, string(payloadJSON), nil, "generic"}}})
	if err != nil {
		return nil, errors.New("encode Google Flow request envelope failed")
	}
	endpoint, err := url.Parse(s.endpoints.rpc)
	if err != nil {
		return nil, errors.New("Google Flow RPC endpoint is invalid")
	}
	requestNumber, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return nil, errors.New("create Google Flow request identifier failed")
	}
	query := endpoint.Query()
	query.Set("rpcids", rpcID)
	query.Set("source-path", s.sourcePath())
	query.Set("hl", s.language)
	query.Set("_reqid", strconv.FormatInt(requestNumber.Int64()+100000, 10))
	query.Set("rt", "c")
	if s.buildLabel != "" {
		query.Set("bl", s.buildLabel)
	}
	if s.sessionID != "" {
		query.Set("f.sid", s.sessionID)
	}
	endpoint.RawQuery = query.Encode()
	form := url.Values{"at": {s.at}, "f.req": {string(envelopeJSON)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	applyFlowHeaders(request, s.endpoints)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	request.Header.Set("X-Same-Domain", "1")
	response, err := s.httpClient.Do(request)
	if err != nil {
		return nil, errors.New("send Google Flow request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &APIError{AuthMode: AuthFlow, StatusCode: response.StatusCode, Body: http.StatusText(response.StatusCode)}
	}
	body, err := readBounded(response.Body, flowEnvelopeLimit)
	if err != nil {
		return nil, fmt.Errorf("read Google Flow response: %w", err)
	}
	return parseFlowRPCResponse(body, rpcID)
}

func (s *flowSession) sourcePath() string {
	if s.project == "" {
		return "/"
	}
	return "/project/" + s.project
}

func parseFlowRPCResponse(body []byte, rpcID string) ([]any, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), int(flowEnvelopeLimit))
	var payload []any
	matches := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte(")]}'")))
		if len(line) == 0 || line[0] != '[' {
			continue
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			continue
		}
		found := collectFlowRPCPayloads(value, rpcID, 0)
		for _, candidate := range found {
			matches++
			payload = candidate
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("decode Google Flow response: %w", err)
	}
	if matches == 0 {
		return nil, errFlowRPCNoPayload
	}
	if matches != 1 {
		return nil, errors.New("Google Flow returned multiple RPC payloads")
	}
	return payload, nil
}

func collectFlowRPCPayloads(value any, rpcID string, depth int) [][]any {
	if depth > 8 {
		return nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	if len(values) >= 3 && values[0] == "wrb.fr" && values[1] == rpcID {
		encoded, encodedOK := values[2].(string)
		if !encodedOK || len(encoded) > int(flowEnvelopeLimit) {
			return nil
		}
		var payload []any
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.UseNumber()
		if decoder.Decode(&payload) == nil {
			return [][]any{payload}
		}
		return nil
	}
	var found [][]any
	for _, child := range values {
		found = append(found, collectFlowRPCPayloads(child, rpcID, depth+1)...)
	}
	return found
}

func (s *flowSession) loadRecaptchaVersion(ctx context.Context) (string, error) {
	endpoint, err := url.Parse(s.endpoints.recaptchaScript)
	if err != nil {
		return "", errors.New("Google Flow challenge script endpoint is invalid")
	}
	query := endpoint.Query()
	query.Set("trustedtypes", "true")
	query.Set("render", s.siteKey)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	applyFlowHeaders(request, s.endpoints)
	response, err := s.httpClient.Do(request)
	if err != nil {
		return "", errors.New("load Google Flow challenge configuration failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errors.New("load Google Flow challenge configuration failed")
	}
	body, err := readBounded(response.Body, 2<<20)
	if err != nil {
		return "", errors.New("read Google Flow challenge configuration failed")
	}
	match := flowRecaptchaRelease.FindSubmatch(body)
	if len(match) != 2 || len(match[1]) == 0 || len(match[1]) > 256 {
		return "", errors.New("Google Flow challenge version is unavailable")
	}
	return string(match[1]), nil
}

func (s *flowSession) challenge(ctx context.Context, action string) (string, error) {
	if action != flowImageGenerationAction && action != flowUploadImageAction {
		return "", errors.New("unsupported Google Flow challenge action")
	}
	origin := base64.StdEncoding.EncodeToString([]byte(s.endpoints.recaptchaOrigin))
	origin = strings.ReplaceAll(origin, "=", ".")
	anchorEndpoint, err := url.Parse(s.endpoints.recaptchaAnchor)
	if err != nil {
		return "", errors.New("Google Flow challenge endpoint is invalid")
	}
	query := anchorEndpoint.Query()
	query.Set("ar", "1")
	query.Set("k", s.siteKey)
	query.Set("co", origin)
	query.Set("hl", s.language)
	query.Set("v", s.recaptchaVersion)
	query.Set("size", "invisible")
	query.Set("sa", action)
	query.Set("cb", strings.ReplaceAll(uuid.NewString(), "-", ""))
	anchorEndpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, anchorEndpoint.String(), nil)
	if err != nil {
		return "", err
	}
	applyFlowHeaders(request, s.endpoints)
	response, err := s.httpClient.Do(request)
	if err != nil {
		return "", errors.New("start Google Flow challenge failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errors.New("start Google Flow challenge failed")
	}
	body, err := readBounded(response.Body, 2<<20)
	if err != nil {
		return "", errors.New("read Google Flow challenge failed")
	}
	match := flowRecaptchaAnchorToken.FindSubmatch(body)
	if len(match) != 2 {
		return "", errors.New("Google Flow challenge did not return an anchor token")
	}
	anchorToken := html.UnescapeString(string(match[1]))
	if anchorToken == "" || len(anchorToken) > 16384 {
		return "", errors.New("Google Flow challenge returned an invalid anchor token")
	}
	reloadEndpoint, err := url.Parse(s.endpoints.recaptchaReload)
	if err != nil {
		return "", errors.New("Google Flow challenge reload endpoint is invalid")
	}
	reloadQuery := reloadEndpoint.Query()
	reloadQuery.Set("k", s.siteKey)
	reloadEndpoint.RawQuery = reloadQuery.Encode()
	form := url.Values{
		"v":      {s.recaptchaVersion},
		"reason": {"q"},
		"c":      {anchorToken},
		"k":      {s.siteKey},
		"co":     {origin},
		"hl":     {s.language},
		"size":   {"invisible"},
		"sa":     {action},
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, reloadEndpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	applyFlowHeaders(request, s.endpoints)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err = s.httpClient.Do(request)
	if err != nil {
		return "", errors.New("complete Google Flow challenge failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errors.New("complete Google Flow challenge failed")
	}
	body, err = readBounded(response.Body, 2<<20)
	if err != nil {
		return "", errors.New("read Google Flow challenge response failed")
	}
	match = flowRecaptchaResponseToken.FindSubmatch(body)
	if len(match) != 2 {
		return "", errors.New("Google Flow challenge did not return a response token")
	}
	token := string(match[1])
	if token == "" || len(token) > 16384 {
		return "", errors.New("Google Flow challenge returned an invalid response token")
	}
	return token, nil
}

func (s *flowSession) downloadImage(ctx context.Context, rawURL string) ([]byte, error) {
	if !s.endpoints.allowUntrustedMedia && !trustedFlowMediaURL(rawURL) {
		return nil, errors.New("Google Flow returned an untrusted image URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("Google Flow returned an invalid image URL")
	}
	applyFlowHeaders(request, s.endpoints)
	response, err := s.mediaHTTPClient().Do(request)
	if err != nil {
		if errors.Is(err, errUntrustedFlowMediaRedirect) {
			return nil, errUntrustedFlowMediaRedirect
		}
		return nil, errors.New("download Google Flow image failed")
	}
	defer response.Body.Close()
	if !s.endpoints.allowUntrustedMedia && !trustedFlowMediaURL(response.Request.URL.String()) {
		return nil, errUntrustedFlowMediaRedirect
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &APIError{AuthMode: AuthFlow, StatusCode: response.StatusCode, Body: http.StatusText(response.StatusCode)}
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return nil, errors.New("Google Flow image response has an unsupported media type")
	}
	return readBounded(response.Body, maxOutputImageBytes)
}

func (s *flowSession) mediaHTTPClient() *http.Client {
	client := *s.httpClient
	original := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !s.endpoints.allowUntrustedMedia && !trustedFlowMediaURL(request.URL.String()) {
			return errUntrustedFlowMediaRedirect
		}
		if original != nil {
			return original(request, via)
		}
		return nil
	}
	return &client
}

func applyFlowHeaders(request *http.Request, endpoints flowEndpoints) {
	origin := endpoints.origin
	if origin == "" {
		origin = flowOrigin
	}
	request.Header.Set("Origin", origin)
	request.Header.Set("Referer", strings.TrimRight(origin, "/")+"/")
	request.Header.Set("User-Agent", flowUserAgent)
}

func firstPatternValue(value string, patterns ...*regexp.Regexp) string {
	for _, pattern := range patterns {
		match := pattern.FindStringSubmatch(value)
		if len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

func flowSliceAt(value []any, index int) ([]any, bool) {
	if index < 0 || index >= len(value) {
		return nil, false
	}
	nested, ok := value[index].([]any)
	return nested, ok
}

func flowStringAt(value []any, index, limit int) (string, bool) {
	if index < 0 || index >= len(value) {
		return "", false
	}
	text, ok := value[index].(string)
	return text, ok && text != "" && len(text) <= limit
}

func flowIntAt(value []any, index int) (int, bool) {
	if index < 0 || index >= len(value) {
		return 0, false
	}
	switch number := value[index].(type) {
	case json.Number:
		parsed, err := strconv.Atoi(number.String())
		return parsed, err == nil
	case float64:
		parsed := int(number)
		return parsed, number == float64(parsed)
	case int:
		return number, true
	default:
		return 0, false
	}
}

func trustedFlowMediaURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "flow-content.google" || host == "google.com" || strings.HasSuffix(host, ".google.com") || host == "googleusercontent.com" || strings.HasSuffix(host, ".googleusercontent.com")
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if limit < 1 {
		return nil, errors.New("invalid response size limit")
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrResponseTooLarge, limit)
	}
	return data, nil
}

func flowUploadLabel(filename string, index int) string {
	label := filepath.Base(strings.TrimSpace(filename))
	if label == "" || label == "." {
		return fmt.Sprintf("image-%d", index+1)
	}
	if len(label) > 255 {
		return label[:255]
	}
	return label
}

func jarHasGoogleSessionCookie(jar http.CookieJar) bool {
	if jar == nil {
		return false
	}
	cookies := jar.Cookies(&url.URL{Scheme: "https", Host: "gemini.google.com", Path: "/"})
	for _, cookie := range cookies {
		if cookie.Name == "__Secure-1PSID" && cookie.Value != "" {
			return true
		}
	}
	return false
}
