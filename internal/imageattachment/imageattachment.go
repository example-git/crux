package imageattachment

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
	"path/filepath"
	"strings"

	"github.com/disintegration/imaging"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	_ "golang.org/x/image/webp"
)

const (
	DefaultMaxSourceBytes = int64(5 * 1024 * 1024)
	MaxSourceBytes        = int64(25 * 1024 * 1024)
)

type Policy struct {
	Extensions      []string
	MIMETypes       map[string]bool
	MaxSourceBytes  int64
	MaxSide         int
	MaxPatches      int
	MaxRawBytes     int
	OutputMediaType string
	FlattenAlpha    string
	QualitySteps    []int
	ResizePercent   int
}

func policyFromDeclaration(value *manifest.ImagePolicy) (Policy, bool) {
	if value == nil {
		return Policy{}, false
	}
	policy := Policy{
		MIMETypes:      make(map[string]bool, len(value.AcceptedMediaTypes)),
		MaxSourceBytes: value.MaxSourceBytes, MaxSide: value.MaxSidePixels,
		MaxPatches: value.MaxPatches, MaxRawBytes: int(value.MaxOutputBytes),
		OutputMediaType: value.OutputMediaType, FlattenAlpha: value.FlattenAlpha,
		QualitySteps: append([]int(nil), value.QualitySteps...), ResizePercent: value.ResizePercent,
	}
	for _, mediaType := range value.AcceptedMediaTypes {
		policy.MIMETypes[mediaType] = true
		policy.Extensions = append(policy.Extensions, extensionsForMediaType(mediaType)...)
	}
	if len(policy.QualitySteps) == 0 {
		policy.QualitySteps = []int{85, 75, 65, 55, 45, 35, 25}
	}
	if policy.ResizePercent == 0 {
		policy.ResizePercent = 80
	}
	return policy, true
}

func imagePolicyFor(providerID string) *manifest.ImagePolicy {
	registration, ok := config.ProviderBehaviorCapabilities(providerID)
	if !ok {
		return nil
	}
	return registration.Images
}

func PolicyFor(providerID string) (Policy, bool) {
	return policyFromDeclaration(imagePolicyFor(providerID))
}

func SourceLimitFor(providerID string) int64 {
	if policy := imagePolicyFor(providerID); policy != nil {
		return policy.MaxSourceBytes
	}
	return DefaultMaxSourceBytes
}

func ExtensionsFor(providerID string) []string {
	if policy, ok := PolicyFor(providerID); ok {
		return append([]string(nil), policy.Extensions...)
	}
	return []string{".jpeg", ".jpg", ".png"}
}

func NormalizeAll(providerID string, attachments []message.Attachment) ([]message.Attachment, error) {
	policy, ok := PolicyFor(providerID)
	if !ok {
		return attachments, nil
	}
	result := make([]message.Attachment, len(attachments))
	for index, attachment := range attachments {
		normalized, err := Normalize(policy, attachment)
		if err != nil {
			return nil, fmt.Errorf("prepare image %q: %w", attachment.FileName, err)
		}
		result[index] = normalized
	}
	return result, nil
}

func Normalize(policy Policy, attachment message.Attachment) (message.Attachment, error) {
	if !attachment.IsImage() {
		return attachment, nil
	}
	if int64(len(attachment.Content)) > policy.MaxSourceBytes {
		return message.Attachment{}, fmt.Errorf("image exceeds the %d MiB source limit", policy.MaxSourceBytes/(1024*1024))
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(attachment.Content))
	if err != nil {
		return message.Attachment{}, fmt.Errorf("decode image metadata: %w", err)
	}
	mimeType := mimeForFormat(format)
	if !policy.MIMETypes[mimeType] {
		return message.Attachment{}, fmt.Errorf("image format %q is not supported by this provider", format)
	}
	attachment.MimeType = mimeType
	needsResize := policy.MaxSide > 0 && max(config.Width, config.Height) > policy.MaxSide
	needsPatchResize := policy.MaxPatches > 0 && patchCount(config.Width, config.Height) > policy.MaxPatches
	needsCompression := policy.MaxRawBytes > 0 && len(attachment.Content) > policy.MaxRawBytes
	needsConversion := policy.OutputMediaType != "" && mimeType != policy.OutputMediaType
	if !needsResize && !needsPatchResize && !needsCompression && !needsConversion {
		attachment.MimeType = mimeType
		return attachment, nil
	}

	decoded, _, err := image.Decode(bytes.NewReader(attachment.Content))
	if err != nil {
		return message.Attachment{}, fmt.Errorf("decode image: %w", err)
	}
	width, height := targetDimensions(config.Width, config.Height, policy)
	if width != config.Width || height != config.Height {
		decoded = imaging.Resize(decoded, width, height, imaging.Lanczos)
	}
	return encodeWithinPolicy(attachment, decoded, policy)
}

