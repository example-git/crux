package model

import (
	"testing"

	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

func TestQuestionNotificationPreservesNextConsentForm(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "notification before consent"
		if late {
			name = "notification after consent"
		}
		t.Run(name, func(t *testing.T) {
			u := newTestUIForPermissions()
			u.com.Workspace = attachmentClickWorkspace{}
			open := func(id, text string) {
				u.openBatchFormDialog(question.Request{
					ID: id, ToolCallID: "fetch-call",
					Questions: []question.Question{{ID: id + "-question", Type: question.TypeSingleChoice, Text: text, Description: "Synthetic browser fetch prompt", Choices: []question.Choice{{ID: "yes", Label: "Allow"}, {ID: "no", Label: "Deny"}}}},
				})
			}
			notify := func(id string) {
				_, _ = u.Update(pubsub.Event[question.Notification]{Type: pubsub.CreatedEvent, Payload: question.Notification{BatchID: id}})
			}
			open("profile", "Which browser profile?")
			if !late {
				notify("profile")
				require.Nil(t, u.activeInline)
			}
			open("consent", "Allow browser session access?")
			form := u.activeInline.(*dialog.QuestionForm)
			if late {
				notify("profile")
			}
			require.Same(t, form, u.activeInline, "profile completion must not hide pending consent")
			notify("")
			require.Same(t, form, u.activeInline, "unidentified completion must not hide consent")
			notify("unrelated")
			require.Same(t, form, u.activeInline)
			notify("consent")
			require.Nil(t, u.activeInline, "matching completion must close consent")
			notify("consent")
			require.Nil(t, u.activeInline)
		})
	}
}
