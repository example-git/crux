package dialog

import (
	"fmt"
	"image"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/foundation/bubbles/textinput"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newOAuthLoginTestDialog(onboarding bool) *OAuthLogin {
	theme := styles.CharmtonePantera()
	return NewOAuthLogin(&common.Common{Styles: &theme}, onboarding, "Selected provider")
}

func renderOAuthLoginTestDialog(d *OAuthLogin, width, height int) (string, *tea.Cursor) {
	screen := uv.NewScreenBuffer(width, height)
	cursor := d.Draw(screen, image.Rect(0, 0, width, height))
	return ansi.Strip(screen.Render()), cursor
}

func TestOAuthLoginActionsFollowPresentation(t *testing.T) {
	d := newOAuthLoginTestDialog(false)
	require.Equal(t, LoginID, d.ID())
	for _, tt := range []struct {
		name string
		key  tea.KeyPressMsg
		p    OAuthLoginPresentation
		want Action
	}{
		{"submit", tea.KeyPressMsg{Code: tea.KeyEnter}, OAuthLoginPresentation{Editable: true}, ActionOAuthLoginSubmit{d}},
		{"open", tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, OAuthLoginPresentation{Open: true}, ActionOAuthLoginOpen{d}},
		{"retry", tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}, OAuthLoginPresentation{Retry: true}, ActionOAuthLoginRetry{d}},
		{"reload", tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl}, OAuthLoginPresentation{Reload: true}, ActionOAuthLoginReload{d}},
		{"recover", tea.KeyPressMsg{Code: 'r', Mod: tea.ModAlt}, OAuthLoginPresentation{Recover: true}, ActionOAuthLoginRecover{Dialog: d}},
		{"retry-recovery", tea.KeyPressMsg{Code: 't', Mod: tea.ModAlt}, OAuthLoginPresentation{RetryRecovery: true}, ActionOAuthLoginRecover{Dialog: d, Retry: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d.SetPresentation(OAuthLoginPresentation{})
			require.Nil(t, d.HandleMsg(tt.key))
			d.SetPresentation(tt.p)
			require.Equal(t, tt.want, d.HandleMsg(tt.key))
			d.SetPresentation(OAuthLoginPresentation{})
			require.Nil(t, d.HandleMsg(tt.key))
		})
	}
	for _, dispatched := range []bool{false, true} {
		d.SetPresentation(OAuthLoginPresentation{CompleteDispatched: dispatched})
		for _, press := range []tea.KeyPressMsg{{Code: tea.KeyEscape}, {Code: 'c', Mod: tea.ModCtrl}} {
			require.Equal(t, ActionClose{}, d.HandleMsg(press))
		}
		if dispatched {
			require.Equal(t, "close; receipt retained", d.close.Help().Desc)
			view, _ := renderOAuthLoginTestDialog(d, 72, 16)
			require.Contains(t, view, "original completion receipt")
		} else {
			require.Equal(t, "cancel", d.close.Help().Desc)
		}
	}
}

func TestOAuthLoginSelectsExactDetachedProviderOwner(t *testing.T) {
	d := newOAuthLoginTestDialog(false)
	providers := []OAuthLoginProvider{
		{Owner: providerauth.Owner{ProviderID: "first"}, Name: "First provider"},
		{Owner: providerauth.Owner{
			ProviderID: "selected", Construction: "plugin", CompatibilityAdapter: "openai",
			HasOAuth: true, OAuthAdapter: "hosted-paste", OAuthFlowID: "literal-flow",
			HasManifest: true, ManifestID: "manifest", ManifestVersion: "2",
			HasPreset: true, PresetID: "preset", PresetVersion: "3", PresetDigest: "exact-digest",
		}, Name: "Selected provider"},
	}
	want := providers[1].Owner
	d.SetProviders(providers)
	providers[1] = OAuthLoginProvider{Owner: providerauth.Owner{ProviderID: "mutated"}, Name: "Mutated"}
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Zero(t, d.selected)
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 1, d.selected)
	require.Equal(t, ActionOAuthLoginSelect{Dialog: d, Owner: want}, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	view, _ := renderOAuthLoginTestDialog(d, 40, 12)
	require.Contains(t, view, "Selected provider")
	require.NotContains(t, view, "Mutated")
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "first", d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionOAuthLoginSelect).Owner.ProviderID)
	d.SetProviders(nil)
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	d.SetPresentation(OAuthLoginPresentation{Editable: true})
	require.Equal(t, ActionOAuthLoginSubmit{d}, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
}

func TestOAuthLoginTransferPreservesOriginalUnboundedInput(t *testing.T) {
	for _, source := range []string{
		"  https://callback.invalid/?code=original&state=unchanged  ",
		strings.Repeat("é🙂", 5000),
		"first\r\nsecond\t\x00\x1b[31mresponse\x7f",
		"invalid-\xff\xc3-UTF8-�-tail",
	} {
		d := newOAuthLoginTestDialog(false)
		d.SetPresentation(OAuthLoginPresentation{Editable: true})
		require.Zero(t, d.input.CharLimit)
		require.Equal(t, textinput.EchoPassword, d.input.EchoMode)
		d.HandleMsg(tea.PasteMsg{Content: source})
		view, _ := renderOAuthLoginTestDialog(d, 40, 12)
		require.NotContains(t, view, source)
		require.NotContains(t, fmt.Sprintf("%+v %#v %s", d, *d, d), source)
		require.Equal(t, source, d.TakeSource())
		require.Empty(t, d.input.Value())
		require.Empty(t, d.source)
		require.Empty(t, d.TakeSource())
		require.Nil(t, d.HandleMsg(tea.PasteMsg{Content: "late paste"}))
		require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'x', Text: "x"}))
		require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
		require.Empty(t, d.TakeSource())
	}
}

