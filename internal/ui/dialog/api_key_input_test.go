package dialog

import (
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyProbeDescriptionsAreEvidenceOnly(t *testing.T) {
	for _, tt := range []struct {
		p    config.ConnectionProbeResult
		want string
	}{
		{config.ConnectionProbeResult{Kind: config.ConnectionProbeNotProbed, Policy: config.ConnectionProbePolicyNone}, "No network probe"},
		{config.ConnectionProbeResult{Kind: config.ConnectionProbeFormatOnly, Policy: config.ConnectionProbePolicySKPrefix}, "Format check only"},
		{config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPAttempt, Policy: config.ConnectionProbePolicyHTTP200}, "no response"},
		{config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPResponse, Policy: config.ConnectionProbePolicyNon401, HTTPStatus: 503, AuthorizationOverridden: true}, "HTTP 503 observed under"},
		{config.ConnectionProbeResult{Kind: config.ConnectionProbeUnsupported, Policy: config.ConnectionProbePolicyNone}, "unsupported"},
		{config.ConnectionProbeResult{Kind: config.ConnectionProbeNotProbed, Policy: config.ConnectionProbePolicyManifestHTTP200}, "No declared model-catalog request"},
		{config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPResponse, Policy: config.ConnectionProbePolicyManifestHTTP200, HTTPStatus: 200, EnteredKeyInAuthorization: true}, "declared model-catalog operation"},
		{config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPResponse, Policy: config.ConnectionProbePolicyManifestHTTP200, HTTPStatus: 503, AuthorizationOverridden: true}, "HTTP 503 observed from the declared"},
	} {
		text := APIKeyProbeDescription(tt.p)
		require.Contains(t, text, tt.want)
		require.NotContains(t, strings.ToLower(text), "validated")
		require.Contains(t, text, "does not establish permission")
		if tt.p.AuthorizationOverridden && tt.p.Policy == config.ConnectionProbePolicyManifestHTTP200 {
			require.Contains(t, text, "was not established in that header")
		} else if tt.p.AuthorizationOverridden {
			require.Contains(t, text, "replaced the entered key")
		}
	}
}

func TestAPIKeyCloseHelpDistinguishesCancellationFromAdmittedSave(t *testing.T) {
	theme := styles.ThemeForProvider("checked")
	input, _ := NewAPIKeyInput(&common.Common{Styles: &theme}, false, ActionSelectModel{})
	input.SetPresentation(APIKeyPresentation{Save: true})
	require.Equal(t, "cancel", input.close.Help().Desc)
	input.SetPresentation(APIKeyPresentation{SaveDispatched: true})
	require.Equal(t, "close; receipt retained", input.close.Help().Desc)
}

func TestAPIKeyShortTerminalRetainsActionsAndScrollableEvidence(t *testing.T) {
	for _, onboarding := range []bool{false, true} {
		theme := styles.CharmtonePantera()
		d, _ := NewAPIKeyInput(&common.Common{Styles: &theme}, onboarding, ActionSelectModel{})
		d.SetPresentation(APIKeyPresentation{SaveDispatched: true, Message: strings.Repeat("The configuration was saved but publication is not acknowledged. ", 12), Evidence: "FINAL EVIDENCE", Retry: true, Recover: true, RetryRecovery: true})
		render := func() string {
			screen := uv.NewScreenBuffer(40, 12)
			d.Draw(screen, image.Rect(0, 0, 40, 12))
			return ansi.Strip(screen.Render())
		}
		text := render()
		require.Positive(t, d.details.Height())
		require.Greater(t, d.details.TotalLineCount(), d.details.Height())
		seen := false
		for i := 0; i < 200; i++ {
			text = render()
			for _, hint := range []string{"esc", "ctrl+t", "alt+r", "alt+t", "pgup/pgdn"} {
				require.Contains(t, text, hint, "action clipped at40x12: %s", text)
			}
			if strings.Contains(text, "FINAL EVIDENCE") {
				seen = true
				break
			}
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyPgDown})
		}
		require.True(t, seen, "the full evidence must remain reachable by scrolling")
		require.Positive(t, d.details.YOffset())
	}
}
