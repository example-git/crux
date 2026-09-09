package proto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestProviderAccountRemovalProtocolExactEffect(t *testing.T) {
	switchRequest, response := mutationProtocolFixture()
	request := providerauth.RemoveRequest{OperationID: switchRequest.OperationID, Target: switchRequest.Target, AccountID: "removed"}
	response.Outcome.RemovedAccountID = request.AccountID
	response.Outcome.Progress = providerauth.MutationProgress{AccountsSaved: true}
	require.NoError(t, response.ValidateRemove(request))
	data, err := json.Marshal(response)
	require.NoError(t, err)
	result, err := DecodeProviderAuthRemoveResponse(data, request)
	require.NoError(t, err)
	require.Equal(t, response.Outcome, result.Outcome)
	for _, body := range []string{
		strings.Replace(string(data), `"removed_account_id":"removed"`, `"removed_account_id":"other"`, 1),
		strings.Replace(string(data), `"removed_account_id":"removed",`, "", 1),
		strings.Replace(string(data), `"removed_account_id":"removed"`, `"removed_account_id":"removed","removed_account_id":"other"`, 1),
		strings.Replace(string(data), `"accounts_saved":true`, `"accounts_saved":false`, 1),
		strings.Replace(string(data), `"config_saved":false`, `"config_saved":true`, 1),
		strings.ReplaceAll(string(data), `"selected"`, `"removed"`),
	} {
		_, err := DecodeProviderAuthRemoveResponse([]byte(body), request)
		require.Error(t, err)
	}
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	decoded, err := DecodeProviderAuthRemoveRequest(raw)
	require.NoError(t, err)
	require.Equal(t, request, decoded)
	for _, raw := range []string{"null", string(raw) + "{}", strings.Replace(string(raw), `"account_id":"removed"`, `"account_id":""`, 1), strings.Repeat(" ", MaxProviderAuthRequestBytes) + string(raw)} {
		_, err := DecodeProviderAuthRemoveRequest([]byte(raw))
		require.Error(t, err)
	}
}
