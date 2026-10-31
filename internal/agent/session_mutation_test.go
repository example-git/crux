package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBeginSessionMutationRejectsDuplicateReservation(t *testing.T) {
	t.Parallel()
	sa, _ := newCancelTestAgent(t)

	release, err := sa.beginSessionMutation("session")
	require.NoError(t, err)

	_, err = sa.beginSessionMutation("session")
	require.ErrorIs(t, err, ErrSessionMutationInProgress)

	release()
	release()

	release, err = sa.beginSessionMutation("session")
	require.NoError(t, err)
	release()
}

func TestBeginSessionMutationRejectsSessionWork(t *testing.T) {
	t.Parallel()
	sa, _ := newCancelTestAgent(t)

	accepted := sa.BeginAccepted("accepted")
	_, err := sa.beginSessionMutation("accepted")
	require.ErrorIs(t, err, ErrSessionBusy)
	accepted.Close()

	sa.activeRequests.Set("active", &activeCancel{cancel: func() {}})
	_, err = sa.beginSessionMutation("active")
	require.ErrorIs(t, err, ErrSessionBusy)
	sa.activeRequests.Del("active")

	sa.messageQueue.Set("queued", []SessionAgentCall{{SessionID: "queued", Prompt: "later"}})
	_, err = sa.beginSessionMutation("queued")
	require.ErrorIs(t, err, ErrSessionBusy)
	sa.messageQueue.Del("queued")

	release, err := sa.beginSessionMutation("idle")
	require.NoError(t, err)
	release()
}
