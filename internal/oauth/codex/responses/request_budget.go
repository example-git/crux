package responses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
	"strings"

	"github.com/example-git/crux/internal/imageutil"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	_ "golang.org/x/image/webp"
)

const (
	maxCodexRequestBytes      = 14 * 1024 * 1024
	retryCodexRequestBytes    = 10 * 1024 * 1024
	codexRequestImageBytes    = 512 * 1024
	initialBudgetImageBytes   = 256 * 1024
	minimumBudgetImageBytes   = 64 * 1024
	omittedImageBudgetMessage = "[Earlier image omitted to keep the Codex request within its message size limit]"
)

type requestBudget struct {
	requestBytes       int
	retryRequestBytes  int
	perImageTargets    []int
	omitOldImages      bool
	retainNewestImage  bool
	acceptedMediaTypes map[string]struct{}
	maxSourceBytes     int
	maxSidePixels      int
	maxOutputBytes     int
	maxPatches         int
	outputMediaType    string
	flattenAlpha       string
	qualitySteps       []int
	resizePercent      int
}

func defaultImagePolicyDeclaration() *manifest.ImagePolicy {
	return &manifest.ImagePolicy{
		AcceptedMediaTypes: []string{"image/gif", "image/jpeg", "image/png", "image/webp"},
		MaxSourceBytes:     25 * 1024 * 1024,
		MaxSidePixels:      1920,
		MaxOutputBytes:     codexRequestImageBytes,
		MaxPatches:         2500,
		OutputMediaType:    "image/jpeg",
		FlattenAlpha:       "white",
		QualitySteps:       []int{85, 75, 65, 55, 45, 35, 25},
		ResizePercent:      80,
		HistoryBudget: &manifest.ImageHistoryBudget{
			RequestBytes:      maxCodexRequestBytes,
			RetryRequestBytes: retryCodexRequestBytes,
			PerImageTargets:   []int64{codexRequestImageBytes, initialBudgetImageBytes, minimumBudgetImageBytes},
			OmitOldImages:     true,
			RetainNewestImage: true,
		},
	}
}

func compileRequestBudget(value *manifest.ImagePolicy) (requestBudget, error) {
	if value == nil || value.HistoryBudget == nil {
		return requestBudget{}, fmt.Errorf("Codex image history policy is unavailable")
	}
	maxSourceBytes, err := requestBudgetBytes("max_source_bytes", value.MaxSourceBytes, false)
	if err != nil {
		return requestBudget{}, err
	}
	maxOutputBytes, err := requestBudgetBytes("max_output_bytes", value.MaxOutputBytes, false)
	if err != nil {
		return requestBudget{}, err
	}
	if value.MaxSidePixels < 1 {
		return requestBudget{}, fmt.Errorf("Codex image history max_side_pixels is outside the executable range")
	}
	if value.MaxPatches < 1 {
		return requestBudget{}, fmt.Errorf("Codex image history max_patches is outside the executable range")
	}
	if value.OutputMediaType != "image/jpeg" {
		return requestBudget{}, fmt.Errorf("Codex image history output_media_type must be image/jpeg")
	}
	if value.FlattenAlpha != "white" && value.FlattenAlpha != "black" {
		return requestBudget{}, fmt.Errorf("Codex image history flatten_alpha must be white or black")
	}
	if len(value.QualitySteps) == 0 {
		return requestBudget{}, fmt.Errorf("Codex image history quality_steps must be non-empty")
	}
	if value.ResizePercent < 1 || value.ResizePercent > 99 {
		return requestBudget{}, fmt.Errorf("Codex image history resize_percent must be between 1 and 99")
	}
	budget := requestBudget{
		acceptedMediaTypes: make(map[string]struct{}, len(value.AcceptedMediaTypes)),
		maxSourceBytes:     maxSourceBytes,
		maxSidePixels:      value.MaxSidePixels,
		maxOutputBytes:     maxOutputBytes,
		maxPatches:         value.MaxPatches,
		outputMediaType:    value.OutputMediaType,
		flattenAlpha:       value.FlattenAlpha,
		qualitySteps:       append([]int(nil), value.QualitySteps...),
		resizePercent:      value.ResizePercent,
	}
	for _, mediaType := range value.AcceptedMediaTypes {
		switch mediaType {
		case "image/gif", "image/jpeg", "image/png", "image/webp":
		default:
			return requestBudget{}, fmt.Errorf("Codex image history accepted media type %q is unsupported", mediaType)
		}
		if _, exists := budget.acceptedMediaTypes[mediaType]; exists {
			return requestBudget{}, fmt.Errorf("Codex image history accepted media type %q is duplicated", mediaType)
		}
		budget.acceptedMediaTypes[mediaType] = struct{}{}
	}
	if _, exists := budget.acceptedMediaTypes[value.OutputMediaType]; !exists {
		return requestBudget{}, fmt.Errorf("Codex image history output media type %q is not accepted", value.OutputMediaType)
	}
	qualities := make(map[int]struct{}, len(value.QualitySteps))
	for _, quality := range value.QualitySteps {
		if quality < 1 || quality > 100 {
			return requestBudget{}, fmt.Errorf("Codex image history quality step %d is outside the executable range", quality)
		}
		if _, exists := qualities[quality]; exists {
			return requestBudget{}, fmt.Errorf("Codex image history quality step %d is duplicated", quality)
		}
		qualities[quality] = struct{}{}
	}
	history := value.HistoryBudget
	requestBytes, err := requestBudgetBytes("request_bytes", history.RequestBytes, false)
	if err != nil {
		return requestBudget{}, err
	}
	retryRequestBytes, err := requestBudgetBytes("retry_request_bytes", history.RetryRequestBytes, true)
	if err != nil {
		return requestBudget{}, err
	}
	budget.requestBytes = requestBytes
	budget.retryRequestBytes = retryRequestBytes
	budget.omitOldImages = history.OmitOldImages
	budget.retainNewestImage = history.RetainNewestImage
	budget.perImageTargets = make([]int, len(history.PerImageTargets))
	seen := make(map[int64]struct{}, len(history.PerImageTargets))
	for index, target := range history.PerImageTargets {
		if _, exists := seen[target]; exists {
			return requestBudget{}, fmt.Errorf("Codex image history per_image_targets contains duplicate target %d", target)
		}
		seen[target] = struct{}{}
		compiled, compileErr := requestBudgetBytes("per_image_targets", target, false)
		if compileErr != nil {
			return requestBudget{}, compileErr
		}
		budget.perImageTargets[index] = compiled
	}
	if budget.retainNewestImage && !budget.omitOldImages {
		return requestBudget{}, fmt.Errorf("Codex image history retain_newest_image requires omit_old_images")
	}
	return budget, nil
}

