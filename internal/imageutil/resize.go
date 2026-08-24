package imageutil

import (
	"image"

	"golang.org/x/image/draw"
)

func Resize(source image.Image, width, height int) image.Image {
	if source == nil || width <= 0 || height <= 0 {
		return source
	}
	bounds := source.Bounds()
	if bounds.Dx() == width && bounds.Dy() == height {
		return source
	}
	destination := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(destination, destination.Bounds(), source, bounds, draw.Over, nil)
	return destination
}

func Fit(source image.Image, maximumWidth, maximumHeight int) image.Image {
	if source == nil || maximumWidth <= 0 || maximumHeight <= 0 {
		return source
	}
	bounds := source.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= maximumWidth && height <= maximumHeight {
		return source
	}
	if width*maximumHeight > height*maximumWidth {
		height = max(1, height*maximumWidth/width)
		width = maximumWidth
	} else {
		width = max(1, width*maximumHeight/height)
		height = maximumHeight
	}
	return Resize(source, width, height)
}
