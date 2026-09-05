package responses

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testRequestBudget(t *testing.T) requestBudget {
	t.Helper()
	budget, err := compileRequestBudget(defaultImagePolicyDeclaration())
	require.NoError(t, err)
	return budget
}

func TestFitCodexRequestCompressesMultipleImagesBelowMessageLimit(t *testing.T) {
	imageData := noisyPNG(t, 1024)
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageData)
	items := make([]inputItem, 4)
	for index := range items {
		items[index] = inputItem{
			Type: "message",
			Role: "user",
			Content: []messageContent{{
				Type:     "input_image",
				ImageURL: imageURL,
				Detail:   "high",
			}},
		}
	}
	frame := testRequestFrame(items...)
	original := cloneRequestFrame(frame)

	bounded, stats, err := fitCodexRequest(frame, testRequestBudget(t))
	require.NoError(t, err)
	require.Greater(t, stats.originalBytes, maxCodexRequestBytes)
	require.LessOrEqual(t, stats.finalBytes, maxCodexRequestBytes)
	require.Positive(t, stats.compressedImages)
	require.Zero(t, stats.omittedImages)
	require.True(t, reflect.DeepEqual(original, frame))

	encoded, err := json.Marshal(fullWireRequest(bounded))
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), maxCodexRequestBytes)
	jpegImages := 0
	for _, item := range bounded.Input {
		for _, content := range item.Content {
			if content.Type == "input_image" && strings.HasPrefix(content.ImageURL, "data:image/jpeg;base64,") {
				jpegImages++
			}
		}
	}
	require.Equal(t, stats.compressedImages, jpegImages)

	repeated, repeatedStats, err := fitCodexRequest(frame, testRequestBudget(t))
	require.NoError(t, err)
	require.Equal(t, stats, repeatedStats)
	require.Equal(t, bounded, repeated)
}

func TestFitCodexRequestPreservesIncrementalChainCompatibility(t *testing.T) {
	imageData := noisyPNG(t, 64)
	imageItem := inputItem{
		Type: "message",
		Role: "user",
		Content: []messageContent{{
			Type:     "input_image",
			ImageURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageData),
			Detail:   "high",
		}},
	}
	first, _, err := fitCodexRequest(testRequestFrame(imageItem), testRequestBudget(t))
	require.NoError(t, err)
	state := sessionState{chain: &responseChain{
		properties:        propertiesOf(first),
		sourceRepresented: cloneInputItems(first.Input),
		responseID:        "resp_image",
	}}
	secondSource := testRequestFrame(imageItem, testMessageItem("user", "continue"))
	second, _, err := fitCodexRequest(secondSource, testRequestBudget(t))
	require.NoError(t, err)

	wire, incremental, reason := state.wireRequestLocked(second)
	require.True(t, incremental)
	require.Empty(t, reason)
	require.Equal(t, "resp_image", wire.PreviousResponseID)
	require.Equal(t, []inputItem{testMessageItem("user", "continue")}, wire.Input)
}

func TestFitCodexRequestSupportsTighterRetryCeiling(t *testing.T) {
	frame := testRequestFrame(testMessageItem("user", strings.Repeat("x", 11*1024*1024)))

	_, _, err := fitCodexRequest(frame, testRequestBudget(t))
	require.NoError(t, err)
	_, _, err = fitCodexRequestTo(frame, testRequestBudget(t), retryCodexRequestBytes)
	require.ErrorContains(t, err, "safe maximum")
}

func TestFitCodexRequestRejectsOversizedTextOnlyRequest(t *testing.T) {
	frame := testRequestFrame(testMessageItem("user", strings.Repeat("x", maxCodexRequestBytes)))

	_, _, err := fitCodexRequest(frame, testRequestBudget(t))
	require.ErrorContains(t, err, "compact the session before retrying")
}

func TestFitCodexRequestConvertsSmallHistoricalImagesToJPEG(t *testing.T) {
	imageData := noisyPNG(t, 64)
	frame := testRequestFrame(inputItem{
		Type: "message",
		Role: "user",
		Content: []messageContent{{
			Type:     "input_image",
			ImageURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageData),
			Detail:   "high",
		}},
	})

	bounded, stats, err := fitCodexRequest(frame, testRequestBudget(t))
	require.NoError(t, err)
	require.Equal(t, 1, stats.compressedImages)
	require.True(t, strings.HasPrefix(bounded.Input[0].Content[0].ImageURL, "data:image/jpeg;base64,"))
}

