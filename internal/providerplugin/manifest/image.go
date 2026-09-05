package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/invopop/jsonschema"
	validator "github.com/kaptinlin/jsonschema"
)

const (
	PluginTypeImageProvider = "image-provider"
	ImageSchemaID           = "https://raw.githubusercontent.com/example-git/crux/main/image-provider-plugin.schema.json"
)

var compiledImageSchema = sync.OnceValues(func() (*validator.Schema, error) {
	data, err := ImageSchemaJSON()
	if err != nil {
		return nil, err
	}
	return validator.NewCompiler().Compile(data)
})

func ImageSchemaIssuePaths(data []byte) ([]string, error) {
	schema, err := compiledImageSchema()
	if err != nil {
		return nil, err
	}
	return schemaIssuePaths(schema.ValidateJSON(data)), nil
}

type ImageManifest struct {
	Schema           string                            `json:"$schema,omitempty" jsonschema:"format=uri-reference"`
	PluginType       string                            `json:"plugin_type" jsonschema:"required,enum=image-provider"`
	ManifestVersion  int                               `json:"manifest_version" jsonschema:"required,minimum=1,maximum=1"`
	ID               string                            `json:"id" jsonschema:"required,maxLength=128"`
	Version          string                            `json:"version" jsonschema:"required,maxLength=128"`
	Name             string                            `json:"name" jsonschema:"required,minLength=1,maxLength=128"`
	Description      string                            `json:"description" jsonschema:"required,minLength=1,maxLength=1024"`
	Publisher        Publisher                         `json:"publisher" jsonschema:"required"`
	Compatibility    Compatibility                     `json:"compatibility" jsonschema:"required"`
	Backend          string                            `json:"backend" jsonschema:"required,maxLength=64"`
	Configuration    Configuration                     `json:"configuration" jsonschema:"required"`
	Credentials      []ImageCredential                 `json:"credentials,omitempty" jsonschema:"maxItems=16"`
	Origins          []ImageOrigin                     `json:"origins" jsonschema:"required,minItems=1,maxItems=32"`
	Models           []ImageModel                      `json:"models" jsonschema:"required,minItems=1,maxItems=128"`
	ClientIdentities map[string]ResolvedClientIdentity `json:"client_identities,omitempty" jsonschema:"maxProperties=16"`
	DefaultModel     string                            `json:"default_model" jsonschema:"required,maxLength=128"`
	Options          ImageOptions                      `json:"options" jsonschema:"required"`
	Limits           ImageLimits                       `json:"limits" jsonschema:"required"`
	Session          string                            `json:"session,omitempty" jsonschema:"maxLength=64"`
	Discovery        string                            `json:"discovery,omitempty" jsonschema:"maxLength=64"`
	Upload           *ImageUpload                      `json:"upload,omitempty"`
	Generate         string                            `json:"generate" jsonschema:"required,maxLength=64"`
	Edit             string                            `json:"edit,omitempty" jsonschema:"maxLength=64"`
	VariantMode      string                            `json:"variant_mode" jsonschema:"required,enum=individual,enum=batch"`
	Workflows        map[string]ImageWorkflow          `json:"workflows" jsonschema:"required,minProperties=1,maxProperties=64"`
}

type ImageCredential struct {
	ID          string   `json:"id" jsonschema:"required,maxLength=64"`
	Source      string   `json:"source" jsonschema:"required,enum=environment,enum=provider,enum=browser"`
	Environment string   `json:"environment,omitempty" jsonschema:"maxLength=128"`
	Provider    string   `json:"provider,omitempty" jsonschema:"maxLength=64"`
	Domains     []string `json:"domains,omitempty" jsonschema:"maxItems=16,uniqueItems=true"`
}

type ImageOrigin struct {
	ProviderCredential string   `json:"provider_credential,omitempty" jsonschema:"maxLength=64"`
	URL                string   `json:"url" jsonschema:"required,format=uri,maxLength=2048"`
	Subdomains         bool     `json:"subdomains,omitempty"`
	Credentials        []string `json:"credentials,omitempty" jsonschema:"maxItems=16,uniqueItems=true"`
}

type ImageModel struct {
	ID              string         `json:"id" jsonschema:"required,maxLength=128"`
	Name            string         `json:"name" jsonschema:"required,maxLength=128"`
	AvailabilityKey string         `json:"availability_key,omitempty" jsonschema:"maxLength=128"`
	Parameters      map[string]any `json:"parameters,omitempty"`
	Fallback        *ImageFallback `json:"fallback,omitempty"`
}

