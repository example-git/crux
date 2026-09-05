package manifest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageAspectRatio(t *testing.T) {
	for size, expected := range map[string]string{"1920x1080": "16:9", "800x600": "4:3", "640x640": "1:1", "600x800": "3:4", "1080x1920": "9:16"} {
		ratio, err := ImageAspectRatio(size)
		require.NoError(t, err)
		require.Equal(t, expected, ratio)
	}
	for _, size := range []string{"", "auto", "0x1", "1x0", "01x1", "1x01", "+1x1", "16385x1", "1x1x1", "1:1"} {
		_, err := ImageAspectRatio(size)
		require.Error(t, err, size)
	}
}
