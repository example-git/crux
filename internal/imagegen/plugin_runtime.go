package imagegen

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/providertransport/clientidentity"
)

func imagePluginOutput(item any, limit int64) (string, error) {
	switch data := item.(type) {
	case []byte:
		if len(data) == 0 || int64(len(data)) > limit {
			return "", errors.New("image output is empty or exceeds plugin byte limit")
		}
		return base64.StdEncoding.EncodeToString(data), nil
	case string:
		if data == "" || int64(len(data)) > (limit+2)/3*4 {
			return "", errors.New("image output is empty or exceeds plugin byte limit")
		}
		decoded := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(data))
		count, err := io.Copy(io.Discard, io.LimitReader(decoded, limit+1))
		if err != nil || count == 0 || count > limit {
			return "", errors.New("image output base64 is invalid or exceeds plugin byte limit")
		}
		return data, nil
	default:
		return "", errors.New("image workflow returned missing or unsupported image data")
	}
}

type PluginCredentials struct {
	Identity   string
	Values     map[string]any
	CookieJars map[string]http.CookieJar
	Validate   func() error
}

type PluginRuntime struct {
	UploadDirectory    string
	Environment        []string
	Select             func(context.Context) (providerplugin.ImageOwner, error)
	ResolveOwner       func(string) (providerplugin.ImageOwner, error)
	Manager            *providerplugin.Manager
	Client             *http.Client
	ResolveCredentials func(context.Context, providerplugin.RegisteredImageBundle) (PluginCredentials, error)
	Configuration      func(providerplugin.ImageOwner) (map[string]any, error)
	mu                 sync.Mutex
	sessions           map[providerplugin.ImageOwner]*imagePluginSession
}

type imagePluginSession struct {
	identity [32]byte
	loading  chan struct{}
	mu       sync.Mutex
	value    any
	uploads  map[string]any
}

var imagePluginPermits = struct {
	sync.Mutex
	values map[providerplugin.ImageOwner]chan struct{}
}{values: map[providerplugin.ImageOwner]chan struct{}{}}