func targetDimensions(width, height int, policy Policy) (int, int) {
	scale := 1.0
	if policy.MaxSide > 0 && max(width, height) > policy.MaxSide {
		scale = min(scale, float64(policy.MaxSide)/float64(max(width, height)))
	}
	if policy.MaxPatches > 0 {
		patches := patchCount(width, height)
		if patches > policy.MaxPatches {
			scale = min(scale, math.Sqrt(float64(policy.MaxPatches)/float64(patches)))
		}
	}
	targetWidth := max(1, int(math.Floor(float64(width)*scale)))
	targetHeight := max(1, int(math.Floor(float64(height)*scale)))
	for policy.MaxPatches > 0 && patchCount(targetWidth, targetHeight) > policy.MaxPatches {
		targetWidth = max(1, targetWidth*99/100)
		targetHeight = max(1, targetHeight*99/100)
	}
	return targetWidth, targetHeight
}

func patchCount(width, height int) int {
	return ((width + 31) / 32) * ((height + 31) / 32)
}

func encodeWithinPolicy(attachment message.Attachment, value image.Image, policy Policy) (message.Attachment, error) {
	if policy.FlattenAlpha != "" && policy.FlattenAlpha != "none" {
		value = flattenAlpha(value, policy.FlattenAlpha)
	}
	encodeJPEG := policy.OutputMediaType == "image/jpeg" || (policy.OutputMediaType == "" && isOpaque(value))
	if encodeJPEG {
		current := value
		for {
			for _, quality := range policy.QualitySteps {
				var output bytes.Buffer
				if err := jpeg.Encode(&output, current, &jpeg.Options{Quality: quality}); err != nil {
					return message.Attachment{}, err
				}
				if policy.MaxRawBytes == 0 || output.Len() <= policy.MaxRawBytes {
					attachment.Content = output.Bytes()
					attachment.MimeType = "image/jpeg"
					attachment.FileName = replaceExtension(attachment.FileName, ".jpg")
					return attachment, nil
				}
			}
			bounds := current.Bounds()
			if bounds.Dx() <= 1 && bounds.Dy() <= 1 {
				break
			}
			current = imaging.Resize(current, max(1, bounds.Dx()*policy.ResizePercent/100), max(1, bounds.Dy()*policy.ResizePercent/100), imaging.Lanczos)
		}
	} else {
		current := value
		for {
			var output bytes.Buffer
			if err := imaging.Encode(&output, current, imaging.PNG); err != nil {
				return message.Attachment{}, err
			}
			if policy.MaxRawBytes == 0 || output.Len() <= policy.MaxRawBytes {
				attachment.Content = output.Bytes()
				attachment.MimeType = "image/png"
				attachment.FileName = replaceExtension(attachment.FileName, ".png")
				return attachment, nil
			}
			bounds := current.Bounds()
			if bounds.Dx() <= 1 && bounds.Dy() <= 1 {
				break
			}
			current = imaging.Resize(current, max(1, bounds.Dx()*policy.ResizePercent/100), max(1, bounds.Dy()*policy.ResizePercent/100), imaging.Lanczos)
		}
	}
	return message.Attachment{}, fmt.Errorf("image cannot be reduced below the provider payload limit")
}

func mimeForFormat(format string) string {
	switch strings.ToLower(format) {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return ""
	}
}

func flattenAlpha(value image.Image, mode string) image.Image {
	background := color.White
	if mode == "black" {
		background = color.Black
	}
	bounds := value.Bounds()
	flattened := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(flattened, flattened.Bounds(), &image.Uniform{C: background}, image.Point{}, draw.Src)
	draw.Draw(flattened, flattened.Bounds(), value, bounds.Min, draw.Over)
	return flattened
}

func extensionsForMediaType(mediaType string) []string {
	switch mediaType {
	case "image/gif":
		return []string{".gif"}
	case "image/jpeg":
		return []string{".jpeg", ".jpg"}
	case "image/png":
		return []string{".png"}
	case "image/webp":
		return []string{".webp"}
	default:
		return nil
	}
}

func isOpaque(value image.Image) bool {
	if opaque, ok := value.(interface{ Opaque() bool }); ok {
		return opaque.Opaque()
	}
	return false
}

func replaceExtension(name, extension string) string {
	current := filepath.Ext(name)
	if current == "" {
		return name + extension
	}
	return strings.TrimSuffix(name, current) + extension
}