func TestFitCodexRequestBoundsSmallEncodedOversizedJPEGOnClone(t *testing.T) {
	pixels := image.NewNRGBA(image.Rect(0, 0, 3000, 1000))
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, pixels, &jpeg.Options{Quality: 90}))
	frame := imageRequestFrame("image/jpeg", encoded.Bytes(), 1)
	original := cloneRequestFrame(frame)
	budget := testRequestBudget(t)
	budget.maxSidePixels = 1600
	budget.maxPatches = 900
	budget.maxOutputBytes = 32 * 1024
	budget.perImageTargets = []int{512 * 1024}

	bounded, stats, err := fitCodexRequest(frame, budget)
	require.NoError(t, err)
	require.Equal(t, 1, stats.compressedImages)
	require.True(t, reflect.DeepEqual(original, frame))
	mimeType, encodedImage, err := splitImageDataURL(bounded.Input[0].Content[0].ImageURL)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", mimeType)
	data, err := base64.StdEncoding.DecodeString(encodedImage)
	require.NoError(t, err)
	require.LessOrEqual(t, len(data), budget.maxOutputBytes)
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, "jpeg", format)
	require.LessOrEqual(t, max(config.Width, config.Height), budget.maxSidePixels)
	require.LessOrEqual(t, requestImagePatchCount(config.Width, config.Height), budget.maxPatches)
}

func TestFitCodexRequestLeavesSmallRequestsUntouched(t *testing.T) {
	frame := testRequestFrame(testMessageItem("user", "hello"))

	bounded, stats, err := fitCodexRequest(frame, testRequestBudget(t))
	require.NoError(t, err)
	require.Equal(t, frame, bounded)
	require.Equal(t, stats.originalBytes, stats.finalBytes)
	require.Zero(t, stats.compressedImages)
	require.Zero(t, stats.omittedImages)
}

func TestNewCopiesDeclaredImagePolicy(t *testing.T) {
	policy := defaultImagePolicyDeclaration()
	created, err := New(WithImagePolicy(policy))
	require.NoError(t, err)
	policy.AcceptedMediaTypes[0] = "image/png"
	policy.QualitySteps[0] = 1
	policy.HistoryBudget.PerImageTargets[0] = 1

	modelValue, err := created.LanguageModel(context.Background(), "gpt-test")
	require.NoError(t, err)
	model, ok := modelValue.(*languageModel)
	require.True(t, ok)
	require.Contains(t, model.client.requestBudget.acceptedMediaTypes, "image/gif")
	require.Equal(t, 85, model.client.requestBudget.qualitySteps[0])
	require.Equal(t, 512*1024, model.client.requestBudget.perImageTargets[0])
}

func TestFitCodexRequestExecutesDeclaredPerImageTarget(t *testing.T) {
	data := noisyPNG(t, 256)
	frame := imageRequestFrame("image/png", data, 1)
	budget := testRequestBudget(t)
	budget.requestBytes = 2 * 1024 * 1024
	budget.perImageTargets = []int{32 * 1024}

	bounded, stats, err := fitCodexRequest(frame, budget)
	require.NoError(t, err)
	require.Equal(t, 1, stats.compressedImages)
	mimeType, encoded, err := splitImageDataURL(bounded.Input[0].Content[0].ImageURL)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", mimeType)
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	require.LessOrEqual(t, len(compressed), 32*1024)
}

func TestFitCodexRequestEnforcesDeclaredMediaAndSourcePolicy(t *testing.T) {
	data := noisyPNG(t, 32)
	frame := imageRequestFrame("image/png", data, 1)

	mediaBudget := testRequestBudget(t)
	mediaBudget.acceptedMediaTypes = map[string]struct{}{"image/jpeg": {}}
	_, _, err := fitCodexRequest(frame, mediaBudget)
	require.ErrorContains(t, err, "is not accepted")

	sourceBudget := testRequestBudget(t)
	sourceBudget.maxSourceBytes = len(data) - 1
	_, _, err = fitCodexRequest(frame, sourceBudget)
	require.ErrorContains(t, err, fmt.Sprintf("declared source limit of %d bytes", sourceBudget.maxSourceBytes))
}

func TestFitCodexRequestExecutesDeclaredOmissionPolicy(t *testing.T) {
	data := noisyPNG(t, 32)
	frame := imageRequestFrame("image/png", data, 2)
	budget := testRequestBudget(t)
	budget.perImageTargets = nil

	withoutOmission := budget
	withoutOmission.omitOldImages = false
	withoutOmission.retainNewestImage = false
	originalBytes, err := fullRequestSize(frame)
	require.NoError(t, err)
	_, _, err = fitCodexRequestTo(frame, withoutOmission, originalBytes-1)
	require.ErrorContains(t, err, "safe maximum")

	retainNewest := budget
	retainNewest.omitOldImages = true
	retainNewest.retainNewestImage = true
	oneOmitted := cloneRequestFrame(frame)
	oneOmitted.Input[0].Content[0] = messageContent{Type: "input_text", Text: omittedImageBudgetMessage}
	oneOmittedBytes, err := fullRequestSize(oneOmitted)
	require.NoError(t, err)
	bounded, stats, err := fitCodexRequestTo(frame, retainNewest, oneOmittedBytes)
	require.NoError(t, err)
	require.Equal(t, 1, stats.omittedImages)
	require.Equal(t, oneOmitted, bounded)
	require.Equal(t, "input_image", bounded.Input[0].Content[1].Type)

	omitAll := budget
	omitAll.omitOldImages = true
	omitAll.retainNewestImage = false
	bothOmitted := cloneRequestFrame(oneOmitted)
	bothOmitted.Input[0].Content[1] = messageContent{Type: "input_text", Text: omittedImageBudgetMessage}
	bothOmittedBytes, err := fullRequestSize(bothOmitted)
	require.NoError(t, err)
	bounded, stats, err = fitCodexRequestTo(frame, omitAll, bothOmittedBytes)
	require.NoError(t, err)
	require.Equal(t, 2, stats.omittedImages)
	require.Equal(t, bothOmitted, bounded)
}

