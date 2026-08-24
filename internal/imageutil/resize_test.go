package imageutil

import (
	"image"
	"image/color"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResizeUsesRequestedDimensions(t *testing.T) {
	source := image.NewRGBA(image.Rect(2, 3, 12, 23))
	source.Set(2, 3, color.RGBA{R: 255, A: 255})
	resized := Resize(source, 5, 10)
	require.Equal(t, image.Rect(0, 0, 5, 10), resized.Bounds())
}

func TestFitPreservesAspectRatioWithoutEnlarging(t *testing.T) {
	testCases := map[string]struct {
		bounds image.Rectangle
		width  int
		height int
		want   image.Rectangle
	}{
		"wide": {
			bounds: image.Rect(0, 0, 400, 200),
			width:  100,
			height: 100,
			want:   image.Rect(0, 0, 100, 50),
		},
		"tall": {
			bounds: image.Rect(0, 0, 200, 400),
			width:  100,
			height: 100,
			want:   image.Rect(0, 0, 50, 100),
		},
		"small": {
			bounds: image.Rect(0, 0, 20, 10),
			width:  100,
			height: 100,
			want:   image.Rect(0, 0, 20, 10),
		},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			source := image.NewRGBA(testCase.bounds)
			require.Equal(t, testCase.want, Fit(source, testCase.width, testCase.height).Bounds())
		})
	}
}
