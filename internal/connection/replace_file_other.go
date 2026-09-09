//go:build !windows

package connection

import "os"

func replaceConnectionFile(source, destination string) error {
	return os.Rename(source, destination)
}
