package styles

import (
	"strings"
	"testing"

	"charm.land/glamour/v2"
	glamouransi "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/x/ansi"
)

// Blockquote continuation lines must keep the "│ " indent token and its
// styling whether the line came from ordinary word wrapping or from
// hard-breaking an unbreakable token. Reserving the token width as Margin
// (2 cells) instead of Indent (1 cell) keeps prefixed lines within the
// render width so the enclosing document never re-wraps them apart from
// their token.
func TestBlockquoteWrappedLinesKeepIndentToken(t *testing.T) {
	t.Parallel()

	const width = 30
	const doc = "> this is a fairly long quoted sentence that must wrap across lines\n" +
		">\n" +
		"> supercalifragilisticexpialidociouslongtoken and more words after\n\n" +
		"plain trailing paragraph"

	sty := CharmtonePantera()
	for name, cfg := range map[string]glamouransi.StyleConfig{
		"markdown":      sty.Markdown,
		"quietMarkdown": sty.QuietMarkdown,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r, err := glamour.NewTermRenderer(
				glamour.WithStyles(cfg),
				glamour.WithWordWrap(width),
			)
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.Render(doc)
			if err != nil {
				t.Fatal(err)
			}

			var quoteLines, hardBroken int
			for _, line := range strings.Split(ansi.Strip(out), "\n") {
				trimmed := strings.TrimRight(line, " ")
				if trimmed == "" || strings.HasPrefix(trimmed, "plain") {
					continue
				}
				quoteLines++
				if !strings.HasPrefix(trimmed, "│") {
					t.Errorf("quote line lost its indent token: %q", trimmed)
				}
				if got := ansi.StringWidth(trimmed); got > width {
					t.Errorf("quote line width = %d, want <= %d: %q", got, width, trimmed)
				}
				if strings.Contains(trimmed, "supercali") || strings.Contains(trimmed, "ciouslongtoken") {
					hardBroken++
				}
			}
			if quoteLines < 5 {
				t.Fatalf("expected at least 5 wrapped quote lines, got %d:\n%s", quoteLines, ansi.Strip(out))
			}
			if hardBroken < 2 {
				t.Fatalf("expected the long token to hard-wrap into prefixed lines:\n%s", ansi.Strip(out))
			}
		})
	}
}
