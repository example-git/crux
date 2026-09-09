package providerauth

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func oauthLoginRefFor(target Target) OAuthLoginRef {
	return OAuthLoginRef{LoginID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Target: target}
}

func TestOAuthLoginStatePhaseAndPrivateFormatting(t *testing.T) {
	request, _ := validMutationOutcome()
	ref := oauthLoginRefFor(request.Target)
	ref.Target.Owner.OAuthAdapter = providerregistry.LoginBrowser
	callback := &OAuthLoginCallback{Mode: "loopback-dynamic", Path: "/oauth%2Fcallback"}
	state := OAuthLoginState{Login: ref, Sequence: 1, Phase: OAuthLoginPreparing}
	require.NoError(t, state.Validate(), "zero expiry preserves adapters with no overall deadline")
	state.Phase, state.Callback = OAuthLoginWaitingLoopback, callback
	require.NoError(t, state.Validate())
	state.Phase, state.AuthorizationURL = OAuthLoginWaitingBrowser, "https://auth.example/authorize?state=private-marker"
	require.NoError(t, state.Validate())
	copy := cloneOAuthLoginState(state)
	copy.Callback.Path = "/different"
	require.Equal(t, "/oauth%2Fcallback", state.Callback.Path)
	for _, phase := range []OAuthLoginPhase{OAuthLoginAuthorizing, OAuthLoginAuthorized, OAuthLoginCommitting, OAuthLoginComplete, OAuthLoginFailed, OAuthLoginExpired, OAuthLoginCanceled} {
		copy := state
		copy.Phase = phase
		require.Error(t, copy.Validate(), "non-interactive phase cannot retain its URL or callback")
		copy.Callback, copy.AuthorizationURL = nil, ""
		require.NoError(t, copy.Validate())
	}
	code := OAuthLoginCodeRequest{Login: ref, SubmissionID: strings.Repeat("c", 32), Input: "code=private-input&state=private-state"}
	require.NoError(t, code.Validate())
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		require.NotContains(t, fmt.Sprintf(format, state), "private-marker")
		require.NotContains(t, fmt.Sprintf(format, code), "private-input")
	}
	encoded, err := json.Marshal(code)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "private-input", "authenticated private routes carry the callback input intentionally")
	for _, invalid := range []string{"", strings.Repeat("a", OAuthLoginInputLimit+1), "code=bad\x00", string([]byte{0xff})} {
		copy := code
		copy.Input = invalid
		require.Error(t, copy.Validate())
	}
	state.Callback, state.AuthorizationURL = nil, "https://auth.example/device"
	state.Phase, state.UserCode = OAuthLoginWaitingDevice, "private-user-code"
	require.Error(t, state.Validate(), "browser session cannot become a device session")
	state.Login.Target.Owner.OAuthAdapter = providerregistry.LoginDeviceCode
	require.NoError(t, state.Validate())
	code.Login = state.Login
	require.Error(t, code.Validate(), "device polling does not accept a caller token/code")
}

func TestOAuthLoginOutcomeRequiresExactLoginAndEstablishedEffects(t *testing.T) {
	request, original := validMutationOutcome()
	request.Target.Owner.OAuthAdapter = providerregistry.LoginBrowser
	ref := oauthLoginRefFor(request.Target)
	original.Previous = ref.Target
	original.Change.Previous = ref.Target
	original.Change.Current.Target.Owner = ref.Target.Owner
	original.Change.Current.Status.Owner = ref.Target.Owner
	original.LoginID = ref.LoginID
	original.Progress = MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}
	require.NoError(t, original.ValidateOAuthLogin(ref))
	for _, variant := range []string{"other-login", "key-receipt", "missing-config", "missing-account", "absent-oauth", "partial", "no-namespace"} {
		t.Run(variant, func(t *testing.T) {
			copy, err := cloneMutationOutcome(original)
			require.NoError(t, err)
			switch variant {
			case "other-login":
				copy.LoginID = strings.Repeat("d", 32)
			case "key-receipt":
				copy.CheckID = strings.Repeat("e", 32)
			case "missing-config":
				copy.Progress.ConfigSaved = false
			case "missing-account":
				copy.Progress.AccountsSaved = false
			case "absent-oauth":
				copy.Change.Current.Status.Credentials[1].State = "absent"
			case "partial":
				copy.Change = nil
				copy.Progress.RuntimePublished = false
				require.NoError(t, copy.ValidateOAuthLogin(ref))
				return
			case "no-namespace":
				copy.Progress.AccountsSaved = false
				copy.Change.Current.Status.AccountState = "none"
				copy.Change.Current.Status.ActiveAccountID = ""
				copy.Change.Current.Accounts = []AccountSummary{}
				require.NoError(t, copy.ValidateOAuthLogin(ref))
				return
			}
			require.Error(t, copy.ValidateOAuthLogin(ref))
		})
	}
	logout := LogoutRequest{OperationID: ref.OperationID, Target: ref.Target}
	require.Error(t, original.ValidateLogout(logout), "another mutation route cannot accept a login receipt")
}
