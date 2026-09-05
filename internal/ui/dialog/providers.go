package dialog

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/util"
)

// ProvidersID is the identifier for the providers dialog.
const ProvidersID = "providers"

// ActionProviderToggled is returned when a provider's enabled state changes.
type ActionProviderToggled struct {
	ProviderID string
	Disabled   bool
}

// providerEntry is one row in the providers list.
type providerEntry struct {
	id       string
	name     string
	disabled bool
	owner    providerregistry.RegistrationOwner
	ownerSet bool
}

// Providers is a dialog that lets the user enable or disable providers.
type Providers struct {
	com      *common.Common
	items    []providerEntry
	cursor   int
	keyMap   providersKeyMap
	maxWidth int
}

type providersKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Toggle key.Binding
	Close  key.Binding
}

var _ Dialog = (*Providers)(nil)

// NewProviders builds the providers dialog from the current config.
func NewProviders(com *common.Common) *Providers {
	p := &Providers{
		com:      com,
		maxWidth: 50,
		keyMap: providersKeyMap{
			Up: key.NewBinding(
				key.WithKeys("up", "k"),
				key.WithHelp("↑/k", "up"),
			),
			Down: key.NewBinding(
				key.WithKeys("down", "j"),
				key.WithHelp("↓/j", "down"),
			),
			Toggle: key.NewBinding(
				key.WithKeys("enter", " "),
				key.WithHelp("space/enter", "toggle"),
			),
			Close: CloseKey,
		},
	}
	p.refresh()
	return p
}

// refresh rebuilds the items list from the current config. This is called
// at construction and after every toggle so the list always reflects the
// persisted state.
func (p *Providers) refresh() {
	cfg := p.com.Config()
	knownProviders, _ := config.Providers(cfg)
	p.items = providerEntries(cfg, knownProviders)
	// Clamp cursor if the list shrank.
	if p.cursor >= len(p.items) {
		p.cursor = max(0, len(p.items)-1)
	}
}

func providerEntries(cfg *config.Config, knownProviders []catalog.Provider) []providerEntry {
	seen := make(map[string]bool)
	var items []providerEntry

	// Configured providers (includes those only in the JSON file). Preserve
	// ordinary disabled providers so users can re-enable them, but omit
	// plugin/OAuth integrations the active host profile cannot construct.
	for id, pc := range cfg.Providers.Seq2() {
		seen[id] = true
		if !cfg.IsProviderIntegrationAvailable(id) {
			continue
		}
		name := pc.Name
		if name == "" {
			name = id
		}
		owner, ownerSet := cfg.ProviderOwner(id)
		items = append(items, providerEntry{id: id, name: name, disabled: pc.Disable, owner: owner, ownerSet: ownerSet})
	}

	// Known but unconfigured providers remain visible so the user can
	// pre-disable them. The profile-aware catalog has already omitted inactive
	// plugin integrations.
	for _, provider := range knownProviders {
		id := string(provider.ID)
		if seen[id] {
			continue
		}
		owner, ownerSet := cfg.ProviderOwner(id)
		items = append(items, providerEntry{id: id, name: provider.Name, owner: owner, ownerSet: ownerSet})
	}

	slices.SortFunc(items, func(a, b providerEntry) int {
		return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	return items
}

// ID implements [Dialog].
func (*Providers) ID() string { return ProvidersID }

// HandleMsg implements [Dialog].
func (p *Providers) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, p.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, p.keyMap.Up):
			if p.cursor > 0 {
				p.cursor--
			}
		case key.Matches(msg, p.keyMap.Down):
			if p.cursor < len(p.items)-1 {
				p.cursor++
			}
		case key.Matches(msg, p.keyMap.Toggle):
			if p.cursor >= len(p.items) {
				break
			}
			item := p.items[p.cursor]
			cfg := p.com.Config()
			if cfg == nil {
				return ActionCmd{util.ReportError(fmt.Errorf("configuration not found"))}
			}
			currentOwner, currentOwnerSet := cfg.ProviderOwner(item.id)
			if !item.ownerSet {
				p.refresh()
				return ActionCmd{util.ReportError(fmt.Errorf("provider owner is missing for %s", item.id))}
			}
			if currentOwnerSet != item.ownerSet || currentOwnerSet && currentOwner != item.owner {
				p.refresh()
				return ActionCmd{util.ReportError(fmt.Errorf("provider owner changed before %s could be toggled", item.id))}
			}
			newDisabled := !item.disabled

			if err := p.com.Workspace.SetProviderDisabled(config.ScopeGlobal, item.owner, newDisabled); err != nil {
				p.refresh()
				return ActionCmd{util.ReportError(fmt.Errorf("failed to toggle provider %s: %w", item.id, err))}
			}
			p.refresh()

			return ActionProviderToggled{
				ProviderID: item.id,
				Disabled:   newDisabled,
			}
		}
	}
	return nil
}

// Draw implements [Dialog].
func (p *Providers) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := p.com.Styles

	title := "Providers"
	titleStyle := t.Dialog.TitleText

	var rows []string
	for i, item := range p.items {
		check := "✓"
		if item.disabled {
			check = " "
		}

		label := fmt.Sprintf(" [%s] %s", check, item.name)

		var style lipgloss.Style
		if i == p.cursor {
			style = t.Dialog.SelectedItem
		} else {
			style = t.Dialog.NormalItem
		}
		rows = append(rows, style.Render(label))
	}

	if len(rows) == 0 {
		rows = append(rows, t.Dialog.SecondaryText.Render("  No providers found"))
	}

	hint := t.Dialog.SecondaryText.Render("  space/enter to toggle · esc to close")

	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	content := lipgloss.JoinVertical(
		lipgloss.Left,
		titleStyle.Render(title),
		"",
		body,
		"",
		hint,
	)

	frameStyle := t.Dialog.View
	view := frameStyle.MaxWidth(p.maxWidth).Render(content)
	DrawCenter(scr, area, view)
	return nil
}

// ShortHelp implements [help.KeyMap].
func (p *Providers) ShortHelp() []key.Binding {
	return []key.Binding{p.keyMap.Up, p.keyMap.Down, p.keyMap.Toggle, p.keyMap.Close}
}

// FullHelp implements [help.KeyMap].
func (p *Providers) FullHelp() [][]key.Binding {
	return [][]key.Binding{{p.keyMap.Up, p.keyMap.Down, p.keyMap.Toggle, p.keyMap.Close}}
}
