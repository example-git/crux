package app

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskNotificationDeliveryAdmissionIsDeduplicated(t *testing.T) {
	app := &App{taskNotificationDeliveries: make(map[string]struct{})}
	var admitted atomic.Int64
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if app.beginTaskNotificationDelivery("notification") {
				admitted.Add(1)
			}
		}()
	}
	wait.Wait()
	require.EqualValues(t, 1, admitted.Load())

	app.finishTaskNotificationDelivery("notification")
	require.True(t, app.beginTaskNotificationDelivery("notification"))
}
