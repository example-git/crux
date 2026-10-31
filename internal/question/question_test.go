package question

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAskSerializesConcurrentRequests(t *testing.T) {
	service := NewService()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	events := service.Subscribe(ctx)
	request := func(id string) Request {
		return Request{ID: id, Questions: []Question{{ID: id, Type: TypeYesNo, Text: "Proceed?", Description: "Synthetic question"}}}
	}
	type result struct {
		answers []Answer
		err     error
	}
	first := make(chan result, 1)
	go func() {
		answers, err := service.Ask(ctx, request("first"))
		first <- result{answers, err}
	}()
	select {
	case event := <-events:
		require.Equal(t, "first", event.Payload.ID)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := make(chan result, 1)
	go func() {
		answers, err := service.Ask(ctx, request("second"))
		second <- result{answers, err}
	}()
	select {
	case event := <-events:
		t.Fatalf("second request replaced pending request: %s", event.Payload.ID)
	case <-time.After(30 * time.Millisecond):
	}
	require.True(t, service.Answer([]Answer{{QuestionID: "first"}}))
	select {
	case got := <-first:
		require.NoError(t, got.err)
		require.Equal(t, "first", got.answers[0].QuestionID)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case event := <-events:
		require.Equal(t, "second", event.Payload.ID)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.True(t, service.Answer([]Answer{{QuestionID: "second"}}))
	select {
	case got := <-second:
		require.NoError(t, got.err)
		require.Equal(t, "second", got.answers[0].QuestionID)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestAskCanCancelWhileQueued(t *testing.T) {
	service := NewService()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	events := service.Subscribe(ctx)
	request := Request{Questions: []Question{{Type: TypeYesNo, Text: "Proceed?", Description: "Synthetic question"}}}
	first := make(chan error, 1)
	go func() {
		_, err := service.Ask(ctx, request)
		first <- err
	}()
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waiting, stop := context.WithCancel(ctx)
	stop()
	_, err := service.Ask(waiting, Request{Questions: []Question{{Type: TypeYesNo, Text: "Queued?", Description: "Synthetic queued question"}}})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, service.Cancel())
	select {
	case err := <-first:
		require.ErrorIs(t, err, ErrCancelled)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestPendingQuestionResolution(t *testing.T) {
	service := NewService()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	events := service.Subscribe(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := service.Ask(ctx, Request{ID: "current", Questions: []Question{{Type: TypeYesNo, Text: "Proceed?", Description: "Current request"}}})
		done <- err
	}()
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Equal(t, "current", service.Pending().ID)
	require.False(t, service.AnswerRequest("old", nil))
	require.False(t, service.AnswerRequest("", nil))
	require.NotNil(t, service.Pending())
	results := make(chan bool, 20)
	for i := range 20 {
		go func() {
			if i%2 == 0 {
				results <- service.Cancel()
			} else {
				results <- service.AnswerRequest("current", nil)
			}
		}()
	}
	winners := 0
	for range 20 {
		if <-results {
			winners++
		}
	}
	require.Equal(t, 1, winners)
	select {
	case err := <-done:
		if err != nil {
			require.ErrorIs(t, err, ErrCancelled)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Nil(t, service.Pending())
}

func TestPendingQuestionClearedOnContextCancellation(t *testing.T) {
	service := NewService()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := service.Subscribe(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := service.Ask(ctx, Request{Questions: []Question{{Type: TypeYesNo, Text: "Proceed?", Description: "Cancelled request"}}})
		done <- err
	}()
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("no question event")
	}
	require.NotNil(t, service.Pending())
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Nil(t, service.Pending())
}
