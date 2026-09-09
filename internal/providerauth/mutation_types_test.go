package providerauth

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func validMutationOutcome() (SwitchRequest, MutationOutcome) {
	owner := Owner{ProviderID: "example", HasOAuth: true}
	before := Target{WorkspaceID: "workspace", Owner: owner, Generation: Generation{Epoch: strings.Repeat("a", 32), Sequence: 7}}
	after := before
	after.Generation.Sequence++
	request := SwitchRequest{OperationID: strings.Repeat("b", 32), Target: before, AccountID: "selected"}
	status := Status{Owner: owner, Configured: true, AccountState: "in-sync", ActiveAccountID: "selected", Credentials: []CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "present"}}}
	current := AccountsState{Target: after, Status: status, Accounts: []AccountSummary{{ID: "selected", Active: true, CredentialState: "present"}}}
	models := ModelState{Large: &OwnedModelState{Owner: &owner, Model: config.SelectedModel{Provider: "example", Model: "large", ProviderOptions: map[string]any{"precise": json.Number("9007199254740993"), "false": false, "zero": json.Number("0"), "empty": ""}}}}
	return request, MutationOutcome{OperationID: request.OperationID, Previous: before, Progress: MutationProgress{RuntimePublished: true}, Change: &Change{OperationID: request.OperationID, Previous: before, Current: current, Models: models}}
}

func TestAuthenticationMutationOutcomeValidationAndNumericCopy(t *testing.T) {
	request, outcome := validMutationOutcome()
	require.NoError(t, outcome.ValidateSwitch(request))
	copy, err := cloneMutationOutcome(outcome)
	require.NoError(t, err)
	require.Equal(t, outcome, copy)
	copy.Change.Models.Large.Model.ProviderOptions["precise"] = "changed"
	copy.Change.Current.Status.Credentials[0].State = "absent"
	require.Equal(t, json.Number("9007199254740993"), outcome.Change.Models.Large.Model.ProviderOptions["precise"])
	require.Equal(t, "configured", outcome.Change.Current.Status.Credentials[0].State)
	for _, invalid := range []string{"operation", "previous", "change-binding", "owner", "generation", "account", "credential", "model-owner", "publication"} {
		t.Run(invalid, func(t *testing.T) {
			copy, err := cloneMutationOutcome(outcome)
			require.NoError(t, err)
			switch invalid {
			case "operation":
				copy.OperationID = strings.Repeat("c", 32)
			case "previous":
				copy.Previous.WorkspaceID = "other"
			case "change-binding":
				copy.Change.OperationID = strings.Repeat("c", 32)
			case "owner":
				copy.Change.Current.Target.Owner.ProviderID = "other"
			case "generation":
				copy.Change.Current.Target.Generation = request.Target.Generation
			case "account":
				copy.Change.Current.Status.ActiveAccountID = "other"
			case "credential":
				copy.Change.Current.Status.Credentials[1].State = "absent"
			case "model-owner":
				copy.Change.Models.Large.Owner.ProviderID = "other"
			case "publication":
				copy.Progress.RuntimePublished = false
			}
			require.Error(t, copy.ValidateSwitch(request))
		})
	}
	partial := MutationOutcome{OperationID: request.OperationID, Previous: request.Target, Progress: MutationProgress{AccountsSaved: true}}
	require.NoError(t, partial.ValidateSwitch(request))
	partial.OperationID = strings.Repeat("c", 32)
	require.Error(t, partial.ValidateSwitch(request), "partial errors remain bound to the exact request")
	partial.OperationID = request.OperationID
	partial.Superseded = true
	require.Error(t, partial.Validate(), "superseded requires an actual complete historical receipt")
	request.Target.Owner.HasOAuth = false
	require.Error(t, request.Validate())
}

func TestAuthenticationLogoutOutcomeRequiresActualCredentialAndAccountRemoval(t *testing.T) {
	switchRequest, outcome := validMutationOutcome()
	request := LogoutRequest{OperationID: switchRequest.OperationID, Target: switchRequest.Target}
	require.Error(t, outcome.ValidateLogout(request))
	outcome.Change.Current.Status.ActiveAccountID = ""
	outcome.Change.Current.Status.AccountState = "none"
	outcome.Change.Current.Accounts = []AccountSummary{}
	for i := range outcome.Change.Current.Status.Credentials {
		outcome.Change.Current.Status.Credentials[i].State = "absent"
	}
	outcome.Change.Models = ModelState{} // Unconfigured workspaces need no fabricated selection.
	require.NoError(t, outcome.ValidateLogout(request))
	outcome.Change.Current.Status.Disabled = true
	require.NoError(t, outcome.ValidateLogout(request), "logout preserves a configured disabled provider")
	outcome.Change.Current.Accounts = []AccountSummary{{ID: "remaining", CredentialState: "absent"}}
	require.Error(t, outcome.ValidateLogout(request), "retained inactive accounts are not a completed logout")
}