func acquireImagePlugin(ctx context.Context, owner providerplugin.ImageOwner, concurrency int) (func(), error) {
	imagePluginPermits.Lock()
	permit := imagePluginPermits.values[owner]
	if permit == nil {
		permit = make(chan struct{}, concurrency)
		imagePluginPermits.values[owner] = permit
	}
	imagePluginPermits.Unlock()
	select {
	case permit <- struct{}{}:
		return func() { <-permit }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *PluginRuntime) Prepare(owner providerplugin.ImageOwner, request JobRequest) (JobRequest, manifest.ImageManifest, error) {
	if r == nil || r.Manager == nil {
		return request, manifest.ImageManifest{}, errors.New("image plugin manager is unavailable")
	}
	bundle, err := r.Manager.ImageBundleForOwner(owner)
	if err != nil {
		return request, manifest.ImageManifest{}, err
	}
	value := bundle.Manifest
	if request.Backend != "" && request.Backend != BackendAuto && string(request.Backend) != owner.Backend {
		return request, value, errors.New("image request backend conflicts with captured owner")
	}
	request.Backend = Backend(owner.Backend)
	if request.Mode != ModeGenerate && request.Mode != ModeEdit {
		return request, value, errors.New("unsupported image mode")
	}
	if request.Mode == ModeEdit && value.Edit == "" {
		return request, value, errors.New("selected image plugin does not support editing")
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return request, value, errors.New("image prompt is required")
	}
	if request.Count < 1 || request.Count > value.Limits.Variants {
		return request, value, errors.New("image variant count exceeds selected plugin limits")
	}
	if len(request.InputPaths) > value.Limits.InputImages {
		return request, value, errors.New("image input count exceeds selected plugin limits")
	}
	if request.Model == "" || request.Model == "auto" {
		request.Model = value.DefaultModel
	}
	if !slices.ContainsFunc(value.Models, func(model manifest.ImageModel) bool { return model.ID == request.Model }) {
		return request, value, errors.New("selected image model is unavailable")
	}
	if request.Quality == "" {
		request.Quality = Quality(value.Options.Quality[0])
	}
	if request.Background == "" {
		request.Background = Background(value.Options.Background[0])
	}
	if !slices.Contains(value.Options.Quality, string(request.Quality)) || !slices.Contains(value.Options.Background, string(request.Background)) {
		return request, value, errors.New("selected image plugin does not support requested options")
	}
	if request.Size == "" {
		request.Size = "auto"
	}
	if !slices.Contains(value.Options.Sizes, request.Size) || value.Options.DimensionLimits != nil && request.Size != "auto" {
		parts := strings.Split(request.Size, "x")
		if !value.Options.Dimensions || len(parts) != 2 {
			return request, value, errors.New("selected image plugin does not support requested size")
		}
		for _, part := range parts {
			dimension, err := strconv.Atoi(part)
			if err != nil || dimension < 1 || dimension > 16384 || strconv.Itoa(dimension) != part {
				return request, value, errors.New("invalid image dimensions")
			}
		}
		if limits := value.Options.DimensionLimits; limits != nil {
			width, _ := strconv.ParseInt(parts[0], 10, 64)
			height, _ := strconv.ParseInt(parts[1], 10, 64)
			pixels := width * height
			if width%int64(limits.Multiple) != 0 || height%int64(limits.Multiple) != 0 || width > int64(limits.MaxEdge) || height > int64(limits.MaxEdge) || pixels < limits.MinPixels || pixels > limits.MaxPixels || width > height*int64(limits.MaxAspect) || height > width*int64(limits.MaxAspect) {
				return request, value, errors.New("image dimensions exceed selected plugin limits")
			}
		}
	}
	if request.Size != "auto" && len(value.Options.AspectRatios) > 0 {
		ratio, err := manifest.ImageAspectRatio(request.Size)
		if err != nil || !slices.Contains(value.Options.AspectRatios, ratio) {
			return request, value, errors.New("image aspect ratio is unsupported by selected plugin")
		}
	}
	return request, value, nil
}

func (r *PluginRuntime) Execute(ctx context.Context, owner providerplugin.ImageOwner, request JobRequest, inputs []EditImage) (response *Response, resultErr error) {
	request, value, err := r.Prepare(owner, request)
	if err != nil {
		return nil, err
	}
	if len(inputs) > value.Limits.InputImages || request.Mode == ModeEdit && len(inputs) == 0 || request.Mode == ModeGenerate && len(inputs) != 0 {
		return nil, errors.New("image input count is invalid")
	}
	var total int64
	preparedInputs := make([]any, len(inputs))
	for index, input := range inputs {
		if int64(len(input.Data)) > value.Limits.InputBytes {
			return nil, errors.New("image input exceeds selected plugin byte limit")
		}
		total += int64(len(input.Data))
		if total > value.Limits.TotalInputBytes {
			return nil, errors.New("image inputs exceed selected plugin aggregate limit")
		}
		digest := sha256.Sum256(input.Data)
		preparedInputs[index] = map[string]any{"filename": input.Filename, "media_type": input.MIMEType, "data": input.Data, "sha256": hex.EncodeToString(digest[:])}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(value.Limits.TimeoutSeconds)*time.Second)
	defer cancel()
	release, err := acquireImagePlugin(ctx, owner, value.Limits.Concurrency)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := r.Manager.ValidateImageOwner(ctx, owner); err != nil {
		return nil, err
	}
	bundle, err := r.Manager.ImageBundleForOwner(owner)
	if err != nil {
		return nil, err
	}
	configuration := map[string]any{}
	if r.Configuration != nil {
		configuration, err = r.Configuration(owner)
		if err != nil {
			return nil, err
		}
	}
	configuration, configurationDigest, err := imageConfiguration(value, configuration)
	if err != nil {
		return nil, err
	}
	credentials := PluginCredentials{}
	if len(value.Credentials) > 0 {
		if r.ResolveCredentials == nil {
			return nil, errors.New("image plugin credential resolver is unavailable")
		}
		credentials, err = r.ResolveCredentials(ctx, bundle)
		if err != nil {
			return nil, err
		}
	}
	for index, origin := range value.Origins {
		if origin.ProviderCredential == "" {
			continue
		}
		credential, ok := credentials.Values[origin.ProviderCredential].(map[string]any)
		if !ok {
			return nil, errors.New("configured image origin credential is unavailable")
		}
		baseURL, ok := credential["base_url"].(string)
		if !ok {
			return nil, errors.New("configured image endpoint is unavailable")
		}
		if baseURL == "" {
			continue
		}
		parsed, err := url.Parse(baseURL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("configured image endpoint must be an exact HTTPS URL")
		}
		value.Origins[index].URL = parsed.Scheme + "://" + parsed.Host
	}
	identity, err := imageSessionIdentity(configurationDigest, credentials)
	if err != nil {
		return nil, err
	}
	host := &providertransport.ImageWorkflowHost{Manifest: value, Client: r.Client, Credentials: credentials.Values, CookieJars: credentials.CookieJars, ValidateOwner: func() error {
		if err := r.Manager.ValidateImageOwner(ctx, owner); err != nil {
			return err
		}
		if r.Configuration != nil {
			current, err := r.Configuration(owner)
			if err != nil {
				return err
			}
			_, digest, err := imageConfiguration(value, current)
			if err != nil {
				return err
			}
			if digest != configurationDigest {
				return errors.New("image configuration changed during execution")
			}
		}
		if credentials.Validate != nil {
			return credentials.Validate()
		}
		return nil
	}}
	session := r.sessionFor(owner, identity)
	defer func() {
		if resultErr != nil || response != nil && len(response.Failures) > 0 {
			r.invalidateSession(owner, session)
		}
	}()
	platform, err := imagePlatformValues(ctx, r.Environment)
	if err != nil {
		return nil, err
	}
	values := map[string]any{"host": platform, "request": map[string]any{"prompt": request.Prompt, "count": request.Count, "quality": string(request.Quality), "background": string(request.Background), "size": request.Size}, "configuration": configuration, "inputs": preparedInputs}
	ratio := "auto"
	if request.Size != "auto" {
		ratio, err = manifest.ImageAspectRatio(request.Size)
		if err != nil && len(value.Options.AspectRatios) > 0 {
			return nil, err
		}
	}
	values["request"].(map[string]any)["aspect_ratio"] = ratio
	identities := map[string]any{}
	for name, declaration := range value.ClientIdentities {
		version, agent, err := clientidentity.ResolveWithEnvironment(providertransport.ContextWithOwnerValidator(ctx, host.ValidateOwner), &declaration, r.Environment)
		if err != nil {
			return nil, err
		}
		identities[name] = map[string]any{"version": version, "user_agent": agent}
	}
	values["clients"] = identities
	if value.Session != "" {
		values["session"], err = session.bootstrap(ctx, func() (any, error) { return host.Execute(ctx, value.Session, values) })
		if err != nil {
			return nil, err
		}
	}
	var available []any
	if value.Discovery != "" {
		discovered, err := host.Execute(ctx, value.Discovery, values)
		if err != nil {
			return nil, err
		}
		var ok bool
		available, ok = providertransport.ImageWorkflowValue(discovered).([]any)
		if !ok {
			return nil, errors.New("image model discovery did not return an array")
		}
	}
	modelIndex := slices.IndexFunc(value.Models, func(model manifest.ImageModel) bool { return model.ID == request.Model })
	model := value.Models[modelIndex]
	isAvailable := func(model manifest.ImageModel) bool {
		return model.AvailabilityKey == "" || slices.ContainsFunc(available, func(item any) bool { return item == model.AvailabilityKey })
	}
	fallback := func(model manifest.ImageModel) (manifest.ImageModel, bool) {
		if model.Fallback == nil {
			return manifest.ImageModel{}, false
		}
		index := slices.IndexFunc(value.Models, func(candidate manifest.ImageModel) bool { return candidate.ID == model.Fallback.Model })
		if index < 0 || !isAvailable(value.Models[index]) {
			return manifest.ImageModel{}, false
		}
		return value.Models[index], true
	}
	if !isAvailable(model) {
		if model.Fallback == nil || !model.Fallback.WhenUnavailable {
			return nil, errors.New("selected image model is unavailable in discovered catalog")
		}
		next, ok := fallback(model)
		if !ok {
			return nil, errors.New("declared image fallback model is unavailable")
		}
		model = next
	}
	if value.Upload != nil && len(inputs) > 0 {
		var persistent *imageUploadCache
		if value.Upload.Persistent {
			persistent, err = openImageUploadCache(ctx, r.UploadDirectory, owner, identity, host.ValidateOwner)
			if err != nil {
				return nil, err
			}
			defer persistent.release()
		}
		uploads := make([]any, len(inputs))
		for index, input := range preparedInputs {
			values["input"] = input
			cacheKey := ""
			if value.Upload.CacheKey != nil {
				key, err := providertransport.EvaluateImageValue(*value.Upload.CacheKey, values)
				if err != nil {
					return nil, err
				}
				text, ok := providertransport.ImageWorkflowValue(key).(string)
				if !ok || len(text) > 8192 {
					return nil, errors.New("invalid image upload cache key")
				}
				digest := sha256.Sum256([]byte(text))
				cacheKey = hex.EncodeToString(digest[:])
			}
			session.mu.Lock()
			cached := session.uploads[cacheKey]
			session.mu.Unlock()
			if persistent != nil {
				cached = nil
				if reference, exists := persistent.entries[cacheKey]; exists {
					cached, err = host.ScopeUploadIdentifier(reference)
					if err != nil {
						return nil, err
					}
				}
			}
			if cacheKey != "" && cached != nil {
				values["upload"] = cached
				checked, err := host.Execute(ctx, value.Upload.Lookup, values)
				if err != nil {
					return nil, err
				}
				valid, ok := providertransport.ImageWorkflowValue(checked).(bool)
				if !ok {
					return nil, errors.New("image upload lookup must return a boolean")
				}
				if valid {
					uploads[index] = cached
					continue
				}
			}
			uploaded, err := host.Execute(ctx, value.Upload.Workflow, values)
			if err != nil {
				return nil, err
			}
			if persistent != nil {
				identifier, err := providertransport.ImageUploadReferenceFromValue(uploaded)
				if err != nil {
					return nil, err
				}
				if err := persistent.save(cacheKey, identifier); err != nil {
					return nil, err
				}
			}
			uploads[index] = uploaded
			if cacheKey != "" {
				session.mu.Lock()
				if len(session.uploads) >= 1024 {
					session.uploads = map[string]any{}
				}
				session.uploads[cacheKey] = uploaded
				session.mu.Unlock()
			}
		}
		values["uploads"] = uploads
	}
	workflow := value.Generate
	if request.Mode == ModeEdit {
		workflow = value.Edit
	}
	run := func(model manifest.ImageModel) (*Response, bool) {
		values["model"] = map[string]any{"id": model.ID, "parameters": model.Parameters}
		count := request.Count
		if value.VariantMode == "batch" {
			count = 1
		}
		type variantResult struct {
			images   []ImageData
			failures []ImageVariantFailure
			err      error
		}
		results := make([]variantResult, count)
		var workers sync.WaitGroup
		for index := range count {
			workers.Go(func() {
				variantValues := make(map[string]any, len(values)+1)
				for key, item := range values {
					variantValues[key] = item
				}
				variantValues["variant"] = index + 1
				result, err := host.Execute(ctx, workflow, variantValues)
				if err != nil {
					results[index].err = err
					return
				}
				items, ok := providertransport.ImageWorkflowValue(result).([]any)
				if !ok {
					results[index].err = errors.New("image workflow result must be an image array")
					return
				}
				expected := 1
				if value.VariantMode == "batch" {
					expected = request.Count
				}
				if len(items) > expected {
					results[index].err = errors.New("image workflow returned too many images")
					return
				}
				for itemIndex := range expected {
					variant := index + 1
					if value.VariantMode == "batch" {
						variant = itemIndex + 1
					}
					var item any
					if itemIndex < len(items) {
						item = items[itemIndex]
					}
					encoded, err := imagePluginOutput(item, value.Limits.OutputBytes)
					if err != nil {
						results[index].failures = append(results[index].failures, ImageVariantFailure{Variant: variant, Error: err.Error()})
						continue
					}
					results[index].images = append(results[index].images, ImageData{B64JSON: encoded, Variant: variant})
				}
			})
		}
		workers.Wait()
		response := &Response{Model: model.ID}
		generationFailures := true
		for index, result := range results {
			response.Data = append(response.Data, result.images...)
			response.Failures = append(response.Failures, result.failures...)
			if result.err != nil {
				var failure *providertransport.ImageWorkflowError
				if !errors.As(result.err, &failure) || failure.Phase != "generation" {
					generationFailures = false
				}
				first, last := index+1, index+1
				if value.VariantMode == "batch" {
					first, last = len(result.images)+1, request.Count
				}
				for variant := first; variant <= last; variant++ {
					response.Failures = append(response.Failures, ImageVariantFailure{Variant: variant, Error: result.err.Error()})
				}
			} else {
				generationFailures = false
			}
		}
		return response, generationFailures
	}
	response, generationFailures := run(model)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(response.Data) == 0 && generationFailures && model.Fallback != nil && model.Fallback.WhenAllGenerationRequestsFail {
		if next, ok := fallback(model); ok {
			response, _ = run(next)
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(response.Data) == 0 {
		return response, fmt.Errorf("image plugin produced no outputs: %d failed variants", len(response.Failures))
	}
	return response, nil
}