type ImageFallback struct {
	Model                         string `json:"model" jsonschema:"required,maxLength=128"`
	WhenUnavailable               bool   `json:"when_unavailable,omitempty"`
	WhenAllGenerationRequestsFail bool   `json:"when_all_generation_requests_fail,omitempty"`
}

type ImageDimensionLimits struct {
	Multiple  int   `json:"multiple" jsonschema:"required,minimum=1,maximum=16384"`
	MaxEdge   int   `json:"max_edge" jsonschema:"required,minimum=1,maximum=16384"`
	MinPixels int64 `json:"min_pixels" jsonschema:"required,minimum=1,maximum=268435456"`
	MaxPixels int64 `json:"max_pixels" jsonschema:"required,minimum=1,maximum=268435456"`
	MaxAspect int   `json:"max_aspect" jsonschema:"required,minimum=1,maximum=16384"`
}

type ImageOptions struct {
	AspectRatios    []string              `json:"aspect_ratios,omitempty" jsonschema:"maxItems=64,uniqueItems=true"`
	DimensionLimits *ImageDimensionLimits `json:"dimension_limits,omitempty"`
	Quality         []string              `json:"quality" jsonschema:"required,minItems=1,maxItems=16,uniqueItems=true"`
	Background      []string              `json:"background" jsonschema:"required,minItems=1,maxItems=16,uniqueItems=true"`
	Sizes           []string              `json:"sizes,omitempty" jsonschema:"maxItems=64,uniqueItems=true"`
	Dimensions      bool                  `json:"dimensions,omitempty"`
	OutputExtension string                `json:"output_extension" jsonschema:"required,enum=.png,enum=.jpg,enum=.webp"`
}

type ImageLimits struct {
	Requests           int   `json:"requests,omitempty" jsonschema:"minimum=1,maximum=4096,default=512"`
	Steps              int   `json:"steps,omitempty" jsonschema:"minimum=1,maximum=65536,default=4096"`
	TotalResponseBytes int64 `json:"total_response_bytes,omitempty" jsonschema:"minimum=1,maximum=1073741824,default=1073741824"`
	Concurrency        int   `json:"concurrency" jsonschema:"required,minimum=1,maximum=4"`
	Variants           int   `json:"variants" jsonschema:"required,minimum=1,maximum=10"`
	InputImages        int   `json:"input_images" jsonschema:"required,minimum=0,maximum=16"`
	InputBytes         int64 `json:"input_bytes" jsonschema:"required,minimum=1,maximum=52428800"`
	TotalInputBytes    int64 `json:"total_input_bytes" jsonschema:"required,minimum=1,maximum=209715200"`
	OutputBytes        int64 `json:"output_bytes" jsonschema:"required,minimum=1,maximum=104857600"`
	ResponseBytes      int64 `json:"response_bytes" jsonschema:"required,minimum=1,maximum=536870912"`
	TimeoutSeconds     int   `json:"timeout_seconds" jsonschema:"required,minimum=1,maximum=600"`
}

type ImageUpload struct {
	Workflow   string      `json:"workflow" jsonschema:"required,maxLength=64"`
	Lookup     string      `json:"lookup,omitempty" jsonschema:"maxLength=64"`
	CacheKey   *ImageValue `json:"cache_key,omitempty"`
	Persistent bool        `json:"persistent,omitempty"`
}

type ImageWorkflow struct {
	Steps  []ImageStep `json:"steps" jsonschema:"required,minItems=1,maxItems=64"`
	Result ImageValue  `json:"result" jsonschema:"required"`
}

type ImageStep struct {
	ID       string                `json:"id" jsonschema:"required,maxLength=64"`
	Request  *ImageRequest         `json:"request,omitempty"`
	Value    *ImageValue           `json:"value,omitempty"`
	Assert   *ImageValue           `json:"assert,omitempty"`
	Call     string                `json:"call,omitempty" jsonschema:"maxLength=64"`
	Bindings map[string]ImageValue `json:"bindings,omitempty" jsonschema:"maxProperties=32"`
}

