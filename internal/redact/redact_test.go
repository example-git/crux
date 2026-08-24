package redact

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegisterRedactsExactValuesLongestFirst(t *testing.T) {
	Register("overlap", "overlap-long", "overlap")
	require.Equal(t, "[REDACTED] [REDACTED]", String("overlap-long overlap"))
	require.Equal(t, []byte("before [REDACTED] after"), Bytes([]byte("before overlap-long after")))
}

func TestRegisterIsConcurrent(t *testing.T) {
	var wait sync.WaitGroup
	for index := range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			Register(fmt.Sprintf("concurrent-secret-%d", index))
		}()
	}
	wait.Wait()
	for index := range 64 {
		require.NotContains(t, String(fmt.Sprintf("concurrent-secret-%d", index)), "concurrent-secret")
	}
}
