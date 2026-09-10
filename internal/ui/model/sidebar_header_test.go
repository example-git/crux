package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/config"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/stretchr/testify/require"
)

func sidebarFrameText(m *UI) string {
	frame := m.View()
	area := m.layout.sidebar
	lines := strings.Split(frame.Content, "\n")
	var sidebar []string
	for y := area.Min.Y; y < area.Max.Y && y < len(lines); y++ {
		sidebar = append(sidebar, ansi.Strip(ansi.Cut(lines[y], area.Min.X, area.Max.X)))
	}
	return strings.Join(sidebar, "\n")
}

func TestSidebarResourceSpacing(t *testing.T) {
	p, err := NewPreview()
	require.NoError(t, err)
	_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 100})
	require.NoError(t, err)
	m := p.ui
	lines := strings.Split(m.sidebarContent, "\n")
	for _, id := range []string{"model", "session", "lsp", "mcp", "skills"} {
		row, ok := m.sidebarSectionHeaders[id]
		require.True(t, ok, id)
		require.Empty(t, strings.TrimSpace(ansi.Strip(lines[row+1])), "%s should have one heading gap", id)
		require.NotEmpty(t, strings.TrimSpace(ansi.Strip(lines[row+2])), "%s must not add a second blank row", id)
	}
}

func TestSidebarUsageRemainsVisibleBelowLogo(t *testing.T) {
	for _, rows := range []int{100, 45, 25, 15} {
		t.Run(fmt.Sprint(rows), func(t *testing.T) {
			p, err := NewPreview()
			require.NoError(t, err)
			_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: rows})
			require.NoError(t, err)
			m := p.ui
			m.providerUsage = &oauthusage.Usage{Windows: []oauthusage.Window{{Name: "5h", Percent: 25}, {Name: "weekly", Percent: 60}}}
			// A prior collapsed usage section or collapsed model must not hide
			// provider quota now that it belongs to the header.
			m.sidebarCollapsed = map[string]bool{"model": true, "usage": true}
			frame := sidebarFrameText(m)
			require.Contains(t, frame, "75% left")
			require.Contains(t, frame, "40% left")
			require.NotContains(t, frame, "Provider Usage")
			require.NotContains(t, m.sidebarSectionHeaders, "usage")
			if modelRow := strings.Index(frame, "Model / Context"); modelRow >= 0 {
				require.Less(t, strings.Index(frame, "40% left"), modelRow)
			}
			logoHeight := 0
			if m.sidebarDrawLogo != "" {
				logoHeight = lipgloss.Height(m.sidebarDrawLogo)
			}
			click := tea.MouseClickMsg{X: m.layout.sidebar.Min.X + 2, Y: m.layout.sidebar.Min.Y + 1 + logoHeight, Button: uv.MouseLeft}
			require.False(t, m.handleSidebarSectionClick(click), "quota rows are not collapsible headings")
			require.False(t, m.handleSidebarLogoClick(click), "quota rows are outside the logo toggle")
			for _, usage := range []*oauthusage.Usage{nil, {}} {
				m.providerUsage = usage
				frame = sidebarFrameText(m)
				require.NotContains(t, frame, "% left")
				require.NotContains(t, frame, "Provider Usage")
			}
		})
	}
}

func TestUsageBarsKeepLongWindowNamesVisible(t *testing.T) {
	m := newTestUI()
	m.providerUsage = &oauthusage.Usage{Windows: []oauthusage.Window{{Name: "A long provider quota window", Percent: 30}, {Name: "週間使用量", Percent: 100}}}
	for _, width := range []int{30, 12, 6} {
		lines := strings.Split(m.usageBars(width, true), "\n")
		require.Len(t, lines, 2)
		require.Contains(t, ansi.Strip(lines[0]), "70%")
		require.Contains(t, ansi.Strip(lines[1]), "0%")
		for _, line := range lines {
			require.LessOrEqual(t, ansi.StringWidth(line), width)
		}
	}
}

func TestSidebarAuthorityKeysBoldValuesNormal(t *testing.T) {
	for _, mode := range []string{"client", "server"} {
		t.Run(mode, func(t *testing.T) {
			p, err := NewPreview()
			require.NoError(t, err)
			authority := &config.RemoteAuthority{Mode: mode, Revision: 7, Principal: strings.Repeat("abcdef012345", 4), Accounts: []config.RemoteAccountIdentity{{ProviderID: "dummy-preview", AccountID: "chosen-account-with-a-long-wrapped-label"}}}
			data, err := json.Marshal(map[string]any{"authority": authority, "settings": map[string]any{"debug": true}})
			require.NoError(t, err)
			frame, err := p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 100, Data: data})
			require.NoError(t, err)
			m := p.ui
			screen := uv.NewScreenBuffer(frame.Cols, frame.Rows)
			uv.NewStyledString(frame.Content).Draw(screen, screen.Bounds())
			contentTop := m.layout.sidebar.Min.Y + 1 + lipgloss.Height(m.sidebarDrawLogo)
			start := contentTop + m.sidebarSectionHeaders["authority"] + 2
			end := contentTop + m.sidebarFilesHeaderLine
			var actual []uv.Cell
			for y := start; y < end; y++ {
				for x := m.layout.sidebar.Min.X + 2; x < m.layout.sidebar.Max.X-2; x++ {
					cell := screen.CellAt(x, y)
					if cell != nil && strings.TrimSpace(cell.Content) != "" {
						actual = append(actual, *cell)
					}
				}
			}
			type span struct {
				text string
				bold bool
			}
			spans := []span{{workspaceAuthorityLabel(authority), false}, {"Client fingerprint:", true}, {authority.Principal, false}, {"Credentials:", true}}
			if mode == "client" {
				spans = append(spans, span{"owning client", false}, span{"Selected account:", true}, span{authority.Accounts[0].AccountID, false})
			} else {
				spans = append(spans, span{"execution server", false})
			}
			index := 0
			for _, part := range spans {
				for _, char := range part.text {
					if unicode.IsSpace(char) {
						continue
					}
					require.Less(t, index, len(actual))
					require.Equal(t, string(char), actual[index].Content)
					require.Equal(t, part.bold, actual[index].Style.Attrs&uv.AttrBold != 0, "span %q character %d", part.text, index)
					index++
				}
			}
			require.Len(t, actual, index)
		})
	}
}
