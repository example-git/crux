package logo

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Provider wordmarks must render with the letterform art: every letter has a
// form, all rows align, and the result is three rows tall like CRUX.
func TestRenderProviderWordmarks(t *testing.T) {
	t.Parallel()

	for _, word := range []string{"CRUX", "CLAUDE", "CODEX", "GEMINI", "COPILOT"} {
		forms, ok := lettersFor(word)
		if !ok {
			t.Fatalf("no letterforms for %s", word)
		}
		out := renderWord(1, -1, forms...)
		lines := strings.Split(out, "\n")
		if len(lines) != 3 {
			t.Errorf("%s: %d rows, want 3:\n%s", word, len(lines), out)
		}
		if lipgloss.Width(out) < len(word)*4 {
			t.Errorf("%s: implausibly narrow render:\n%s", word, out)
		}
	}
}

//nolint:tparallel // Table subtests intentionally remain serial.
func TestShortProviderWordmarks(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"CLAUDE":  "░▄▀▀░█▒░▒▄▀▄░█▒█░█▀▄▒██▀\n░▀▄▄▒█▄▄░█▀█░▀▄█▒█▄▀░█▄▄",
		"COPILOT": "░▄▀▀░▄▀▄▒█▀▄░█░█▒░░▄▀▄░▀█▀\n░▀▄▄░▀▄▀░█▀▒░█▒█▄▄░▀▄▀░▒█▒",
		"GEMINI":  "░▄▀▒▒██▀░█▄▒▄█░█░█▄░█░█\n░▀▄█░█▄▄░█▒▀▒█░█░█▒▀█░█",
	}
	for word, want := range tests {
		t.Run(word, func(t *testing.T) {
			out, err := ConvertASCIIArt(word, "blocks-in-two-lines-filled")
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("\n%s", out)
			if got := lipgloss.Height(out); got != 2 {
				t.Errorf("%s height = %d, want 2:\n%s", word, got, out)
			}
			if out != want {
				t.Errorf("%s wordmark = %q, want %q", word, out, want)
			}
		})
	}
}

func TestConvertASCIIArt(t *testing.T) {
	t.Parallel()

	asciiArtMap, err := compileASCIIArtMap(ASCIIArtMap{
		Characters: "ab ",
		Rows:       []string{"A ", "B", " ", "1 ", "2", " "},
		Overlap:    true,
		Lowercase:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := convertASCIIArt("AB?", asciiArtMap), "AB\n12"; got != want {
		t.Errorf("converted wordmark = %q, want %q", got, want)
	}
}

//nolint:tparallel // Table subtests intentionally remain serial.
func TestASCIIArtMapStyles(t *testing.T) {
	t.Parallel()

	for mapID, height := range map[string]int{
		"blocks-in-two-lines-filled": 2,
		"blocks-in-two-lines":        2,
		"tiny-2-rows":                2,
		"3-rows":                     3,
		"blurred-black":              3,
		"double-struck":              3,
		"matchsticks":                3,
	} {
		t.Run(mapID, func(t *testing.T) {
			out, err := ConvertASCIIArt("Claude", mapID)
			if err != nil {
				t.Fatal(err)
			}
			if got := lipgloss.Height(out); got != height {
				t.Errorf("height = %d, want %d:\n%s", got, height, out)
			}
		})
	}
}

func TestRenderSidebarProviderWordmark(t *testing.T) {
	t.Parallel()

	wordmark, err := ConvertASCIIArt("Claude", "blocks-in-two-lines-filled")
	if err != nil {
		t.Fatal(err)
	}
	const width = 40
	out := Render(lipgloss.NewStyle(), "v1.0.0", true, Opts{
		Sidebar: true,
		Title:   "Claude",
		Width:   width,
	})
	wordmarkRows := strings.Split(wordmark, "\n")
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[3], wordmarkRows[0]) || !strings.Contains(lines[4], wordmarkRows[1]) {
		t.Errorf("sidebar logo does not contain two-row wordmark:\n%s", out)
	}
	for _, line := range lines[2:5] {
		if got := lipgloss.Width(line); got != width {
			t.Errorf("centered line width = %d, want %d:\n%s", got, width, out)
		}
	}
	if got, want := strings.Index(lines[2], "v1.0.0"), (width-len("v1.0.0"))/2; got != want {
		t.Errorf("version column = %d, want %d:\n%s", got, want, out)
	}
	firstRow, _, _ := strings.Cut(wordmark, "\n")
	if got, want := strings.Index(lines[3], firstRow), (width-lipgloss.Width(firstRow))/2; got != want {
		t.Errorf("wordmark column = %d, want %d:\n%s", got, want, out)
	}
}

func TestRenderDefaultCruxWordmarkCentered(t *testing.T) {
	t.Parallel()

	const width = 64
	wordmark := renderWord(1, -1, LetterC, LetterR, LetterU, LetterX)
	out := ansi.Strip(Render(lipgloss.NewStyle(), "v1.0.0", false, Opts{Width: width}))
	lines := strings.Split(out, "\n")
	wordmarkRows := strings.Split(wordmark, "\n")
	if len(lines) < len(wordmarkRows)+1 {
		t.Fatalf("logo has %d rows, want at least %d:\n%s", len(lines), len(wordmarkRows)+1, out)
	}
	for i, row := range wordmarkRows {
		line := lines[i+1]
		index := strings.Index(line, row)
		if index < 0 {
			t.Fatalf("logo row %d does not contain CRUX row %q:\n%s", i, row, out)
		}
		if got, want := lipgloss.Width(line[:index]), (width-lipgloss.Width(row))/2; got != want {
			t.Errorf("CRUX row %d starts at column %d, want %d:\n%s", i, got, want, out)
		}
	}
}

func TestLetterX(t *testing.T) {
	t.Parallel()

	const want = "▀▄ ▄▀\n ▄▀▄ \n▀   ▀"
	if got := LetterX(false); got != want {
		t.Errorf("LetterX() = %q, want %q", got, want)
	}
}

func TestLettersForUnknownRune(t *testing.T) {
	t.Parallel()

	if _, ok := lettersFor("QUARTZ"); ok {
		t.Error("expected failure for word with missing letterforms")
	}
	if _, ok := lettersFor(""); ok {
		t.Error("expected failure for empty word")
	}
}