func requestBudgetBytes(field string, value int64, optional bool) (int, error) {
	if optional && value == 0 {
		return 0, nil
	}
	if value < 1 || uint64(value) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("Codex image history %s is outside the executable range", field)
	}
	return int(value), nil
}

func (b requestBudget) clone() requestBudget {
	b.perImageTargets = append([]int(nil), b.perImageTargets...)
	b.qualitySteps = append([]int(nil), b.qualitySteps...)
	acceptedMediaTypes := b.acceptedMediaTypes
	b.acceptedMediaTypes = make(map[string]struct{}, len(acceptedMediaTypes))
	for mediaType := range acceptedMediaTypes {
		b.acceptedMediaTypes[mediaType] = struct{}{}
	}
	return b
}

type requestBudgetStats struct {
	originalBytes    int
	finalBytes       int
	compressedImages int
	omittedImages    int
}

type requestImageRef struct {
	content *messageContent
}

func fitCodexRequest(logical *requestFrame, budget requestBudget) (*requestFrame, requestBudgetStats, error) {
	return fitCodexRequestTo(logical, budget, budget.requestBytes)
}

func fitCodexRequestTo(logical *requestFrame, budget requestBudget, maximumBytes int) (*requestFrame, requestBudgetStats, error) {
	if maximumBytes < 1 {
		return nil, requestBudgetStats{}, fmt.Errorf("Codex image history request budget is unavailable")
	}
	bounded := cloneRequestFrame(logical)
	size, err := fullRequestSize(bounded)
	if err != nil {
		return nil, requestBudgetStats{}, err
	}
	stats := requestBudgetStats{originalBytes: size, finalBytes: size}
	images := requestImages(bounded)
	for targetIndex, target := range budget.perImageTargets {
		if targetIndex > 0 && size <= maximumBytes {
			break
		}
		compressedAtTarget := false
		for _, image := range images {
			changed, compressErr := compressRequestImage(image.content, target, targetIndex == 0, budget)
			if compressErr != nil {
				return nil, stats, compressErr
			}
			if !changed {
				continue
			}
			stats.compressedImages++
			compressedAtTarget = true
			if targetIndex > 0 {
				size, err = fullRequestSize(bounded)
				if err != nil {
					return nil, stats, err
				}
				if size <= maximumBytes {
					stats.finalBytes = size
					return bounded, stats, nil
				}
			}
		}
		if compressedAtTarget && targetIndex == 0 {
			size, err = fullRequestSize(bounded)
			if err != nil {
				return nil, stats, err
			}
			stats.finalBytes = size
		}
	}
	if size <= maximumBytes {
		return bounded, stats, nil
	}
	if len(images) == 0 || !budget.omitOldImages {
		return nil, stats, requestSizeError(size, maximumBytes)
	}
	omissionLimit := len(images)
	if budget.retainNewestImage {
		omissionLimit--
	}
	for index := 0; index < omissionLimit && size > maximumBytes; index++ {
		*images[index].content = messageContent{Type: "input_text", Text: omittedImageBudgetMessage}
		stats.omittedImages++
		size, err = fullRequestSize(bounded)
		if err != nil {
			return nil, stats, err
		}
	}
	stats.finalBytes = size
	if size > maximumBytes {
		return nil, stats, requestSizeError(size, maximumBytes)
	}
	return bounded, stats, nil
}

