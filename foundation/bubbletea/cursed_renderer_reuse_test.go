package tea

import (
	"bytes"
	"testing"
)

func TestCursedRendererOutputBufferReuse(t *testing.T) {
	frames := []View{{Content: "hello", AltScreen: true}, {Content: "\x1b[31m世界\x1b[m", AltScreen: true}, {Content: "one\ntwo", AltScreen: true}, {Content: "short"}, {Content: ""}}
	var outputs [2][]string
	for mode := range outputs {
		var output bytes.Buffer
		renderer := newCursedRenderer(&output, []string{"TERM=xterm-256color"}, 20, 4)
		for i, frame := range frames {
			output.Reset()
			if mode == 1 {
				renderer.output = bytes.Buffer{}
			}
			if i == 2 {
				renderer.resize(12, 3)
			}
			renderer.render(frame)
			if err := renderer.flush(false); err != nil {
				t.Fatal(err)
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
