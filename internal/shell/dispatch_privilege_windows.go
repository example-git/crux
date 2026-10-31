//go:build windows

package shell

import "io/fs"

func trustedSetuidExecutable(fs.FileInfo) bool {
	return false
}