func TestFitCodexRequestCountsClientMetadata(t *testing.T) {
	frame := testRequestFrame(testMessageItem("user", "hello"))
	frame.ClientMetadata = map[string]string{"declared": strings.Repeat("x", 1024)}
	withMetadata, err := fullRequestSize(frame)
	require.NoError(t, err)
	withoutMetadata := cloneRequestFrame(frame)
	withoutMetadata.ClientMetadata = nil
	withoutMetadataBytes, err := fullRequestSize(withoutMetadata)
	require.NoError(t, err)
	require.Less(t, withoutMetadataBytes, withMetadata)

	budget := testRequestBudget(t)
	budget.perImageTargets = nil
	budget.omitOldImages = false
	budget.retainNewestImage = false
	_, _, err = fitCodexRequestTo(withoutMetadata, budget, withMetadata-1)
	require.NoError(t, err)
	_, _, err = fitCodexRequestTo(frame, budget, withMetadata-1)
	require.ErrorContains(t, err, "safe maximum")
}

func TestCompressCodexRequestImageExecutesDeclaredTransformPolicy(t *testing.T) {
	data := noisyPNG(t, 256)
	budget := testRequestBudget(t)
	budget.qualitySteps = []int{100}
	highQuality, err := compressImageToJPEG(data, len(data)*2, budget)
	require.NoError(t, err)
	budget.qualitySteps = []int{1}
	lowQuality, err := compressImageToJPEG(data, len(data)*2, budget)
	require.NoError(t, err)
	require.Greater(t, len(highQuality), len(lowQuality))

	budget.qualitySteps = []int{90}
	budget.resizePercent = 80
	full, err := compressImageToJPEG(data, len(data)*2, budget)
	require.NoError(t, err)
	for _, percent := range []int{50, 80} {
		resizedBudget := budget
		resizedBudget.resizePercent = percent
		resized, resizeErr := compressImageToJPEG(data, len(full)-1, resizedBudget)
		require.NoError(t, resizeErr)
		config, _, decodeErr := image.DecodeConfig(bytes.NewReader(resized))
		require.NoError(t, decodeErr)
		require.Equal(t, 256*percent/100, config.Width)
		require.Equal(t, 256*percent/100, config.Height)
	}

	transparent := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	transparent.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 0})
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, transparent))
	for _, test := range []struct {
		mode string
		want uint32
	}{
		{mode: "white", want: 60000},
		{mode: "black", want: 5000},
	} {
		alphaBudget := budget
		alphaBudget.flattenAlpha = test.mode
		alphaBudget.qualitySteps = []int{100}
		flattened, flattenErr := compressImageToJPEG(encoded.Bytes(), len(data)*2, alphaBudget)
		require.NoError(t, flattenErr)
		decoded, _, decodeErr := image.Decode(bytes.NewReader(flattened))
		require.NoError(t, decodeErr)
		red, green, blue, _ := decoded.At(0, 0).RGBA()
		if test.mode == "white" {
			require.Greater(t, red, test.want)
			require.Greater(t, green, test.want)
			require.Greater(t, blue, test.want)
		} else {
			require.Less(t, red, test.want)
			require.Less(t, green, test.want)
			require.Less(t, blue, test.want)
		}
	}
}

func imageRequestFrame(mediaType string, data []byte, count int) *requestFrame {
	content := make([]messageContent, count)
	imageURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
	for index := range content {
		content[index] = messageContent{Type: "input_image", ImageURL: imageURL, Detail: "high"}
	}
	return testRequestFrame(inputItem{Type: "message", Role: "user", Content: content})
}

func noisyPNG(t *testing.T, side int) []byte {
	t.Helper()
	pixels := image.NewNRGBA(image.Rect(0, 0, side, side))
	state := uint32(1)
	for offset := range pixels.Pix {
		state = state*1664525 + 1013904223
		pixels.Pix[offset] = byte(state >> 24)
	}
	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, pixels))
	return output.Bytes()
}
