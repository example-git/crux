package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

var trimGlamourMarginsSink string

func TestTrimGlamourMarginsMatchesReference(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"",
		"\n",
		"\n\n",
		"text",
		"\ntext\n",
		" \t\ntext\n \t",
		"\x1b[0m\ntext\n\x1b[0m",
		" \n\x1b[31mtext\x1b[0m\n ",
		"first\n\nsecond",
		"\nfirst\n\nsecond\n\n",
	} {
		require.Equal(t, trimGlamourMarginsReference(input), trimGlamourMargins(input))
	}
}

func TestTrimGlamourMarginsUsesBoundedAllocations(t *testing.T) {
	body := strings.Repeat("rendered body line\n", 10_000)
	document := "\n\n" + body + "\n\n"

	tinyAllocations := testing.AllocsPerRun(100, func() {
		trimGlamourMarginsSink = trimGlamourMargins("\n\nrendered body line\n\n")
	})
	largeAllocations := testing.AllocsPerRun(100, func() {
		trimGlamourMarginsSink = trimGlamourMargins(document)
	})

	require.Equal(t, tinyAllocations, largeAllocations)
	require.LessOrEqual(t, largeAllocations, 4.0)
	require.Equal(t, strings.TrimSuffix(body, "\n"), trimGlamourMarginsSink)
}

func TestLiveThinkingSinglePassWrapMatchesPreviousOutput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		input string
		width int
	}{
		{input: "plain text", width: 20},
		{input: "a sentence with enough words to wrap across several lines", width: 12},
		{input: "prefix supercalifragilisticexpialidocious suffix", width: 10},
		{input: "    indented code with a long trailing value", width: 12},
		{input: "first paragraph\n\nsecond paragraph with wrapping", width: 14},
		{input: "wide 世界 emoji 🚀 and more words", width: 12},
		{input: "\x1b[31mcolored words that wrap across lines\x1b[0m", width: 11},
	} {
		previous := ansi.Hardwrap(ansi.Wordwrap(test.input, test.width, ""), test.width, true)
		require.Equal(t, previous, ansi.Wrap(test.input, test.width, ""))
	}
}

func TestThinkingHashIncrementalTracksLineCount(t *testing.T) {
	t.Parallel()

	item := &AssistantMessageItem{}
	for _, test := range []struct {
		thinking string
		lines    int
	}{
		{thinking: "first", lines: 1},
		{thinking: "first\nsecond", lines: 2},
		{thinking: "first\nsecond\n\nthird", lines: 4},
		{thinking: "rewritten\ncontent", lines: 2},
		{thinking: "short", lines: 1},
	} {
		item.thinkingHashIncremental(test.thinking)
		require.Equal(t, test.lines, item.thinkingLineCount)
	}
}

func TestLiveThinkingDefersMarkdownUntilFinished(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	thinking := "# Heading\n\n**bold reasoning**"
	active := &message.Message{
		ID:   "live-thinking",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: thinking},
		},
	}
	item := NewAssistantMessageItem(&sty, active).(*AssistantMessageItem)
	activeRender := ansi.Strip(item.renderThinking(thinking, 80))
	require.Contains(t, activeRender, "# Heading")
	require.Contains(t, activeRender, "**bold reasoning**")
	require.Empty(t, item.streamingThinking.stablePrefix)

	finished := active.Clone()
	finished.Parts = append(finished.Parts, message.Finish{Reason: message.FinishReasonEndTurn})
	item.SetMessage(&finished)
	finishedRender := ansi.Strip(item.renderThinking(thinking, 80))
	require.Contains(t, finishedRender, "Heading")
	require.Contains(t, finishedRender, "bold reasoning")
	require.NotContains(t, finishedRender, "# Heading")
	require.NotContains(t, finishedRender, "**bold reasoning**")
}

func trimGlamourMarginsReference(s string) string {
	lines := strings.Split(s, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(ansi.Strip(lines[start])) == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimSpace(ansi.Strip(lines[end-1])) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}
