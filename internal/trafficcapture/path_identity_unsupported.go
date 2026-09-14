//go:build !darwin && !linux

package trafficcapture

import (
	"errors"
	"os"
)

func identityFromFileInfo(os.FileInfo) (pathIdentity, error) {
	return pathIdentity{}, errors.New("traffic capture filesystem identity is unsupported on this platform")
}
