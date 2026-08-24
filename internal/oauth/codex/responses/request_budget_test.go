package responses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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

	bounded, stats, err := fitCodexRequest(frame)
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

	repeated, repeatedStats, err := fitCodexRequest(frame)
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
	first, _, err := fitCodexRequest(testRequestFrame(imageItem))
	require.NoError(t, err)
	state := sessionState{chain: &responseChain{
		properties:        propertiesOf(first),
		sourceRepresented: cloneInputItems(first.Input),
		responseID:        "resp_image",
	}}
	secondSource := testRequestFrame(imageItem, testMessageItem("user", "continue"))
	second, _, err := fitCodexRequest(secondSource)
	require.NoError(t, err)

	wire, incremental, reason := state.wireRequestLocked(second)
	require.True(t, incremental)
	require.Empty(t, reason)
	require.Equal(t, "resp_image", wire.PreviousResponseID)
	require.Equal(t, []inputItem{testMessageItem("user", "continue")}, wire.Input)
}

func TestFitCodexRequestSupportsTighterRetryCeiling(t *testing.T) {
	frame := testRequestFrame(testMessageItem("user", strings.Repeat("x", 11*1024*1024)))

	_, _, err := fitCodexRequest(frame)
	require.NoError(t, err)
	_, _, err = fitCodexRequestTo(frame, retryCodexRequestBytes)
	require.ErrorContains(t, err, "safe maximum")
}

func TestFitCodexRequestRejectsOversizedTextOnlyRequest(t *testing.T) {
	frame := testRequestFrame(testMessageItem("user", strings.Repeat("x", maxCodexRequestBytes)))

	_, _, err := fitCodexRequest(frame)
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

	bounded, stats, err := fitCodexRequest(frame)
	require.NoError(t, err)
	require.Equal(t, 1, stats.compressedImages)
	require.True(t, strings.HasPrefix(bounded.Input[0].Content[0].ImageURL, "data:image/jpeg;base64,"))
}

func TestFitCodexRequestLeavesSmallRequestsUntouched(t *testing.T) {
	frame := testRequestFrame(testMessageItem("user", "hello"))

	bounded, stats, err := fitCodexRequest(frame)
	require.NoError(t, err)
	require.Equal(t, frame, bounded)
	require.Equal(t, stats.originalBytes, stats.finalBytes)
	require.Zero(t, stats.compressedImages)
	require.Zero(t, stats.omittedImages)
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
