package imageattachment

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func testRegistration(t *testing.T, providerID string) providerregistry.Registration {
	t.Helper()
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup(providerID)
	require.True(t, ok)
	return registration
}

func TestProviderPoliciesExposeSupportedFormats(t *testing.T) {
	t.Parallel()

	for _, providerID := range []string{"codex", "gemini-ag"} {
		policy, ok := PolicyFor(testRegistration(t, providerID))
		require.True(t, ok)
		require.Equal(t, []string{".gif", ".jpeg", ".jpg", ".png", ".webp"}, policy.Extensions)
		for _, mimeType := range []string{"image/gif", "image/jpeg", "image/png", "image/webp"} {
			require.True(t, policy.MIMETypes[mimeType])
		}
	}

	_, ok := PolicyFor(providerregistry.Registration{})
	require.False(t, ok)
	require.Equal(t, []string{".jpeg", ".jpg", ".png"}, ExtensionsFor(providerregistry.Registration{}))
	require.Equal(t, MaxSourceBytes, SourceLimitFor(testRegistration(t, "codex")))
	require.Equal(t, DefaultMaxSourceBytes, SourceLimitFor(providerregistry.Registration{}))
}

func TestConfiguredOwnerCannotInheritProviderIDImagePolicy(t *testing.T) {
	t.Parallel()

	_, ok := PolicyFor(providerregistry.Registration{})
	require.False(t, ok)

	_, ok = PolicyFor(testRegistration(t, "codex"))
	require.True(t, ok)
}

func TestNormalizeCodexEnforcesDetailPatchBudget(t *testing.T) {
	t.Parallel()

	attachment := encodeJPEGAttachment(t, "wide.jpg", image.NewRGBA(image.Rect(0, 0, 3000, 1000)))
	policy, ok := PolicyFor(testRegistration(t, "codex"))
	require.True(t, ok)
	normalized, err := Normalize(policy, attachment)
	require.NoError(t, err)

	config, _, err := image.DecodeConfig(bytes.NewReader(normalized.Content))
	require.NoError(t, err)
	require.LessOrEqual(t, max(config.Width, config.Height), policy.MaxSide)
	require.LessOrEqual(t, patchCount(config.Width, config.Height), policy.MaxPatches)
	require.InDelta(t, 3, float64(config.Width)/float64(config.Height), 0.01)
	require.LessOrEqual(t, len(normalized.Content), policy.MaxRawBytes)
}

func TestNormalizeCodexConvertsImagesToBoundedJPEG(t *testing.T) {
	t.Parallel()

	pixels := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	pixels.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 0})
	attachment := encodePNGAttachment(t, "transparent.png", pixels)
	attachment.MimeType = "IMAGE/PNG"
	policy, ok := PolicyFor(testRegistration(t, "codex"))
	require.True(t, ok)
	require.Equal(t, 512*1024, policy.MaxRawBytes)
	require.Equal(t, 1920, policy.MaxSide)

	normalized, err := Normalize(policy, attachment)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", normalized.MimeType)
	require.Equal(t, "transparent.jpg", normalized.FileName)
	require.LessOrEqual(t, len(normalized.Content), policy.MaxRawBytes)

	decoded, format, err := image.Decode(bytes.NewReader(normalized.Content))
	require.NoError(t, err)
	require.Equal(t, "jpeg", format)
	red, green, blue, _ := decoded.At(0, 0).RGBA()
	require.Greater(t, red, uint32(60000))
	require.Greater(t, green, uint32(60000))
	require.Greater(t, blue, uint32(60000))

	var gifOutput bytes.Buffer
	require.NoError(t, gif.Encode(&gifOutput, image.NewPaletted(image.Rect(0, 0, 16, 16), color.Palette{color.Black, color.White}), nil))
	gifNormalized, err := Normalize(policy, message.Attachment{FileName: "animated.gif", MimeType: "image/gif", Content: gifOutput.Bytes()})
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", gifNormalized.MimeType)
	require.Equal(t, "animated.jpg", gifNormalized.FileName)
}

