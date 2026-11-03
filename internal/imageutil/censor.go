package imageutil

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp"
)

const CensorshipLabel = "CENSORED FROM AGENT PRYING"

func Censor(data []byte) ([]byte, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode protected image: %w", err)
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	blurred := image.NewNRGBA(image.Rect(0, 0, width, height))
	radius := max(3, min(width, height)/32)
	horizontal := image.NewNRGBA(blurred.Bounds())
	for y := range height {
		var red, green, blue uint64
		for x := -radius; x <= radius; x++ {
			r, g, b, _ := source.At(bounds.Min.X+max(0, min(width-1, x)), bounds.Min.Y+y).RGBA()
			red, green, blue = red+uint64(r), green+uint64(g), blue+uint64(b)
		}
		divisor := uint64(2*radius+1) * 257
		for x := range width {
			horizontal.SetNRGBA(x, y, color.NRGBA{R: uint8(red / divisor), G: uint8(green / divisor), B: uint8(blue / divisor), A: 255})
			oldR, oldG, oldB, _ := source.At(bounds.Min.X+max(0, x-radius), bounds.Min.Y+y).RGBA()
			newR, newG, newB, _ := source.At(bounds.Min.X+min(width-1, x+radius+1), bounds.Min.Y+y).RGBA()
			red = red - uint64(oldR) + uint64(newR)
			green = green - uint64(oldG) + uint64(newG)
			blue = blue - uint64(oldB) + uint64(newB)
		}
	}
	for x := range width {
		var red, green, blue uint64
		for y := -radius; y <= radius; y++ {
			pixel := horizontal.NRGBAAt(x, max(0, min(height-1, y)))
			red, green, blue = red+uint64(pixel.R), green+uint64(pixel.G), blue+uint64(pixel.B)
		}
		divisor := uint64(2*radius + 1)
		for y := range height {
			gray := color.GrayModel.Convert(color.NRGBA{R: uint8(red / divisor), G: uint8(green / divisor), B: uint8(blue / divisor), A: 255}).(color.Gray).Y
			blurred.SetNRGBA(x, y, color.NRGBA{R: gray, G: gray, B: gray, A: 255})
			old := horizontal.NRGBAAt(x, max(0, y-radius))
			next := horizontal.NRGBAAt(x, min(height-1, y+radius+1))
			red = red - uint64(old.R) + uint64(next.R)
			green = green - uint64(old.G) + uint64(next.G)
			blue = blue - uint64(old.B) + uint64(next.B)
		}
	}
	face := basicfont.Face7x13
	lines := []string{CensorshipLabel}
	if width < font.MeasureString(face, CensorshipLabel).Ceil()+12 {
		lines = strings.Fields(CensorshipLabel)
	}
	labelWidth := 0
	for _, line := range lines {
		labelWidth = max(labelWidth, font.MeasureString(face, line).Ceil())
	}
	label := image.NewNRGBA(image.Rect(0, 0, labelWidth, len(lines)*face.Height))
	drawer := font.Drawer{Dst: label, Src: image.NewUniform(color.NRGBA{R: 255, A: 255}), Face: face}
	for index, line := range lines {
		drawer.Dot = fixed.P((labelWidth-font.MeasureString(face, line).Ceil())/2, index*face.Height+face.Ascent)
		drawer.DrawString(line)
	}
	scale := max(1, width/(labelWidth+20))
	label = scaleLabel(label, min(width, labelWidth*scale), min(height, label.Bounds().Dy()*scale))
	position := image.Pt((width-label.Bounds().Dx())/2, (height-label.Bounds().Dy())/2)
	draw.Draw(blurred, label.Bounds().Add(position), label, image.Point{}, draw.Over)
	var output bytes.Buffer
	if err := png.Encode(&output, blurred); err != nil {
		return nil, fmt.Errorf("encode protected image: %w", err)
	}
	return output.Bytes(), nil
}

func scaleLabel(source *image.NRGBA, width, height int) *image.NRGBA {
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.NearestNeighbor.Scale(result, result.Bounds(), source, source.Bounds(), draw.Src, nil)
	return result
}
