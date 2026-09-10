package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/dop251/goja"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/model"
	"github.com/stretchr/testify/require"
)

// TestDemoSidebarIncrementalRedraw compares actual UI.View frames after
// incremental terminal updates. Set CRUX_DEMO_REPLAY_URL to exercise a running
// binary; otherwise use the production demo HTTP handler in-process.
func TestDemoSidebarIncrementalRedraw(t *testing.T) {
	baseURL := os.Getenv("CRUX_DEMO_REPLAY_URL")
	if baseURL == "" {
		handler, err := NewHandler()
		require.NoError(t, err)
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		baseURL = server.URL
	}
	var incremental headlessRenderer
	t.Cleanup(func() {
		if incremental.ctx != nil {
			incremental.ctx.Close()
		}
	})
	client := &http.Client{Timeout: 30 * time.Second}
	fetchFrame := func(t *testing.T, options model.PreviewOptions) model.PreviewFrame {
		t.Helper()
		data, err := json.Marshal(options)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/preview", bytes.NewReader(data))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var frame model.PreviewFrame
		require.NoError(t, json.NewDecoder(response.Body).Decode(&frame))
		require.Equal(t, "crux/internal/ui/model.UI.View", frame.Renderer)
		return frame
	}
	for _, cols := range []int{100, 160, 230} {
		for _, scenario := range []struct{ name, glyph string }{
			{"scroll", ""},
			{"ascii", "plain"},
			{"cjk", "界"},
			{"emoji", "😀"},
			{"variation", "⚙️"},
			{"joined", "👩‍💻"},
			{"joined_variation", "⛓️‍💥"},
			{"joined_initial", "👩‍💻"},
		} {
			t.Run(fmt.Sprintf("%d/%s", cols, scenario.name), func(t *testing.T) {
				capture := retainedTerminalCapture(t, &incremental, cols, 45)

				var output bytes.Buffer
				writer := uv.NewTerminalRenderer(&output, []string{"TERM=xterm-256color", "TERM_PROGRAM=iTerm.app"})
				writer.SetColorProfile(colorprofile.TrueColor)
				writer.SetFullscreen(true)
				writer.SetWidthMethod(ansi.WcWidth)
				writer.SetScrollOptim(true)
				output.WriteString("\x1b[?7h")
				var previousFrame model.PreviewFrame
				for i := 0; i < 4; i++ {
					frameIndex := i
					if strings.HasPrefix(scenario.name, "joined") && i == 3 {
						frameIndex = 2 // An unchanged frame must not require a repaint.
					}
					options := model.PreviewOptions{Cols: cols, Rows: 45, Example: "all", Model: "dummy-coder", Scenario: "working", ToolsExpanded: true, PlanExpanded: true, Scroll: frameIndex * 3}
					if (i > 0 || scenario.name == "joined_initial") && scenario.glyph != "" {
						options.Example = "streaming"
						var text strings.Builder
						for row := 0; row < 60; row++ {
							glyph := scenario.glyph
							if strings.HasPrefix(scenario.name, "joined") && i >= 2 {
								glyph = "plain"
							}
							fmt.Fprintf(&text, "Row %d %s %s\n", row+frameIndex, strings.Repeat("words ", (row+frameIndex)%12), glyph)
						}
						data, err := json.Marshal(map[string]any{"examples": map[string]any{"streaming": map[string]any{"message": map[string]any{"Parts": []any{map[string]any{"text": text.String()}}}}}})
						require.NoError(t, err)
						options.Data = data
					}
					var frame model.PreviewFrame
					if strings.HasPrefix(scenario.name, "joined") && i == 3 {
						// Replay the identical bytes. Asking the stateful preview to
						// render again can settle its lazy scroll measurements.
						frame = previousFrame
					} else {
						frame = fetchFrame(t, options)
					}
					require.True(t, utf8.ValidString(frame.Content), "invalid UTF-8 from UI.View")
					canvas := uv.NewScreenBuffer(cols, 45)
					uv.NewStyledString(frame.Content).Draw(canvas, canvas.Bounds())
					previousFrame = frame
					writer.Render(canvas.RenderBuffer)
					require.NoError(t, writer.Flush())
					delta := output.String()
					require.True(t, utf8.ValidString(delta), "invalid UTF-8 from incremental renderer")
					output.Reset()
					got := capture(delta)
					if strings.HasPrefix(scenario.name, "joined") && i == 3 {
						require.Empty(t, delta, "identical frame should emit nothing")
					}

					sidebar := frame.Regions["sidebar"]
					var mismatchCount int
					var firstMismatch string
					actualCells := make(map[[2]int]terminalCell, len(got.Cells))
					for _, c := range got.Cells {
						actualCells[[2]int{c.X, c.Y}] = c
					}
					for y := sidebar.Min.Y; y < sidebar.Max.Y; y++ {
						for x := sidebar.Min.X; x < sidebar.Max.X; x++ {
							c := actualCells[[2]int{x, y}]
							e := canvas.CellAt(x, y)
							if e == nil {
								e = &uv.Cell{Content: " ", Width: 1}
							}
							if strings.TrimRight(c.Text, " ") == strings.TrimRight(e.Content, " ") && c.Width == e.Width {
								continue
							}
							mismatchCount++
							if firstMismatch == "" {
								firstMismatch = fmt.Sprintf("sidebar cell (%d,%d): got %q width=%d expected %q width=%d", x, y, c.Text, c.Width, e.Content, e.Width)
							}
						}
					}
					if mismatchCount != 0 || (strings.HasPrefix(scenario.name, "joined") && i <= 3) {
						out := os.Getenv("CRUX_SIDEBAR_PROBE_OUTPUT_DIR")
						if out != "" {
							require.NoError(t, os.MkdirAll(out, 0o700))
							png, err := incremental.raster.Render(terminalSVG(got.Cells, cols, 45, incremental.font))
							require.NoError(t, err)
							require.NoError(t, os.WriteFile(filepath.Join(out, fmt.Sprintf("incremental-%s-%d-%d.png", scenario.name, cols, i)), png, 0o600))
							// Anchor each composed row/cell explicitly to avoid using the suspect
							// terminal width behavior to place the reference sidebar.
							var reference []terminalCell
							for y := 0; y < 45; y++ {
								for x := 0; x < cols; x++ {
									e := canvas.CellAt(x, y)
									if e == nil || e.Width == 0 {
										continue
									}
									reference = append(reference, terminalCell{X: x, Y: y, Width: e.Width, Text: e.Content, FG: -1, BG: -1})
								}
							}
							refPNG, err := incremental.raster.Render(terminalSVG(reference, cols, 45, incremental.font))
							require.NoError(t, err)
							require.NoError(t, os.WriteFile(filepath.Join(out, fmt.Sprintf("composed-%s-%d-%d.png", scenario.name, cols, i)), refPNG, 0o600))
							data, err := json.MarshalIndent(map[string]any{"frame": frame, "delta": delta, "actual": got, "expected": canvas.Buffer, "mismatches": mismatchCount, "source": baseURL, "options": options}, "", "  ")
							require.NoError(t, err)
							require.NoError(t, os.WriteFile(filepath.Join(out, fmt.Sprintf("snapshot-%s-%d-%d.json", scenario.name, cols, i)), data, 0o600))
						}
						if mismatchCount > 0 {
							t.Errorf("frame=%d sidebar mismatches=%d first=%s", i, mismatchCount, firstMismatch)
						}
					}
					if strings.HasPrefix(scenario.name, "joined") && i == 3 {
						t.Logf("unchanged frame: %d sidebar mismatches; no repaint", mismatchCount)
						break
					}
				}
			})
		}
	}
}