func TestOAuthLoginOriginalInputRemainsEditableAtCursor(t *testing.T) {
	d := newOAuthLoginTestDialog(false)
	d.SetPresentation(OAuthLoginPresentation{Editable: true})
	d.HandleMsg(tea.PasteMsg{Content: "a\x00é🙂z"})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyLeft})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyBackspace})
	d.HandleMsg(tea.KeyPressMsg{Code: '界', Text: "界"})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyHome})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDelete})
	d.HandleMsg(tea.PasteMsg{Content: "\xff\t"})
	require.Equal(t, "\xff\t\x00é界z", d.TakeSource())
	for _, tt := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"delete-before", tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}, "🙂z"},
		{"delete-after", tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}, "a\x00é"},
		{"delete-word-before", tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl}, "🙂z"},
		{"delete-word-after", tea.KeyPressMsg{Code: 'd', Mod: tea.ModAlt}, "a\x00é"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d.SetPresentation(OAuthLoginPresentation{Editable: true})
			d.HandleMsg(tea.PasteMsg{Content: "a\x00é🙂z"})
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyLeft})
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyLeft})
			d.HandleMsg(tt.key)
			require.Equal(t, tt.want, d.TakeSource())
		})
	}
}

func TestOAuthLoginClipboardInputRetainsOriginalBytes(t *testing.T) {
	d := newOAuthLoginTestDialog(false)
	press := tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}
	require.Nil(t, d.HandleMsg(press))
	d.SetPresentation(OAuthLoginPresentation{Editable: true})
	require.IsType(t, ActionCmd{}, d.HandleMsg(press))
	const source = "  original\x00\r\n\t\xffresponse  "
	d.HandleMsg(tea.ClipboardMsg{Content: source, Selection: 'c'})
	require.Equal(t, source, d.TakeSource())
	d.HandleMsg(tea.ClipboardMsg{Content: "late clipboard", Selection: 'c'})
	require.Empty(t, d.TakeSource())
}

func TestOAuthLoginCursorTracksUTF8FragmentsJoinedByPaste(t *testing.T) {
	d := newOAuthLoginTestDialog(false)
	d.SetPresentation(OAuthLoginPresentation{Editable: true})
	d.HandleMsg(tea.PasteMsg{Content: "\xc3a"})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyHome})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	d.HandleMsg(tea.PasteMsg{Content: "\xa9"})
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDelete})
	require.Equal(t, "é", d.TakeSource())
}

func TestOAuthLoginShortTerminalRetainsAllActionsAndScrollableStatus(t *testing.T) {
	for _, onboarding := range []bool{false, true} {
		d := newOAuthLoginTestDialog(onboarding)
		d.SetPresentation(OAuthLoginPresentation{
			Message:          strings.Repeat("Waiting for the selected workspace to acknowledge the original completion. ", 10),
			AuthorizationURL: "https://authorization.invalid/authorize?state=exact",
			UserCode:         "FINAL-CODE-1234", Editable: true, Open: true, Retry: true, Reload: true,
			Recover: true, RetryRecovery: true, CompleteDispatched: true,
		})
		d.HandleMsg(tea.PasteMsg{Content: "PRIVATE-RESPONSE"})
		seenURL, seenCode := false, false
		for i := 0; i < 250; i++ {
			view, cursor := renderOAuthLoginTestDialog(d, 40, 12)
			require.Positive(t, d.details.Height())
			require.Greater(t, d.details.TotalLineCount(), d.details.Height())
			require.NotNil(t, cursor)
			require.True(t, image.Pt(cursor.X, cursor.Y).In(image.Rect(0, 0, 40, 12)), "cursor outside dialog")
			require.Contains(t, strings.Split(view, "\n")[cursor.Y], "•", "cursor must remain on the password input row: %s", view)
			for _, hint := range []string{"enter", "ctrl+o", "ctrl+t", "ctrl+n", "alt+r", "alt+t", "esc", "pgup/pgdn"} {
				require.Contains(t, view, hint, "enabled action clipped at 40x12: %s", view)
			}
			require.NotContains(t, view, "PRIVATE-RESPONSE")
			seenURL = seenURL || strings.Contains(view, "authorization.invalid")
			seenCode = seenCode || strings.Contains(view, "FINAL-CODE-1234")
			if seenURL && seenCode {
				break
			}
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyPgDown})
		}
		require.True(t, seenURL, "authorization URL must remain reachable by scrolling")
		require.True(t, seenCode, "device code must remain reachable by scrolling")
		require.Positive(t, d.details.YOffset())
	}
}

func TestOAuthLoginProviderNavigationScrollsSelectedRowIntoView(t *testing.T) {
	d := newOAuthLoginTestDialog(false)
	providers := make([]OAuthLoginProvider, 30)
	for i := range providers {
		providers[i] = OAuthLoginProvider{Name: fmt.Sprintf("Provider %02d", i), Owner: providerauth.Owner{ProviderID: fmt.Sprint(i)}}
	}
	d.SetProviders(providers)
	for range 29 {
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
		renderOAuthLoginTestDialog(d, 40, 12)
	}
	view, _ := renderOAuthLoginTestDialog(d, 40, 12)
	require.Contains(t, view, "Provider 29")
	require.Contains(t, view, "↑/↓")
	require.Equal(t, "29", d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionOAuthLoginSelect).Owner.ProviderID)
	for range 29 {
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	view, _ = renderOAuthLoginTestDialog(d, 40, 12)
	require.Contains(t, view, "Provider 00")
}
