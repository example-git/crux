package workspace

import (
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthenticationReconciliationPublicAcknowledgements(t *testing.T) {
	f, original := rejectedAuthenticationReviewFixture(t, false)
	request := ProviderAuthenticationReviewRequest(authenticationReviewAction(original.OperationID, original.Target, 1))
	var capability ProviderAuthenticationReconciler = f.w
	require.True(t, capability.CanReconcileProviderAuthentication())
	summary, err := capability.ReviewProviderAuthentication(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, summary.Validate(request))
	wrong := summary
	wrong.Choice = ProviderAuthenticationReviewChoice{Kind: "saved-logout"}
	require.ErrorIs(t, wrong.Validate(request), providerauth.ErrReceiptUnverified)
	apply := ProviderAuthenticationApplyRequest(authenticationReviewApply(clientAuthenticationReviewRequest(request), summary))
	outcome, err := capability.ApplyProviderAuthenticationReview(t.Context(), apply)
	require.NoError(t, err)
	require.NoError(t, outcome.Validate(apply))
	outcome.RemoteAcknowledged = false
	require.ErrorIs(t, outcome.Validate(apply), providerauth.ErrReceiptUnverified)
	require.False(t, (&ClientWorkspace{}).CanReconcileProviderAuthentication())
	_, err = (&ClientWorkspace{}).ReviewProviderAuthentication(t.Context(), request)
	require.ErrorContains(t, err, "server-owned and local workspaces are not supported")
	_, err = (&ClientWorkspace{}).ApplyProviderAuthenticationReview(t.Context(), apply)
	require.ErrorContains(t, err, "server-owned and local workspaces are not supported")
	wrong = summary
	wrong.Receiver = config.RemoteAuthority{Mode: "server", Principal: "other", Revision: 3, Digest: strings.Repeat("a", 64)}
	require.ErrorIs(t, wrong.Validate(request), providerauth.ErrReceiptUnverified)
}