// Use the embedded terminal unchanged, except that each update must retain
// previous cells and consume the renderer's cursor commands verbatim.
func retainedTerminalCapture(t *testing.T, renderer *headlessRenderer, cols, rows int) func(string) terminalCapture {
	t.Helper()
	_, _, err := renderer.capture(model.PreviewFrame{Cols: cols, Rows: rows})
	require.NoError(t, err)
	js, err := assets.ReadFile("web/capture.js")
	require.NoError(t, err)
	source := strings.Replace(string(js), "function captureFrame(", "function captureDelta(", 1)
	require.Contains(t, source, "  terminal.reset();")
	source = strings.Replace(source, "  terminal.reset();", "", 1)
	const fullFrameWrite = `'\x1b[?25l\x1b[?7l\x1b[0m\x1b[2J\x1b[H'+content.replace(/\r?\n/g,'\r\n')+'\x1b[0m'`
	require.Contains(t, source, fullFrameWrite)
	source = strings.Replace(source, fullFrameWrite, "content", 1)
	_, err = renderer.vm.RunScript("incremental-replay.js", source)
	require.NoError(t, err)
	capture, ok := goja.AssertFunction(renderer.vm.Get("captureDelta"))
	require.True(t, ok)
	return func(delta string) terminalCapture {
		t.Helper()
		value, err := capture(goja.Undefined(), renderer.vm.ToValue(delta), renderer.vm.ToValue(cols), renderer.vm.ToValue(rows))
		require.NoError(t, err)
		var got terminalCapture
		require.NoError(t, json.Unmarshal([]byte(value.String()), &got))
		return got
	}
}
