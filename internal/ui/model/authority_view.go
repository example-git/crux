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
	type field struct{ key, value string }
	fields := []field{{value: workspaceAuthorityLabel(authority)}}
	if authority.Principal != "" {
		fields = append(fields, field{key: "Client fingerprint:"}, field{value: authority.Principal})
	}
	if authority.Mode == "client" {
		fields = append(fields, field{key: "Credentials:", value: "owning client"})
		if cfg := m.com.Config(); cfg != nil {
			provider := cfg.Models[config.SelectedModelTypeLarge].Provider
			for _, account := range authority.Accounts {
				if account.ProviderID == provider && account.AccountID != "" {
					fields = append(fields, field{key: "Selected account:", value: account.AccountID})
					break
				}
			}
		}
	} else if authority.Mode == "server" {
		fields = append(fields, field{key: "Credentials:", value: "execution server"})
	}
	// Account labels are presentation data, never terminal control sequences.
	valueStyle := m.com.Styles.Sidebar.SessionTitle.Bold(false)
	keyStyle := valueStyle.Bold(true)
	lines := make([]string, len(fields))
	for i, field := range fields {
		value := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, ansi.Strip(field.value))
		line := keyStyle.Render(field.key)
		if field.key != "" && value != "" {
			line += " "
		}
		line += valueStyle.Render(value)
		lines[i] = valueStyle.Width(width).Render(ansi.Hardwrap(line, width, true))
	}
	return strings.Join(lines, "\n")
}