type ImageRequest struct {
	AcceptedMediaTypes []string              `json:"accepted_media_types,omitempty" jsonschema:"minItems=1,maxItems=16,uniqueItems=true"`
	Method             string                `json:"method" jsonschema:"required,enum=GET,enum=POST,enum=PUT"`
	URL                ImageValue            `json:"url" jsonschema:"required"`
	Headers            map[string]ImageValue `json:"headers,omitempty" jsonschema:"maxProperties=64"`
	Query              map[string]ImageValue `json:"query,omitempty" jsonschema:"maxProperties=64"`
	Body               *ImageValue           `json:"body,omitempty"`
	Encoding           string                `json:"encoding" jsonschema:"required,enum=none,enum=json,enum=form,enum=multipart,enum=binary"`
	Response           string                `json:"response" jsonschema:"required,enum=json,enum=text,enum=binary,enum=framed-json"`
	FramePrefix        string                `json:"frame_prefix,omitempty" jsonschema:"maxLength=128"`
	Phase              string                `json:"phase" jsonschema:"required,enum=setup,enum=upload,enum=generation,enum=media,enum=download"`
	MaxBytes           int64                 `json:"max_bytes" jsonschema:"required,minimum=1,maximum=536870912"`
	TimeoutSeconds     int                   `json:"timeout_seconds" jsonschema:"required,minimum=1,maximum=600"`
	Retry              *ImageRetry           `json:"retry,omitempty"`
}

type ImageRetry struct {
	Attempts int   `json:"attempts" jsonschema:"required,minimum=1,maximum=10"`
	DelayMS  int   `json:"delay_ms" jsonschema:"required,minimum=0,maximum=60000"`
	Statuses []int `json:"statuses" jsonschema:"required,minItems=1,maxItems=32,uniqueItems=true"`
}

type ImageValue struct {
	Literal json.RawMessage       `json:"literal,omitempty"`
	Ref     string                `json:"ref,omitempty" jsonschema:"maxLength=1024"`
	Op      string                `json:"op,omitempty" jsonschema:"maxLength=64"`
	Args    []ImageValue          `json:"args,omitempty" jsonschema:"maxItems=64"`
	Object  map[string]ImageValue `json:"object,omitempty" jsonschema:"maxProperties=128"`
	Array   []ImageValue          `json:"array,omitempty" jsonschema:"maxItems=256"`
}

func (v ImageValue) MarshalJSON() ([]byte, error) {
	type plain ImageValue
	return json.Marshal(struct {
		plain
		Object *map[string]ImageValue `json:"object,omitempty"`
		Array  *[]ImageValue          `json:"array,omitempty"`
	}{
		plain: plain(v),
		Object: func() *map[string]ImageValue {
			if v.Object == nil {
				return nil
			}
			return &v.Object
		}(),
		Array: func() *[]ImageValue {
			if v.Array == nil {
				return nil
			}
			return &v.Array
		}(),
	})
}

func ImageSchema() *jsonschema.Schema {
	reflector := &jsonschema.Reflector{RequiredFromJSONSchemaTags: true}
	schema := reflector.Reflect(&ImageManifest{})
	schema.ID = jsonschema.ID(ImageSchemaID)
	schema.Title = "Crux Image Provider Plugin Manifest"
	return schema
}

