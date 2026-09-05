package manifest

import (
	"errors"
	"strconv"
	"strings"
)

func ImageAspectRatio(size string) (string, error) {
	widthText, heightText, ok := strings.Cut(size, "x")
	width, widthErr := strconv.Atoi(widthText)
	height, heightErr := strconv.Atoi(heightText)
	if !ok || widthErr != nil || heightErr != nil || width < 1 || height < 1 || width > 16384 || height > 16384 || strconv.Itoa(width) != widthText || strconv.Itoa(height) != heightText {
		return "", errors.New("invalid image dimensions")
	}
	left, right := width, height
	for right != 0 {
		left, right = right, left%right
	}
	return strconv.Itoa(width/left) + ":" + strconv.Itoa(height/left), nil
}
