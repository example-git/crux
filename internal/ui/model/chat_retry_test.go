package model

import (
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/anim"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/stretchr/testify/require"
)

func TestChatRoutesRetryAnimationTickToOwningAssistant(t *testing.T) {
	t.Parallel()

	com := common.DefaultCommon(nil)
	model := NewChat(com, config.ScrollbarDefault)
	model.SetSize(100, 20)

	msg := &message.Message{ID: "assistant-retry", Role: message.Assistant}
	msg.SetRetrying()
	item := chat.NewAssistantMessageItem(com.Styles, msg)
	model.AppendMessages(item)

	animatable := item.(chat.Animatable)
	start := animatable.StartAnimation()
	require.NotNil(t, start)
	step, ok := start().(anim.StepMsg)
	require.True(t, ok)
	require.Equal(t, "assistant-retry-retry", step.ID)

	beforeVersion := item.Version()
	beforeRender := item.RawRender(100)
	next := model.Animate(step)
	require.NotNil(t, next, "retry tick must route through the chat model")
	require.Greater(t, item.Version(), beforeVersion)
	require.NotEqual(t, beforeRender, item.RawRender(100), "retry glyph must advance")
}
