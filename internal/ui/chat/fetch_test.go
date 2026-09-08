package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestFetchToolShowsBrowserMode(t *testing.T) {
	sty := styles.CharmtonePantera()
	renderer := &FetchToolRenderContext{}
	for _, test := range []struct {
		input string
		user  bool
	}{
		{`{"url":"https://example.test","mode":"user"}`, true},
		{`{"url":"https://example.test"}`, false},
	} {
		output := ansi.Strip(renderer.RenderTool(&sty, 120, &ToolRenderOpts{
			ToolCall: message.ToolCall{Name: "fetch", Input: test.input, Finished: true},
			Status:   ToolStatusSuccess, Compact: true,
		}))
		require.Contains(t, output, "example.test")
		if test.user {
			require.Contains(t, output, "mode")
			require.Contains(t, output, "user")
		} else {
			require.NotContains(t, output, "mode")
		}
	}
	require.Equal(t, "fetch.md", getFileExtensionForFormat(""))
}
