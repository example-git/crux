package dialog

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/example-git/crux/foundation/bubbles/key"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/list"
	"github.com/example-git/crux/internal/workspace"
)

// AuthenticationRow contains public, generation-bound authority supplied by
// the owning workspace. Its label is presentation only; it is never an owner.
type AuthenticationRow struct {
	Target    providerauth.Target
	AccountID string
	Label     string
	Info      string
}

// AccountAuthentication is shared by the two maintenance pickers. Construction,
// rendering and input handling do not inspect configuration or account files.
// The main UI owns reads and mutation lifetimes.
type AccountAuthentication struct {
	accountsPicker
	id                           string
	generation                   uint64
	rows                         map[string]AuthenticationRow
	loading, pending, retry      bool
	readNotice, operationNotice  string
	reloadKey, retryKey          key.Binding
	recoverKey, retryRecoveryKey key.Binding
	recover, retryRecovery       bool
	reviewKey                    key.Binding
	review                       bool
}

type (
	AccountSwitcher struct{ *AccountAuthentication }
	Logout          struct{ *AccountAuthentication }
)

func NewAccountSwitcher(com *common.Common) *AccountSwitcher {
	return &AccountSwitcher{newAccountAuthentication(com, AccountSwitcherID)}
}

func NewLogout(com *common.Common) *Logout {
	return &Logout{newAccountAuthentication(com, LogoutID)}
}

func newAccountAuthentication(com *common.Common, id string) *AccountAuthentication {
	d := &AccountAuthentication{accountsPicker: newAccountsPicker(com), id: id, loading: true, readNotice: "Loading authentication status…"}
	d.reloadKey = key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "reload"))
	d.retryKey = key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "retry original"))
	d.recoverKey = key.NewBinding(key.WithKeys("alt+r"), key.WithHelp("alt+r", "attempt recovery"))
	d.retryRecoveryKey = key.NewBinding(key.WithKeys("alt+t"), key.WithHelp("alt+t", "retry recovery"))
	d.reviewKey = key.NewBinding(key.WithKeys("alt+v"), key.WithHelp("alt+v", "review saved state"))
	d.updateNotice()
	return d
}
func (d *AccountAuthentication) ID() string                                  { return d.id }
func (d *AccountAuthentication) AuthenticationState() *AccountAuthentication { return d }
func (d *AccountAuthentication) Generation() uint64                          { return d.generation }
func (d *AccountAuthentication) BeginRead() uint64 {
	d.generation++
	d.loading = true
	d.rows = nil
	d.list.SetItems()
	d.readNotice = "Loading authentication status…"
	d.updateNotice()
	return d.generation
}

func (d *AccountAuthentication) CompleteRead(generation uint64, rows []AuthenticationRow, err error) bool {
	if generation != d.generation {
		return false
	}
	d.loading = false
	d.rows = make(map[string]AuthenticationRow)
	var items []list.FilterableItem
	if err != nil {
		d.readNotice = "Could not load authentication status: " + err.Error() + ". Ctrl+R reloads status."
	} else {
		d.readNotice = ""
		if len(rows) == 0 {
			d.readNotice = "No stored OAuth accounts."
			if d.id == LogoutID {
				d.readNotice = "No configured OAuth credentials to log out."
			}
		}
		for index, row := range rows {
			id := fmt.Sprintf("account-%d", index)
			d.rows[id] = row
			items = append(items, newAccountPickItem(d.com.Styles, id, row.Label, row.Info))
		}
	}
	d.list.SetItems(items...)
	d.list.SetSelected(0)
	d.updateNotice()
	return true
}

func (d *AccountAuthentication) SetOperation(message string, pending, retry bool) {
	d.operationNotice, d.pending, d.retry = message, pending, retry
	d.updateNotice()
}

func (d *AccountAuthentication) SetRecovery(available, retry bool) {
	d.recover, d.retryRecovery = available, retry
	d.updateNotice()
}
func (d *AccountAuthentication) SetReview(available bool) { d.review = available; d.updateNotice() }
func (d *AccountAuthentication) updateNotice() {
	d.notice = strings.TrimSpace(d.operationNotice + "\n" + d.readNotice)
	if d.recover && !d.pending {
		hint := "Alt+R attempt recovery"
		if d.retryRecovery {
			hint += " · Alt+T retry recovery"
		}
		d.notice = hint + "\n" + d.notice
	}
	d.retryKey.SetEnabled(d.retry && !d.pending)
	d.reloadKey.SetEnabled(!d.pending)
	d.recoverKey.SetEnabled(d.recover && !d.pending)
	d.retryRecoveryKey.SetEnabled(d.retryRecovery && !d.pending)
	d.reviewKey.SetEnabled(d.review && !d.pending)
	if d.review && !d.pending {
		d.notice = "Alt+V review saved state\n" + d.notice
	}
}

