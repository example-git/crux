package imageutil

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCensorPreservesInputAndProducesGrayWithCenteredRedLabel(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 640, 480))
	for y := range 480 {
		for x := range 640 {
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, source))
	original := bytes.Clone(encoded.Bytes())
	result, err := Censor(encoded.Bytes())
	require.NoError(t, err)
	require.Equal(t, original, encoded.Bytes())
	require.NotEqual(t, original, result)
	decoded, err := png.Decode(bytes.NewReader(result))
	require.NoError(t, err)
	require.Equal(t, source.Bounds(), decoded.Bounds())
	redBounds := image.Rectangle{}
	redCount := 0
	for y := range 480 {
		for x := range 640 {
			r, g, b, _ := decoded.At(x, y).RGBA()
			if r != g || g != b {
				require.Greater(t, r, g)
				require.Equal(t, g, b)
				redBounds = redBounds.Union(image.Rect(x, y, x+1, y+1))
				redCount++
			}
		}
	}
	require.Greater(t, redCount, 100)
	require.InDelta(t, 320, (redBounds.Min.X+redBounds.Max.X)/2, 10)
	require.InDelta(t, 240, (redBounds.Min.Y+redBounds.Max.Y)/2, 10)
}