func TestPolicyFromDeclarationCopiesImagePolicyValues(t *testing.T) {
	t.Parallel()

	declaration := &manifest.ImagePolicy{
		AcceptedMediaTypes: []string{"image/png"},
		MaxSourceBytes:     1024,
		MaxSidePixels:      768,
		MaxOutputBytes:     512,
		MaxPatches:         64,
		OutputMediaType:    "image/png",
		FlattenAlpha:       "white",
		QualitySteps:       []int{90},
		ResizePercent:      60,
	}
	policy, ok := PolicyFromDeclaration(declaration)
	require.True(t, ok)
	declaration.AcceptedMediaTypes[0] = "image/jpeg"
	declaration.QualitySteps[0] = 10

	require.Equal(t, []string{".png"}, policy.Extensions)
	require.Equal(t, map[string]bool{"image/png": true}, policy.MIMETypes)
	require.Equal(t, int64(1024), policy.MaxSourceBytes)
	require.Equal(t, 768, policy.MaxSide)
	require.Equal(t, 512, policy.MaxRawBytes)
	require.Equal(t, 64, policy.MaxPatches)
	require.Equal(t, "image/png", policy.OutputMediaType)
	require.Equal(t, "white", policy.FlattenAlpha)
	require.Equal(t, []int{90}, policy.QualitySteps)
	require.Equal(t, 60, policy.ResizePercent)
}

func TestNormalizeExecutesDeclaredAlphaMode(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		mode string
		want uint32
	}{
		{mode: "white", want: 0xffff},
		{mode: "black", want: 0},
	} {
		t.Run(test.mode, func(t *testing.T) {
			t.Parallel()
			pixels := image.NewNRGBA(image.Rect(0, 0, 1, 1))
			pixels.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 0})
			attachment := encodePNGAttachment(t, "transparent.png", pixels)
			policy := Policy{
				MIMETypes:       map[string]bool{"image/png": true},
				MaxSourceBytes:  int64(len(attachment.Content) + 1),
				OutputMediaType: "image/png",
				FlattenAlpha:    test.mode,
				ResizePercent:   80,
			}

			normalized, err := Normalize(policy, attachment)
			require.NoError(t, err)
			decoded, _, err := image.Decode(bytes.NewReader(normalized.Content))
			require.NoError(t, err)
			red, green, blue, alpha := decoded.At(0, 0).RGBA()
			require.Equal(t, test.want, red)
			require.Equal(t, test.want, green)
			require.Equal(t, test.want, blue)
			require.Equal(t, uint32(0xffff), alpha)
		})
	}
}

func TestNormalizeExecutesDeclaredJPEGQuality(t *testing.T) {
	t.Parallel()

	attachment := encodePNGAttachment(t, "noise.png", deterministicNoise(image.Rect(0, 0, 128, 128)))
	normalize := func(quality int) message.Attachment {
		policy := Policy{
			MIMETypes:       map[string]bool{"image/jpeg": true, "image/png": true},
			MaxSourceBytes:  int64(len(attachment.Content) + 1),
			OutputMediaType: "image/jpeg",
			QualitySteps:    []int{quality},
			ResizePercent:   80,
		}
		normalized, err := Normalize(policy, attachment)
		require.NoError(t, err)
		return normalized
	}

	high := normalize(100)
	low := normalize(1)
	require.Equal(t, "image/jpeg", high.MimeType)
	require.Equal(t, "image/jpeg", low.MimeType)
	require.Greater(t, len(high.Content), len(low.Content))
}

