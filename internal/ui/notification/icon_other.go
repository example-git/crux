//go:build !darwin

package notification

import (
	_ "embed"
)

//go:embed crux-icon-solo.png
var Icon []byte
