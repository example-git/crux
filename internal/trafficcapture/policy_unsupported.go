//go:build !darwin

package trafficcapture

import "fmt"

func (manager *captureManager) initializeWireGuard(environment map[string]string, configuration string) (captureSessionStatus, error) {
	return captureSessionStatus{}, fmt.Errorf("native per-app WireGuard capture currently requires macOS")
}