func fullRequestSize(logical *requestFrame) (int, error) {
	encoded, err := json.Marshal(fullWireRequest(logical))
	if err != nil {
		return 0, fmt.Errorf("serialize Codex request for size validation: %w", err)
	}
	return len(encoded), nil
}

func requestImages(frame *requestFrame) []requestImageRef {
	var images []requestImageRef
	for itemIndex := range frame.Input {
		for contentIndex := range frame.Input[itemIndex].Content {
			content := &frame.Input[itemIndex].Content[contentIndex]
			if content.Type == "input_image" && content.ImageURL != "" {
				images = append(images, requestImageRef{content: content})
			}
		}
	}
	return images
}

func compressRequestImage(content *messageContent, target int, forceJPEG bool, budget requestBudget) (bool, error) {
	mimeType, encoded, err := splitImageDataURL(content.ImageURL)
	if err != nil {
		return false, err
	}
	if _, accepted := budget.acceptedMediaTypes[mimeType]; !accepted {
		return false, fmt.Errorf("Codex request image media type %q is not accepted", mimeType)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false, err
	}
	if len(data) > budget.maxSourceBytes {
		return false, fmt.Errorf("Codex request image exceeds the declared source limit of %d bytes", budget.maxSourceBytes)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return false, fmt.Errorf("decode Codex request image metadata: %w", err)
	}
	if budget.maxOutputBytes < target {
		target = budget.maxOutputBytes
	}
	needsResize := max(config.Width, config.Height) > budget.maxSidePixels || requestImagePatchCount(config.Width, config.Height) > budget.maxPatches
	needsConversion := forceJPEG && mimeType != budget.outputMediaType
	if len(data) <= target && !needsResize && !needsConversion {
		return false, nil
	}
	compressed, err := compressImageToJPEG(data, target, budget)
	if err != nil {
		return false, fmt.Errorf("compress Codex request image: %w", err)
	}
	content.ImageURL = "data:" + budget.outputMediaType + ";base64," + base64.StdEncoding.EncodeToString(compressed)
	return true, nil
}

func compressImageToJPEG(data []byte, target int, budget requestBudget) ([]byte, error) {
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	background := color.White
	if budget.flattenAlpha == "black" {
		background = color.Black
	}
	bounds := decoded.Bounds()
	targetWidth, targetHeight := requestImageTargetDimensions(bounds.Dx(), bounds.Dy(), budget)
	if targetWidth != bounds.Dx() || targetHeight != bounds.Dy() {
		decoded = imageutil.Resize(decoded, targetWidth, targetHeight)
		bounds = decoded.Bounds()
	}
	flattened := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(flattened, flattened.Bounds(), &image.Uniform{C: background}, image.Point{}, draw.Src)
	draw.Draw(flattened, flattened.Bounds(), decoded, bounds.Min, draw.Over)
	var current image.Image = flattened
	for {
		for _, quality := range budget.qualitySteps {
			var output bytes.Buffer
			if err := jpeg.Encode(&output, current, &jpeg.Options{Quality: quality}); err != nil {
				return nil, err
			}
			if output.Len() <= target {
				return output.Bytes(), nil
			}
		}
		bounds = current.Bounds()
		if bounds.Dx() <= 1 && bounds.Dy() <= 1 {
			return nil, fmt.Errorf("image cannot be reduced below %d bytes", target)
		}
		current = imageutil.Resize(current, max(1, bounds.Dx()*budget.resizePercent/100), max(1, bounds.Dy()*budget.resizePercent/100))
	}
}

func requestImageTargetDimensions(width, height int, budget requestBudget) (int, int) {
	scale := min(1.0, float64(budget.maxSidePixels)/float64(max(width, height)))
	patches := requestImagePatchCount(width, height)
	if patches > budget.maxPatches {
		scale = min(scale, math.Sqrt(float64(budget.maxPatches)/float64(patches)))
	}
	targetWidth := max(1, int(math.Floor(float64(width)*scale)))
	targetHeight := max(1, int(math.Floor(float64(height)*scale)))
	for requestImagePatchCount(targetWidth, targetHeight) > budget.maxPatches {
		targetWidth = max(1, targetWidth*99/100)
		targetHeight = max(1, targetHeight*99/100)
	}
	return targetWidth, targetHeight
}

func requestImagePatchCount(width, height int) int {
	return ((width + 31) / 32) * ((height + 31) / 32)
}

func splitImageDataURL(value string) (string, string, error) {
	metadata, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(metadata, "data:image/") || !strings.HasSuffix(metadata, ";base64") {
		return "", "", fmt.Errorf("invalid Codex image data URL")
	}
	mimeType := strings.TrimSuffix(strings.TrimPrefix(metadata, "data:"), ";base64")
	return mimeType, encoded, nil
}

func requestSizeError(size, maximumBytes int) error {
	return fmt.Errorf("Codex request is too large after image compression (%d bytes; safe maximum is %d bytes); compact the session before retrying", size, maximumBytes)
}