func ImageSchemaJSON() ([]byte, error) {
	data, err := json.MarshalIndent(ImageSchema(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func DecodeImageStrict(data []byte) (ImageManifest, error) {
	if len(data) > MaxManifestBytes {
		return ImageManifest{}, errors.New("image manifest exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var value ImageManifest
	if err := decoder.Decode(&value); err != nil {
		return ImageManifest{}, errors.New("invalid image manifest JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ImageManifest{}, errors.New("image manifest contains trailing data")
	}
	paths, err := ImageSchemaIssuePaths(data)
	if err != nil {
		return ImageManifest{}, errors.New("image manifest schema is unavailable")
	}
	if len(paths) != 0 {
		return ImageManifest{}, fmt.Errorf("invalid image manifest schema at %s", strings.Join(paths, ", "))
	}
	if err := ValidateImage(value); err != nil {
		return ImageManifest{}, err
	}
	return value, nil
}

func ValidateImage(value ImageManifest) error {
	var errs []error
	add := func(path string) { errs = append(errs, fmt.Errorf("invalid image manifest declaration at %s", path)) }
	if value.PluginType != PluginTypeImageProvider || value.ManifestVersion != Version {
		add("/plugin_type")
	}
	if !pluginIDPattern.MatchString(value.ID) || len(value.ID) > 128 {
		add("/id")
	}
	if !validSemver(value.Version) {
		add("/version")
	}
	if !pluginIDPattern.MatchString(value.Publisher.ID) || strings.TrimSpace(value.Publisher.Name) == "" {
		add("/publisher")
	}
	if strings.TrimSpace(value.Description) == "" || len(value.Description) > 1024 {
		add("/description")
	}
	if value.Compatibility.HostAPI.Min < 1 || value.Compatibility.HostAPI.Max < value.Compatibility.HostAPI.Min {
		add("/compatibility/host_api")
	}
	configuration, err := json.Marshal(value.Configuration.Schema)
	if err != nil || value.Configuration.Schema == nil {
		add("/configuration/schema")
	} else if _, err := validator.NewCompiler().Compile(configuration); err != nil {
		add("/configuration/schema")
	}
	if strings.TrimSpace(value.Name) == "" || len(value.Name) > 128 {
		add("/name")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`).MatchString(value.Backend) || value.Backend == "auto" {
		add("/backend")
	}
	if len(value.ClientIdentities) > 16 {
		add("/client_identities")
	}
	for id, identity := range value.ClientIdentities {
		if !imageIdentifier(id) {
			add("/client_identities")
		}
		validateClientIdentity("client_identities", &identity, func(string, ...any) { add("/client_identities") })
	}
	credentials := map[string]bool{}
	for _, credential := range value.Credentials {
		if !imageIdentifier(credential.ID) || credentials[credential.ID] {
			add("/credentials")
		}
		credentials[credential.ID] = true
		switch credential.Source {
		case "environment":
			if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(credential.Environment) || credential.Provider != "" || len(credential.Domains) != 0 {
				add("/credentials")
			}
		case "provider":
			if credential.Provider == "" || credential.Environment != "" || len(credential.Domains) != 0 {
				add("/credentials")
			}
		case "browser":
			if len(credential.Domains) == 0 || credential.Provider != "" || credential.Environment != "" {
				add("/credentials")
			}
		default:
			add("/credentials")
		}
	}
	if len(value.Origins) == 0 || len(value.Origins) > 32 {
		add("/origins")
	}
	for _, origin := range value.Origins {
		if origin.ProviderCredential != "" {
			index := slices.IndexFunc(value.Credentials, func(c ImageCredential) bool { return c.ID == origin.ProviderCredential && c.Source == "provider" })
			if index < 0 || origin.Subdomains || len(origin.Credentials) != 1 || origin.Credentials[0] != origin.ProviderCredential {
				add("/origins/provider_credential")
			}
		}
		parsed, err := url.Parse(origin.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			add("/origins")
		}
		for _, id := range origin.Credentials {
			if !credentials[id] {
				add("/origins/credentials")
			}
		}
	}
	if dimensions := value.Options.DimensionLimits; dimensions != nil {
		if !value.Options.Dimensions || dimensions.Multiple < 1 || dimensions.Multiple > 16384 || dimensions.MaxEdge < 1 || dimensions.MaxEdge > 16384 || dimensions.MinPixels < 1 || dimensions.MaxPixels < dimensions.MinPixels || dimensions.MaxPixels > 268435456 || dimensions.MaxAspect < 1 || dimensions.MaxAspect > 16384 {
			add("/options/dimension_limits")
		}
	}
	models := map[string]ImageModel{}
	for _, model := range value.Models {
		if model.ID == "" || model.Name == "" {
			add("/models")
		}
		if _, exists := models[model.ID]; exists {
			add("/models")
		}
		models[model.ID] = model
	}
	if len(models) == 0 || len(models) > 128 {
		add("/models")
	}
	if _, ok := models[value.DefaultModel]; !ok {
		add("/default_model")
	}
	for _, model := range value.Models {
		if fallback := model.Fallback; fallback != nil {
			target, ok := models[fallback.Model]
			if !ok || target.ID == model.ID || target.Fallback != nil || (!fallback.WhenUnavailable && !fallback.WhenAllGenerationRequestsFail) {
				add("/models/fallback")
			}
		}
	}
	if value.VariantMode != "individual" && value.VariantMode != "batch" {
		add("/variant_mode")
	}
	if !slices.Contains([]string{".png", ".jpg", ".webp"}, value.Options.OutputExtension) || len(value.Options.Quality) == 0 || len(value.Options.Background) == 0 {
		add("/options")
	}
	if len(value.Options.AspectRatios) > 64 || len(value.Options.AspectRatios) > 0 && !value.Options.Dimensions {
		add("/options/aspect_ratios")
	}
	seenRatios := map[string]bool{}
	for _, ratio := range value.Options.AspectRatios {
		if normalized, err := ImageAspectRatio(strings.ReplaceAll(ratio, ":", "x")); err != nil || normalized != ratio || seenRatios[ratio] {
			add("/options/aspect_ratios")
		}
		seenRatios[ratio] = true
	}
	limits := value.Limits
	if limits.Concurrency < 1 || limits.Concurrency > 4 || limits.Variants < 1 || limits.Variants > 10 || limits.InputImages < 0 || limits.InputImages > 16 || limits.InputBytes < 1 || limits.InputBytes > 50<<20 || limits.TotalInputBytes < limits.InputBytes || limits.TotalInputBytes > 200<<20 || limits.OutputBytes < 1 || limits.OutputBytes > 100<<20 || limits.ResponseBytes < 1 || limits.ResponseBytes > 512<<20 || limits.TimeoutSeconds < 1 || limits.TimeoutSeconds > 600 {
		add("/limits")
	}
	checkWorkflow := func(id, path string, required bool) {
		if id == "" && !required {
			return
		}
		if _, ok := value.Workflows[id]; !ok {
			add(path)
		}
	}
	checkWorkflow(value.Generate, "/generate", true)
	checkWorkflow(value.Edit, "/edit", false)
	checkWorkflow(value.Session, "/session", false)
	checkWorkflow(value.Discovery, "/discovery", false)
	if value.Upload != nil {
		checkWorkflow(value.Upload.Workflow, "/upload/workflow", true)
		checkWorkflow(value.Upload.Lookup, "/upload/lookup", false)
		if value.Upload.CacheKey != nil {
			validateImageValue(*value.Upload.CacheKey, 0, add)
		}
		if (value.Upload.CacheKey == nil) != (value.Upload.Lookup == "") || value.Upload.Persistent && value.Upload.CacheKey == nil {
			add("/upload")
		}
	}
	for id, workflow := range value.Workflows {
		if !imageIdentifier(id) || len(workflow.Steps) == 0 || len(workflow.Steps) > 64 {
			add("/workflows")
		}
		seen := map[string]bool{}
		for _, step := range workflow.Steps {
			if !imageIdentifier(step.ID) || seen[step.ID] {
				add("/workflows/steps/id")
			}
			seen[step.ID] = true
			count := 0
			if step.Value != nil {
				count++
				validateImageValue(*step.Value, 0, add)
			}
			if step.Assert != nil {
				count++
				validateImageValue(*step.Assert, 0, add)
			}
			if step.Call != "" {
				count++
				checkWorkflow(step.Call, "/workflows/steps/call", true)
			}
			for _, binding := range step.Bindings {
				validateImageValue(binding, 0, add)
			}
			if len(step.Bindings) > 0 && step.Call == "" {
				add("/workflows/steps/bindings")
			}
			if request := step.Request; request != nil {
				count++
				if !slices.Contains([]string{"GET", "POST", "PUT"}, request.Method) || !slices.Contains([]string{"none", "json", "form", "multipart", "binary"}, request.Encoding) || !slices.Contains([]string{"json", "text", "binary", "framed-json"}, request.Response) || !slices.Contains([]string{"setup", "upload", "generation", "media", "download"}, request.Phase) {
					add("/workflows/steps/request")
				}
				if request.MaxBytes < 1 || request.MaxBytes > limits.ResponseBytes || request.TimeoutSeconds < 1 || request.TimeoutSeconds > limits.TimeoutSeconds {
					add("/workflows/steps/request/limits")
				}
				if (request.Encoding == "none") != (request.Body == nil) || (request.FramePrefix != "" && request.Response != "framed-json") {
					add("/workflows/steps/request/encoding")
				}
				seenMedia := map[string]bool{}
				for _, media := range request.AcceptedMediaTypes {
					if !regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`).MatchString(media) || len(media) > 128 || seenMedia[media] {
						add("/workflows/steps/request/accepted_media_types")
					}
					seenMedia[media] = true
				}
				if len(request.AcceptedMediaTypes) > 16 {
					add("/workflows/steps/request/accepted_media_types")
				}
				validateImageValue(request.URL, 0, add)
				if request.Body != nil {
					validateImageValue(*request.Body, 0, add)
				}
				for _, field := range request.Headers {
					validateImageValue(field, 0, add)
				}
				for _, field := range request.Query {
					validateImageValue(field, 0, add)
				}
				if retry := request.Retry; retry != nil {
					if retry.Attempts < 1 || retry.Attempts > 10 || retry.DelayMS < 0 || retry.DelayMS > 60000 || len(retry.Statuses) == 0 || len(retry.Statuses) > 32 {
						add("/workflows/steps/request/retry")
					}
					for _, status := range retry.Statuses {
						if status < 400 || status > 599 {
							add("/workflows/steps/request/retry/statuses")
						}
					}
				}
			}
			if count != 1 {
				add("/workflows/steps")
			}
		}
		validateImageValue(workflow.Result, 0, add)
	}
	if limits.Requests < 0 || limits.Requests > 4096 || limits.Steps < 0 || limits.Steps > 65536 || limits.TotalResponseBytes < 0 || limits.TotalResponseBytes > 1073741824 {
		add("/limits")
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if visiting[id] {
			add("/workflows/cycle")
			return
		}
		if visited[id] {
			return
		}
		visiting[id] = true
		for _, step := range value.Workflows[id].Steps {
			if step.Call != "" {
				visit(step.Call)
			}
		}
		visiting[id] = false
		visited[id] = true
	}
	for id := range value.Workflows {
		visit(id)
	}
	validateImageReferences(value, add)
	return errors.Join(errs...)
}

func imageIdentifier(value string) bool {
	return regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(value)
}

func validateImageValue(value ImageValue, depth int, add func(string)) {
	if depth > 32 {
		add("/workflows/value/depth")
		return
	}
	count := 0
	if value.Literal != nil {
		count++
		if !json.Valid(value.Literal) {
			add("/workflows/value/literal")
		}
	}
	if value.Ref != "" {
		count++
		if !strings.HasPrefix(value.Ref, "/") {
			add("/workflows/value/ref")
		}
	}
	if value.Object != nil {
		count++
		for _, child := range value.Object {
			validateImageValue(child, depth+1, add)
		}
	}
	if value.Array != nil {
		count++
		for _, child := range value.Array {
			validateImageValue(child, depth+1, add)
		}
	}
	if value.Op != "" {
		count++
		arity := map[string]int{"if": 3, "optional": 1, "omit": 0, "add": 2, "less": 2, "json": 1, "parse-json": 1, "base64": 1, "base64-decode": 1, "base64url-decode": 1, "data-url": 2, "replace": 3, "regexp": 3, "html-unescape": 1, "uuid": 0, "random": 1, "get": 2, "map": 2, "flatten": 1, "filter": 2, "equal": 2, "not": 1, "length": 1, "integer": 1, "first": 1}
		if expected, ok := arity[value.Op]; ok && len(value.Args) != expected {
			add("/workflows/value/args")
		}
		if slices.Contains([]string{"concat", "and", "or", "coalesce"}, value.Op) && len(value.Args) == 0 {
			add("/workflows/value/args")
		}
		if value.Op == "regexp" && len(value.Args) == 3 {
			var pattern string
			if json.Unmarshal(value.Args[1].Literal, &pattern) != nil || len(pattern) > 4096 {
				add("/workflows/value/regexp")
			} else if _, err := regexp.Compile(pattern); err != nil {
				add("/workflows/value/regexp")
			}
		}
		if !slices.Contains([]string{"if", "optional", "omit", "add", "less", "json", "parse-json", "base64", "base64-decode", "base64url-decode", "data-url", "concat", "replace", "regexp", "html-unescape", "uuid", "random", "get", "map", "flatten", "filter", "equal", "and", "or", "not", "length", "integer", "first", "coalesce"}, value.Op) {
			add("/workflows/value/op")
		}
		for _, child := range value.Args {
			validateImageValue(child, depth+1, add)
		}
	} else if len(value.Args) > 0 {
		add("/workflows/value/args")
	}
	if count != 1 {
		add("/workflows/value")
	}
}
