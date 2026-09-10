package demo

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/stretchr/testify/require"
)

// Exercise the default Program renderer, not a directly constructed Ultraviolet
// renderer. Moving Crux's screen buffer alone must not silently drop the width
// guard from the event loop's terminal output.
func TestNativeProgramWidthGuard(t *testing.T) {
	const cols, rows = 45, 8
	var terminal headlessRenderer
	t.Cleanup(func() {
		if terminal.ctx != nil {
			terminal.ctx.Close()
		}
	})
	capture := retainedTerminalCapture(t, &terminal, cols, rows)
	output := &programOutput{changed: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	p := tea.NewProgram(programFrame{},
		tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(output),
		tea.WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=iTerm.app"}),
		tea.WithWindowSize(cols, rows), tea.WithColorProfile(colorprofile.TrueColor),
		tea.WithoutSignalHandler(),
	)
	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()
	t.Cleanup(func() {
		p.Quit()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Error("native program did not stop")
		}
	})

	for stage, glyph := range []string{"", "👩‍💻", "⚙️", ""} {
		canvas := uv.NewScreenBuffer(cols, rows)
		for y := range rows {
			uv.NewStyledString(strings.Repeat(string(rune('A'+y)), cols)).Draw(canvas, uv.Rect(0, y, cols, 1))
		}
		const glyphX = 22
		if glyph != "" {
			canvas.SetCell(glyphX, rows-1, &uv.Cell{Content: glyph, Width: 2})
		}
		title := fmt.Sprintf("crux-native-renderer-%d", stage)
		content := canvas.Render()
		// Program.View is a string boundary: cells are measured again before
		// terminal output. Compare with that parsed view, including any blank
		// tail left by a cluster whose measured width differs from Width: 2.
		expected := uv.NewScreenBuffer(cols, rows)
		uv.NewStyledString(content).Draw(expected, expected.Bounds())
		p.Send(programFrame{content: content, title: title})
		// WindowTitle is written in the same output flush as the frame. Wait
		// for that marker rather than assuming a sleep means it was rendered.
		delta := output.takeFrame(t, ctx, ansi.SetWindowTitle(title))
		if glyph != "" {
			require.Contains(t, delta, ansi.ResetModeAutoWrap, "native renderer must invoke the width guard")
		}
		got := capture(delta)
		cells := make(map[[2]int]terminalCell, len(got.Cells))
		for _, cell := range got.Cells {
			cells[[2]int{cell.X, cell.Y}] = cell
		}
		for y := range rows {
			for x := range cols {
				if glyph != "" && y == rows-1 && x >= glyphX && x < glyphX+2 {
					continue // Font shaping may vary within the allocated cells.
				}
				want := " "
				if cell := expected.CellAt(x, y); cell != nil {
					want = cell.Content
				}
				require.Equal(t, want, cells[[2]int{x, y}].Text,
					"stage=%d neighboring cell=(%d,%d)", stage, x, y)
			}
		}
	}
}

type programFrame struct{ content, title string }

func (m programFrame) Init() tea.Cmd { return nil }

func (m programFrame) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if frame, ok := msg.(programFrame); ok {
		m = frame
	}
	return m, nil
}

func (m programFrame) View() tea.View {
	return tea.View{Content: m.content, WindowTitle: m.title, AltScreen: true}
}

type programOutput struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func (w *programOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(data)
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (w *programOutput) takeFrame(t *testing.T, ctx context.Context, marker string) string {
	t.Helper()
	for {
		w.mu.Lock()
		if strings.Contains(w.buf.String(), marker) {
			result := w.buf.String()
			w.buf.Reset()
			w.mu.Unlock()
			return result
		}
		w.mu.Unlock()
		select {
		case <-w.changed:
		case <-ctx.Done():
			t.Fatalf("waiting for native terminal frame %q: %v", marker, ctx.Err())
		}
	}
}
