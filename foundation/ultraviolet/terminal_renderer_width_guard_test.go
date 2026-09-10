package uv

import (
	"bytes"
	"strings"
	"testing"
)

func TestWidthGuardCursorShortcut(t *testing.T) {
	var output bytes.Buffer
	renderer := NewTerminalRenderer(&output, []string{"TERM=xterm-256color"})
	renderer.SetFullscreen(true)
	buf := NewScreenBuffer(20, 2)
	NewStyledString("界").Draw(buf, buf.Bounds())
	renderer.cur.X, renderer.cur.Y = 0, 0
	seq := moveCursor(renderer, buf.RenderBuffer, 2, 0, true)
	if strings.Contains(seq, "界") {
		t.Fatalf("cursor movement reprinted a potentially wider cell: %q", seq)
	}
}

func TestWidthGuardUnchangedFrameEmitsNothing(t *testing.T) {
	for _, text := range []string{"ordinary text", "界 👩‍💻 e\u0301"} {
		var output bytes.Buffer
		renderer := NewTerminalRenderer(&output, []string{"TERM=xterm-256color"})
		renderer.SetFullscreen(true)
		for frame := 0; frame < 2; frame++ {
			buf := NewScreenBuffer(30, 3)
			NewStyledString(text).Draw(buf, buf.Bounds())
			renderer.Render(buf.RenderBuffer)
			if err := renderer.Flush(); err != nil {
				t.Fatal(err)
			}
			if frame == 1 && output.Len() != 0 {
				t.Fatalf("unchanged frame %q repainted: %q", text, output.String())
			}
			output.Reset()
		}
	}
}
