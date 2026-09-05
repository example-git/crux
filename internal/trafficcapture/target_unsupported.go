//go:build !darwin && !linux

package trafficcapture

import (
	"context"
	"fmt"
)

func resolvePIDTarget(context.Context, int) (resolvedTarget, error) {
	return resolvedTarget{}, fmt.Errorf("pid relaunch is supported on Linux and macOS")
}
