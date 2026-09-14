package model

import (
	"strings"
	"testing"

	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func TestStatusToastErrorsPopulateRecentErrorsDialog(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.dialog = dialog.NewOverlay()
	u.status.SetProblemHandler(u.recordStatusProblem)

	long := "status code 500: " + strings.Repeat("provider detail ", 40)
	// Direct status writes, bypassing the InfoMsg update case, must be
	// captured too: this is the red toast drawn over the key map.
	u.status.SetInfoMsg(util.InfoMsg{Type: util.InfoTypeError, Msg: long})
	u.status.SetInfoMsg(util.InfoMsg{Type: util.InfoTypeWarn, Msg: "reconnecting"})
	u.status.SetInfoMsg(util.InfoMsg{Type: util.InfoTypeInfo, Msg: "Copied"})

	require.Len(t, u.recentErrors, 2)
	require.Equal(t, long, u.recentErrors[0].Message)
	require.Equal(t, "warning: reconnecting", u.recentErrors[1].Message)

	u.openErrorsDialog()
	d, ok := u.dialog.Dialog(dialog.ErrorsID).(*dialog.Errors)
	require.True(t, ok)
	require.Contains(t, d.Latest(), "reconnecting")

	// New errors while the dialog is open are pushed into it.
	u.status.SetInfoMsg(util.InfoMsg{Type: util.InfoTypeError, Msg: "second failure"})
	require.Equal(t, "second failure", d.Latest())

	// Toast rendering keeps the expand hint when there is room.
	require.Contains(t, u.status.renderInfo(120), "/errors")
}

func TestAssistantErrorBannerPopulatesRecentErrors(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.dialog = dialog.NewOverlay()
	msg := &message.Message{ID: "m1", Role: message.Assistant, Parts: []message.ContentPart{
		message.Finish{Reason: message.FinishReasonError, Message: "request failed", Details: "status code 500: boom"},
	}}
	u.recordMessageError(msg)
	u.recordMessageError(msg)
	require.Len(t, u.recentErrors, 1)
	require.Equal(t, "request failed\nstatus code 500: boom", u.recentErrors[0].Message)

	msg.Parts = []message.ContentPart{message.Finish{Reason: message.FinishReasonError, Message: "request failed again"}}
	u.recordMessageError(msg)
	require.Len(t, u.recentErrors, 1)
	require.Equal(t, "request failed again", u.recentErrors[0].Message)
}