func TestNormalizeExecutesDeclaredResizePercent(t *testing.T) {
	t.Parallel()

	const side = 256
	attachment := encodePNGAttachment(t, "noise.png", deterministicNoise(image.Rect(0, 0, side, side)))
	base := Policy{
		MIMETypes:       map[string]bool{"image/jpeg": true, "image/png": true},
		MaxSourceBytes:  int64(len(attachment.Content) + 1),
		OutputMediaType: "image/jpeg",
		QualitySteps:    []int{90},
		ResizePercent:   80,
	}
	full, err := Normalize(base, attachment)
	require.NoError(t, err)

	for _, percent := range []int{50, 80} {
		t.Run(fmt.Sprintf("%d", percent), func(t *testing.T) {
			t.Parallel()
			policy := base
			policy.MaxRawBytes = len(full.Content) - 1
			policy.ResizePercent = percent
			normalized, err := Normalize(policy, attachment)
			require.NoError(t, err)
			config, _, err := image.DecodeConfig(bytes.NewReader(normalized.Content))
			require.NoError(t, err)
			require.Equal(t, side*percent/100, config.Width)
			require.Equal(t, side*percent/100, config.Height)
			require.LessOrEqual(t, len(normalized.Content), policy.MaxRawBytes)
		})
	}
}

func deterministicNoise(bounds image.Rectangle) *image.NRGBA {
	pixels := image.NewNRGBA(bounds)
	state := uint32(1)
	for offset := range pixels.Pix {
		state = state*1664525 + 1013904223
		pixels.Pix[offset] = byte(state >> 24)
	}
	return pixels
}

func TestNormalizeGeminiPreservesCompliantOriginal(t *testing.T) {
	t.Parallel()

	attachment := encodePNGAttachment(t, "wide.png", image.NewRGBA(image.Rect(0, 0, 3000, 10)))
	policy, ok := PolicyFor(testRegistration(t, "gemini-ag"))
	require.True(t, ok)
	normalized, err := Normalize(policy, attachment)
	require.NoError(t, err)
	require.Equal(t, attachment.Content, normalized.Content)
	require.Equal(t, attachment.FileName, normalized.FileName)
	require.Equal(t, "image/png", normalized.MimeType)
}

func TestNormalizeAllLeavesNonImagesAndUnknownProvidersUntouched(t *testing.T) {
	t.Parallel()

	attachments := []message.Attachment{{FileName: "notes.txt", MimeType: "text/plain", Content: []byte("hello")}}
	normalized, err := NormalizeAll(testRegistration(t, "codex"), attachments)
	require.NoError(t, err)
	require.Equal(t, attachments, normalized)

	normalized, err = NormalizeAll(providerregistry.Registration{}, attachments)
	require.NoError(t, err)
	require.Equal(t, attachments, normalized)
}

func TestNormalizeEnforcesSourceAndDeclaredFormatLimits(t *testing.T) {
	t.Parallel()

	policy := Policy{MaxSourceBytes: 3, MIMETypes: map[string]bool{"image/png": true}}
	_, err := Normalize(policy, message.Attachment{FileName: "large.png", MimeType: "image/png", Content: []byte("four")})
	require.ErrorContains(t, err, "declared source limit of 3 bytes")

	attachment := encodeJPEGAttachment(t, "photo.jpg", image.NewRGBA(image.Rect(0, 0, 2, 2)))
	policy.MaxSourceBytes = int64(len(attachment.Content) + 1)
	_, err = Normalize(policy, attachment)
	require.ErrorContains(t, err, "not supported")
}

func TestNormalizeRejectsMalformedImages(t *testing.T) {
	t.Parallel()

	policy, ok := PolicyFor(testRegistration(t, "codex"))
	require.True(t, ok)
	_, err := Normalize(policy, message.Attachment{FileName: "broken.png", MimeType: "image/png", Content: []byte("not an image")})
	require.ErrorContains(t, err, "decode image metadata")
}

func encodeJPEGAttachment(t *testing.T, name string, value image.Image) message.Attachment {
	t.Helper()
	var output bytes.Buffer
	require.NoError(t, jpeg.Encode(&output, value, &jpeg.Options{Quality: 95}))
	return message.Attachment{FileName: name, MimeType: "image/jpeg", Content: output.Bytes()}
}

func encodePNGAttachment(t *testing.T, name string, value image.Image) message.Attachment {
	t.Helper()
	if pixels, ok := value.(*image.RGBA); ok {
		pixels.Set(0, 0, color.RGBA{R: 255, A: 255})
	}
	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, value))
	return message.Attachment{FileName: name, MimeType: "image/png", Content: output.Bytes()}
}