type ActionAuthenticationSelect struct {
	Dialog     *AccountAuthentication
	Generation uint64
	Row        AuthenticationRow
}
type (
	ActionAuthenticationReload  struct{ Dialog *AccountAuthentication }
	ActionAuthenticationRetry   struct{ Dialog *AccountAuthentication }
	ActionAuthenticationRecover struct {
		Dialog *AccountAuthentication
		Retry  bool
	}
)

func (d *AccountAuthentication) HandleMsg(msg tea.Msg) Action {
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	if action, handled := d.handleNav(kp); handled {
		return action
	}
	if key.Matches(kp, d.reloadKey) {
		return ActionAuthenticationReload{d}
	}
	if key.Matches(kp, d.retryKey) {
		return ActionAuthenticationRetry{d}
	}
	if key.Matches(kp, d.recoverKey) {
		return ActionAuthenticationRecover{Dialog: d}
	}
	if key.Matches(kp, d.retryRecoveryKey) {
		return ActionAuthenticationRecover{Dialog: d, Retry: true}
	}
	if key.Matches(kp, d.reviewKey) {
		return ActionAuthenticationReviewOpen{d}
	}
	if key.Matches(kp, d.keyMap.Select) {
		if d.loading || d.pending {
			return nil
		}
		if pick := d.selected(); pick != nil {
			if row, exists := d.rows[pick.id]; exists {
				return ActionAuthenticationSelect{d, d.generation, row}
			}
		}
		return nil
	}
	return d.handleFilter(kp)
}

func (d *AccountAuthentication) Cursor() *tea.Cursor {
	return InputCursor(d.com.Styles, d.input.Cursor())
}

func (d *AccountAuthentication) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	title := "Switch Account"
	if d.id == LogoutID {
		title = "Log Out"
	}
	return d.draw(scr, area, title, d)
}

func (d *AccountAuthentication) ShortHelp() []key.Binding {
	if d.pending {
		return []key.Binding{d.keyMap.Close}
	}
	if d.retry {
		keys := []key.Binding{d.retryKey}
		if d.review {
			keys = append(keys, d.reviewKey)
		}
		if d.recover {
			keys = append(keys, d.recoverKey)
		}
		if d.retryRecovery {
			keys = append(keys, d.retryRecoveryKey)
		}
		return append(keys, d.reloadKey, d.keyMap.Close)
	}
	return []key.Binding{d.keyMap.Select, d.reloadKey, d.keyMap.Close}
}
func (d *AccountAuthentication) FullHelp() [][]key.Binding { return [][]key.Binding{d.ShortHelp()} }

// LoadAuthenticationRows runs only inside a command. All account targets come
// from one authenticated snapshot, and a failed owner read fails the whole list.
func LoadAuthenticationRows(ctx context.Context, ws workspace.Workspace, logout bool) ([]AuthenticationRow, error) {
	snapshot, err := ws.ProviderAuthentication(ctx)
	if err != nil {
		return nil, err
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	names := make(map[providerauth.Owner]string)
	for _, surface := range ws.ProviderSurfaces() {
		if surface.Owner != nil {
			names[providerauth.PublicOwner(*surface.Owner)] = surface.Name
		}
	}
	var rows []AuthenticationRow
	for _, status := range snapshot.Providers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !status.Owner.HasOAuth {
			continue
		}
		target := providerauth.Target{WorkspaceID: snapshot.WorkspaceID, Generation: snapshot.Generation, Owner: status.Owner}
		name := names[status.Owner]
		if name == "" {
			name = status.Owner.ProviderID
		}
		if logout {
			if !logoutEligible(status) {
				continue
			}
			info := status.AccountState
			if status.Disabled {
				info += " · disabled"
			}
			rows = append(rows, AuthenticationRow{Target: target, Label: name, Info: info})
			continue
		}
		state, err := ws.ProviderAccounts(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := state.Validate(); err != nil {
			return nil, err
		}
		if state.Target != target || !reflect.DeepEqual(state.Status, status) {
			return nil, fmt.Errorf("authentication accounts target changed")
		}
		for _, account := range state.Accounts {
			info := name + " · " + account.CredentialState
			if account.Refreshable {
				info += " · refreshable"
			}
			if account.Active {
				info += " · active · " + state.Status.AccountState
			}
			if state.Status.Disabled {
				info += " · disabled"
			}
			label := account.DisplayName
			if label == "" {
				label = account.ID
			}
			rows = append(rows, AuthenticationRow{Target: target, AccountID: account.ID, Label: label, Info: info})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func logoutEligible(status providerauth.Status) bool {
	if !status.Configured {
		return false
	}
	for _, credential := range status.Credentials {
		if credential.Kind == "api-key" && credential.State == "configured" ||
			credential.Kind == "oauth" && (credential.State == "present" || credential.State == "refresh-only") {
			return true
		}
	}
	return false
}
