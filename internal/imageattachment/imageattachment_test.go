package imageattachment

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/example-git/crux/internal/message"
	"github.com/stretchr/testify/require"
)

func TestProviderPoliciesExposeSupportedFormats(t *testing.T) {
	t.Parallel()

	for _, providerID := range []string{"codex", "gemini-ag"} {
		policy, ok := PolicyFor(providerID)
		require.True(t, ok)
		require.Equal(t, []string{".gif", ".jpeg", ".jpg", ".png", ".webp"}, policy.Extensions)
		for _, mimeType := range []string{"image/gif", "image/jpeg", "image/png", "image/webp"} {
			require.True(t, policy.MIMETypes[mimeType])
		}
	}

	_, ok := PolicyFor("openai")
	require.False(t, ok)
	require.Equal(t, []string{".jpeg", ".jpg", ".png"}, ExtensionsFor("openai"))
	require.Equal(t, MaxSourceBytes, SourceLimitFor("codex"))
	require.Equal(t, DefaultMaxSourceBytes, SourceLimitFor("openai"))
}

func TestNormalizeCodexEnforcesDetailPatchBudget(t *testing.T) {
	t.Parallel()

	attachment := encodeJPEGAttachment(t, "wide.jpg", image.NewRGBA(image.Rect(0, 0, 3000, 1000)))
	policy, ok := PolicyFor("codex")
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
	policy, ok := PolicyFor("codex")
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

func TestNormalizeGeminiPreservesCompliantOriginal(t *testing.T) {
	t.Parallel()

	attachment := encodePNGAttachment(t, "wide.png", image.NewRGBA(image.Rect(0, 0, 3000, 10)))
	policy, ok := PolicyFor("gemini-ag")
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
	normalized, err := NormalizeAll("codex", attachments)
	require.NoError(t, err)
	require.Equal(t, attachments, normalized)

	normalized, err = NormalizeAll("openai", attachments)
	require.NoError(t, err)
	require.Equal(t, attachments, normalized)
}

func TestNormalizeEnforcesSourceAndDeclaredFormatLimits(t *testing.T) {
	t.Parallel()

	policy := Policy{MaxSourceBytes: 3, MIMETypes: map[string]bool{"image/png": true}}
	_, err := Normalize(policy, message.Attachment{FileName: "large.png", MimeType: "image/png", Content: []byte("four")})
	require.ErrorContains(t, err, "source limit")

	attachment := encodeJPEGAttachment(t, "photo.jpg", image.NewRGBA(image.Rect(0, 0, 2, 2)))
	policy.MaxSourceBytes = int64(len(attachment.Content) + 1)
	_, err = Normalize(policy, attachment)
	require.ErrorContains(t, err, "not supported")
}

func TestNormalizeRejectsMalformedImages(t *testing.T) {
	t.Parallel()

	policy, ok := PolicyFor("codex")
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
