package model

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/workspace"
)

func cachedWorkspaceAuthority(com *common.Common) *config.RemoteAuthority {
	if com == nil || com.Workspace == nil {
		return nil
	}
	view, ok := com.Workspace.(workspace.AuthorityViewProvider)
	if !ok {
		return nil
	}
	return view.AcceptedAuthority()
}

func workspaceAuthorityLabel(authority *config.RemoteAuthority) string {
	if authority == nil {
		return ""
	}
	switch authority.Mode {
	case "client":
		return fmt.Sprintf("Client providers · r%d", authority.Revision)
	case "server":
		return "Server providers"
	default:
		return "Provider authority unavailable"
	}
}

func (m *UI) workspaceAuthorityInfo(width int) string {
	authority := cachedWorkspaceAuthority(m.com)
	if authority == nil || width <= 0 {
		return ""
	}
	lines := []string{workspaceAuthorityLabel(authority)}
	if authority.Principal != "" {
		lines = append(lines, "Client fingerprint:", authority.Principal)
	}
	if authority.Mode == "client" {
		lines = append(lines, "Credentials: owning client")
		if cfg := m.com.Config(); cfg != nil {
			provider := cfg.Models[config.SelectedModelTypeLarge].Provider
			for _, account := range authority.Accounts {
				if account.ProviderID == provider && account.AccountID != "" {
					lines = append(lines, "Selected account: "+account.AccountID)
					break
				}
			}
		}
	} else if authority.Mode == "server" {
		lines = append(lines, "Credentials: execution server")
	}
	// Account labels are presentation data, never terminal control sequences.
	for i, line := range lines {
		line = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, ansi.Strip(line))
		lines[i] = m.com.Styles.Sidebar.SessionTitle.Width(width).Render(ansi.Hardwrap(line, width, true))
	}
	return strings.Join(lines, "\n")
}
