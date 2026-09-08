package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/dop251/goja"
	"github.com/example-git/crux/internal/ui/model"
	resvg "github.com/kanrichan/resvg-go"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/gomonobolditalic"
	"golang.org/x/image/font/gofont/gomonoitalic"
)

type terminalCell struct {
	X, Y, Width, FG, BG                                                    int
	Text                                                                   string
	FGRGB, BGRGB, Bold, Italic, Dim, Inverse, Invisible, Underline, Strike bool
}

type terminalCapture struct {
	Lines []string
	Cells []terminalCell
}

type headlessRenderer struct {
	mu     sync.Mutex
	vm     *goja.Runtime
	ctx    *resvg.Context
	raster *resvg.Renderer
	font   string
}

func (h *headlessRenderer) capture(frame model.PreviewFrame) ([]byte, []string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.vm == nil {
		vm := goja.New()
		if err := vm.Set("characterWidth", func(codepoint int) int { return ansi.StringWidth(string(rune(codepoint))) }); err != nil {
			return nil, nil, err
		}
		started := time.Now()
		if err := vm.Set("clock", func() float64 { return float64(time.Since(started).Microseconds()) / 1000 }); err != nil {
			return nil, nil, err
		}
		_, err := vm.RunString(`var exports={};var process={title:'crux-headless'};var performance={now:clock};var console={log:function(){},warn:function(){},error:function(){}};var tasks=[],nextID=0;function setTimeout(fn){var id=++nextID;tasks.push({id:id,fn:fn});return id;}function clearTimeout(id){tasks=tasks.filter(function(t){return t.id!==id;});}function queueMicrotask(fn){setTimeout(fn);}function drain(){var count=0;while(tasks.length){if(++count>10000)throw Error('timer loop');tasks.shift().fn();}}`)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range []string{"web/headless.js", "web/capture.js"} {
			code, err := assets.ReadFile(name)
			if err != nil {
				return nil, nil, err
			}
			if _, err := vm.RunScript(name, string(code)); err != nil {
				return nil, nil, err
			}
		}
		ctx, err := resvg.NewContext(context.Background())
		if err != nil {
			return nil, nil, err
		}
		raster, err := ctx.NewRenderer()
		if err != nil {
			ctx.Close()
			return nil, nil, err
		}
		for _, font := range [][]byte{gomono.TTF, gomonobold.TTF, gomonoitalic.TTF, gomonobolditalic.TTF} {
			if err := raster.LoadFontData(font); err != nil {
				ctx.Close()
				return nil, nil, err
			}
		}
		for _, name := range []string{"web/fonts/NotoSansJP.ttf", "web/fonts/NotoEmoji.ttf"} {
			font, err := assets.ReadFile(name)
			if err != nil {
				ctx.Close()
				return nil, nil, err
			}
			if err := raster.LoadFontData(font); err != nil {
				ctx.Close()
				return nil, nil, err
			}
		}
		fontFamily := "Go Mono, Noto Sans JP, Noto Emoji"
		if runtime.GOOS == "darwin" {
			for _, path := range []string{"/System/Library/Fonts/Menlo.ttc", "/System/Library/Fonts/Apple Symbols.ttf"} {
				font, err := os.ReadFile(path)
				if err != nil {
					ctx.Close()
					return nil, nil, err
				}
				if err := raster.LoadFontData(font); err != nil {
					ctx.Close()
					return nil, nil, err
				}
			}
			fontFamily = "Menlo, Apple Symbols, Go Mono, Noto Sans JP, Noto Emoji"
		}
		h.vm, h.ctx, h.raster, h.font = vm, ctx, raster, fontFamily
	}
	capture, ok := goja.AssertFunction(h.vm.Get("captureFrame"))
	if !ok {
		return nil, nil, fmt.Errorf("missing embedded terminal capture")
	}
	value, err := capture(goja.Undefined(), h.vm.ToValue(frame.Content), h.vm.ToValue(frame.Cols), h.vm.ToValue(frame.Rows))
	if err != nil {
		return nil, nil, err
	}
	var terminal terminalCapture
	if err := json.Unmarshal([]byte(value.String()), &terminal); err != nil {
		return nil, nil, err
	}
	png, err := h.raster.Render(terminalSVG(terminal.Cells, frame.Cols, frame.Rows, h.font))
	return png, terminal.Lines, err
}

func terminalColor(value int, rgb bool, fallback string) string {
	if rgb {
		return fmt.Sprintf("#%06x", value)
	}
	palette := []string{"#000000", "#cd0000", "#00cd00", "#cdcd00", "#0000ee", "#cd00cd", "#00cdcd", "#e5e5e5", "#7f7f7f", "#ff0000", "#00ff00", "#ffff00", "#5c5cff", "#ff00ff", "#00ffff", "#ffffff"}
	if value < 0 {
		return fallback
	}
	if value < 16 {
		return palette[value]
	}
	if value < 232 {
		levels := []int{0, 95, 135, 175, 215, 255}
		value -= 16
		return fmt.Sprintf("#%02x%02x%02x", levels[value/36], levels[value/6%6], levels[value%6])
	}
	gray := 8 + (value-232)*10
	return fmt.Sprintf("#%02x%02x%02x", gray, gray, gray)
}

func terminalSVG(cells []terminalCell, cols, rows int, font string) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d"><rect width="100%%" height="100%%" fill="#202026"/>`, cols*8, rows*16)
	for _, cell := range cells {
		fg, bg := terminalColor(cell.FG, cell.FGRGB, "#d1ceda"), terminalColor(cell.BG, cell.BGRGB, "#202026")
		if cell.Inverse {
			fg, bg = bg, fg
		}
		fmt.Fprintf(&out, `<rect x="%d" y="%d" width="%d" height="16" fill="%s"/>`, cell.X*8, cell.Y*16, cell.Width*8, bg)
	}
	for _, cell := range cells {
		if cell.Invisible || cell.Text == "" {
			continue
		}
		fg, bg := terminalColor(cell.FG, cell.FGRGB, "#d1ceda"), terminalColor(cell.BG, cell.BGRGB, "#202026")
		if cell.Inverse {
			fg = bg
		}
		weight, style, opacity := "normal", "normal", "1"
		if cell.Bold {
			weight = "bold"
		}
		if cell.Italic {
			style = "italic"
		}
		if cell.Dim {
			opacity = "0.5"
		}
		fmt.Fprintf(&out, `<text x="%d" y="%d" font-family="%s" font-size="13" font-weight="%s" font-style="%s" opacity="%s" fill="%s">%s</text>`, cell.X*8, cell.Y*16+13, html.EscapeString(font), weight, style, opacity, fg, html.EscapeString(cell.Text))
		for _, decoration := range []struct {
			enabled bool
			offset  int
		}{{cell.Underline, 15}, {cell.Strike, 8}} {
			if decoration.enabled {
				fmt.Fprintf(&out, `<rect x="%d" y="%d" width="%d" height="1" fill="%s" opacity="%s"/>`, cell.X*8, cell.Y*16+decoration.offset, cell.Width*8, fg, opacity)
			}
		}
	}
	out.WriteString(`</svg>`)
	return out.Bytes()
}
