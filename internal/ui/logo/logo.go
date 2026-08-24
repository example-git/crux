// Package logo renders Crux and provider wordmarks.
package logo

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/styles"
)

// letterform represents a letterform. It can be stretched horizontally by
// a given amount via the boolean argument.
type letterform func(bool) string

const diag = `╱`

// Opts are the options for rendering the Crux title art.
type Opts struct {
	FieldColor   color.Color // diagonal lines
	TitleColorA  color.Color // left gradient ramp point
	TitleColorB  color.Color // right gradient ramp point
	VersionColor color.Color // version text color
	Width        int         // width of the rendered logo, used for truncation
	Sidebar      bool

	// Title overrides the rendered wordmark (e.g. a provider name such as
	// "CLAUDE"). When set, the word is rendered as bold gradient text
	// instead of the Crux glyph.
	Title string
}

// Render renders the Crux logo. Set the argument to true to render the narrow
// version, intended for use in a sidebar.
//
// The compact argument determines whether it renders compact for the sidebar
// or wider for the main pane.
func Render(base lipgloss.Style, version string, compact bool, o Opts) string {
	fg := func(c color.Color, s string) string {
		return lipgloss.NewStyle().Foreground(c).Render(s)
	}

	// Title.
	const spacing = 1
	wordLetterforms := []letterform{LetterC, LetterR, LetterU, LetterX}
	// Custom wordmark (provider branding): use the letterform art when
	// every letter has a form, otherwise fall back to plain text below.
	var plainTitle string
	var shortTitle string
	if o.Title != "" {
		title := strings.ToUpper(o.Title)
		if o.Sidebar && IsShortTitle(title) {
			wordmark, err := ConvertASCIIArt(title, "blocks-in-two-lines-filled")
			if err == nil {
				shortTitle = wordmark
			}
		}
		if shortTitle == "" {
			if forms, ok := lettersFor(title); ok {
				wordLetterforms = forms
			} else {
				plainTitle = title
			}
		}
	}

	wordmark := renderWord(spacing, -1, wordLetterforms...)
	if shortTitle != "" {
		wordmark = shortTitle
	}
	if plainTitle != "" {
		wordmark = plainTitle
	}
	wordmarkWidth := lipgloss.Width(wordmark)
	centerSidebarContent := compact && (o.Title == "" || o.Sidebar && shortTitle != "")
	contentWidth := wordmarkWidth
	if centerSidebarContent {
		contentWidth = max(contentWidth, o.Width)
	}
	b := new(strings.Builder)
	for r := range strings.SplitSeq(wordmark, "\n") {
		if centerSidebarContent {
			r = lipgloss.PlaceHorizontal(contentWidth, lipgloss.Center, r)
		}
		fmt.Fprintln(b, styles.ApplyForegroundGrad(base, r, o.TitleColorA, o.TitleColorB))
	}
	wordmark = b.String()

	version = ansi.Truncate(version, contentWidth, "…") // truncate version if too long.
	gap := max(0, wordmarkWidth-lipgloss.Width(version))
	metaRow := strings.Repeat(" ", gap) + fg(o.VersionColor, version)
	if centerSidebarContent {
		metaRow = lipgloss.PlaceHorizontal(contentWidth, lipgloss.Center, fg(o.VersionColor, version))
	}

	// Join the meta row and big title.
	wordmark = strings.TrimSuffix(metaRow+"\n"+wordmark, "\n")

	// Narrow version. Fields above and below span the full requested
	// width so the logo block fills its container (e.g. the sidebar).
	if compact {
		fieldWidth := max(wordmarkWidth, o.Width)
		field := fg(o.FieldColor, strings.Repeat(diag, fieldWidth))
		return strings.Join([]string{field, field, wordmark, field, ""}, "\n")
	}

	fieldHeight := lipgloss.Height(wordmark)

	// Left field.
	leftWidth := 6
	rightWidth := 15
	if o.Width > 0 {
		available := max(0, o.Width-wordmarkWidth-2)
		leftWidth = available / 2
		rightWidth = available - leftWidth
	}
	leftFieldRow := fg(o.FieldColor, strings.Repeat(diag, leftWidth))
	leftField := new(strings.Builder)
	for range fieldHeight {
		fmt.Fprintln(leftField, leftFieldRow)
	}

	// Right field.
	const stepDownAt = 0
	rightField := new(strings.Builder)
	for i := range fieldHeight {
		width := rightWidth
		if i >= stepDownAt {
			width = max(0, rightWidth-(i-stepDownAt))
		}
		fmt.Fprint(rightField, fg(o.FieldColor, strings.Repeat(diag, width)), "\n")
	}

	// Return the wide version.
	const hGap = " "
	logo := lipgloss.JoinHorizontal(lipgloss.Top, leftField.String(), hGap, wordmark, hGap, rightField.String())
	if o.Width > 0 {
		// Truncate the logo to the specified width.
		lines := strings.Split(logo, "\n")
		for i, line := range lines {
			lines[i] = ansi.Truncate(line, o.Width, "")
		}
		logo = strings.Join(lines, "\n")
	}
	return logo
}

func IsShortTitle(title string) bool {
	return len([]rune(title)) > 5
}

// SmallRender renders a smaller version of the Crux logo, suitable for
// smaller windows or sidebar usage.
func SmallRender(t *styles.Styles, width int, o Opts) string {
	name := "Crux"
	if o.Title != "" {
		name = o.Title
	}
	gradA, gradB := t.Logo.SmallGradFromColor, t.Logo.SmallGradToColor
	if o.TitleColorA != nil && o.TitleColorB != nil {
		gradA, gradB = o.TitleColorA, o.TitleColorB
	}
	title := styles.ApplyBoldForegroundGrad(t.Logo.GradCanvas, name, gradA, gradB)
	remainingWidth := width - lipgloss.Width(title) - 1 // 1 for the space after the name
	if remainingWidth > 0 {
		lines := strings.Repeat("╱", remainingWidth)
		title = fmt.Sprintf("%s %s", title, t.Logo.SmallDiagonals.Render(lines))
	}
	return title
}
