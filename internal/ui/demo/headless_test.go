package demo

import (
	"bytes"
	"image/png"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/ui/model"
)

func TestPreviewHeadlessTerminalColorsAndReset(t *testing.T) {
	var renderer headlessRenderer
	defer func() {
		if renderer.ctx != nil {
			renderer.ctx.Close()
		}
	}()
	frame := model.PreviewFrame{Cols: 45, Rows: 15, Content: "\x1b[48;2;18;52;86m \x1b[0m\x1b[44m \x1b[0m界 e\u0301"}
	data, lines, err := renderer.capture(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lines[0], "界 e\u0301") {
		t.Fatalf("lost wide or combining text: %q", lines[0])
	}
	image, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct {
		x                int
		red, green, blue uint32
	}{{0, 18, 52, 86}, {8, 0, 0, 238}} {
		red, green, blue, _ := image.At(expected.x, 0).RGBA()
		if red>>8 != expected.red || green>>8 != expected.green || blue>>8 != expected.blue {
			t.Fatalf("wrong background at %d: %d %d %d", expected.x, red>>8, green>>8, blue>>8)
		}
	}
	frame.Content = "new frame"
	_, lines, err = renderer.capture(frame)
	if err != nil {
		t.Fatal(err)
	}
	if lines[0] != "new frame" {
		t.Fatalf("stale terminal content: %q", lines[0])
	}
}

func TestPreviewHeadlessFallbackGlyphs(t *testing.T) {
	var renderer headlessRenderer
	defer func() {
		if renderer.ctx != nil {
			renderer.ctx.Close()
		}
	}()
	images := make(map[string][]byte)
	for _, text := range []string{"", "\U0010ffff", "日", "本", "😀", "🚀"} {
		data, _, err := renderer.capture(model.PreviewFrame{Cols: 45, Rows: 15, Content: text})
		if err != nil {
			t.Fatal(err)
		}
		for previous, image := range images {
			if bytes.Equal(data, image) {
				t.Fatalf("glyph %q rasterized identically to %q", text, previous)
			}
		}
		images[text] = data
	}
}

func TestPreviewHeadlessEmojiBackgroundWidth(t *testing.T) {
	var renderer headlessRenderer
	defer func() {
		if renderer.ctx != nil {
			renderer.ctx.Close()
		}
	}()
	frame := model.PreviewFrame{Cols: 45, Rows: 15, Content: "\x1b[48;2;18;52;86m😀 🚀 \x1b[0m\n\x1b[48;2;18;52;86m日本  \x1b[0m"}
	data, _, err := renderer.capture(frame)
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, y := range []int{0, 16} {
		for x := 0; x < 48; x++ {
			r, g, b, _ := image.At(x, y).RGBA()
			if r>>8 != 18 || g>>8 != 52 || b>>8 != 86 {
				t.Fatalf("lost six-cell background at %d,%d", x, y)
			}
		}
	}
}

func TestPreviewHeadlessSVGAttributes(t *testing.T) {
	svg := string(terminalSVG([]terminalCell{{Text: "<&", Width: 1, FG: 0x123456, FGRGB: true, BG: -1, Bold: true, Italic: true, Dim: true, Underline: true, Strike: true}}, 45, 15, "Go Mono"))
	for _, expected := range []string{"&lt;&amp;", `fill="#123456"`, `font-weight="bold"`, `font-style="italic"`, `opacity="0.5"`, `y="15"`, `y="8"`} {
		if !strings.Contains(svg, expected) {
			t.Fatalf("missing SVG attribute %s", expected)
		}
	}
}
