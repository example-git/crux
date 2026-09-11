package uv

import (
	"bytes"
	"testing"
)

func TestRendererReusesLineDataWithoutChangingOutput(t *testing.T) {
	frames := []string{"hello", "\x1b[31m世界\x1b[m", "one\ntwo\nthree", "short", ""}
	var outputs [2][]string
	for mode := range outputs {
		var output bytes.Buffer
		renderer := NewTerminalRenderer(&output, []string{"TERM=xterm-256color", "COLORTERM=truecolor"})
		renderer.SetFullscreen(true)
		renderer.SetScrollOptim(!isWindows)
		screen := NewScreenBuffer(20, 4)
		for _, frame := range frames {
			output.Reset()
			if mode == 1 {
				screen.Touched = make([]*LineData, screen.Height())
			}
			screen.Clear()
			NewStyledString(frame).Draw(screen, screen.Bounds())
			before := append([]*LineData(nil), screen.Touched...)
			renderer.Render(screen.RenderBuffer)
			if err := renderer.Flush(); err != nil {
				t.Fatal(err)
			}
			for i, line := range before {
				if line != nil && line != screen.Touched[i] {
					t.Fatal("renderer replaced existing line metadata")
				}
			}
			outputs[mode] = append(outputs[mode], output.String())
		}
	}
	for i := range frames {
		if outputs[0][i] != outputs[1][i] {
			t.Fatalf("frame %d output differs: %q != %q", i, outputs[0][i], outputs[1][i])
		}
	}
}
