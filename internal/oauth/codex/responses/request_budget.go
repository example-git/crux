package responses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"strings"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp"
)

const (
	maxCodexRequestBytes      = 14 * 1024 * 1024
	retryCodexRequestBytes    = 10 * 1024 * 1024
	codexRequestImageBytes    = 512 * 1024
	initialBudgetImageBytes   = 256 * 1024
	minimumBudgetImageBytes   = 64 * 1024
	omittedImageBudgetMessage = "[Earlier image omitted to keep the Codex request within its message size limit]"
)

type requestBudgetStats struct {
	originalBytes    int
	finalBytes       int
	compressedImages int
	omittedImages    int
}

type requestImageRef struct {
	content *messageContent
}

func fitCodexRequest(logical *requestFrame) (*requestFrame, requestBudgetStats, error) {
	return fitCodexRequestTo(logical, maxCodexRequestBytes)
}

func fitCodexRequestTo(logical *requestFrame, maximumBytes int) (*requestFrame, requestBudgetStats, error) {
	bounded := cloneRequestFrame(logical)
	size, err := fullRequestSize(bounded)
	if err != nil {
		return nil, requestBudgetStats{}, err
	}
	stats := requestBudgetStats{originalBytes: size, finalBytes: size}
	images := requestImages(bounded)
	for _, image := range images {
		changed, compressErr := compressRequestImage(image.content, codexRequestImageBytes, true)
		if compressErr != nil {
			return nil, stats, compressErr
		}
		if changed {
			stats.compressedImages++
		}
	}
	if stats.compressedImages > 0 {
		size, err = fullRequestSize(bounded)
		if err != nil {
			return nil, stats, err
		}
		stats.finalBytes = size
	}
	if size <= maximumBytes {
		return bounded, stats, nil
	}
	if len(images) == 0 {
		return nil, stats, requestSizeError(size, maximumBytes)
	}
	for target := initialBudgetImageBytes; target >= minimumBudgetImageBytes && size > maximumBytes; target /= 2 {
		for _, image := range images {
			changed, compressErr := compressRequestImage(image.content, target, false)
			if compressErr != nil {
				return nil, stats, compressErr
			}
			if !changed {
				continue
			}
			stats.compressedImages++
			size, err = fullRequestSize(bounded)
			if err != nil {
				return nil, stats, err
			}
			if size <= maximumBytes {
				stats.finalBytes = size
				return bounded, stats, nil
			}
		}
	}

	for index := 0; index < len(images)-1 && size > maximumBytes; index++ {
		*images[index].content = messageContent{Type: "input_text", Text: omittedImageBudgetMessage}
		stats.omittedImages++
		size, err = fullRequestSize(bounded)
		if err != nil {
			return nil, stats, err
		}
	}
	stats.finalBytes = size
	if size > maximumBytes {
		return nil, stats, requestSizeError(size, maximumBytes)
	}
	return bounded, stats, nil
}

func fullRequestSize(logical *requestFrame) (int, error) {
	encoded, err := json.Marshal(fullWireRequest(logical))
	if err != nil {
		return 0, fmt.Errorf("serialize Codex request for size validation: %w", err)
	}
	return len(encoded), nil
}

func requestImages(frame *requestFrame) []requestImageRef {
	var images []requestImageRef
	for itemIndex := range frame.Input {
		for contentIndex := range frame.Input[itemIndex].Content {
			content := &frame.Input[itemIndex].Content[contentIndex]
			if content.Type == "input_image" && content.ImageURL != "" {
				images = append(images, requestImageRef{content: content})
			}
		}
	}
	return images
}

func compressRequestImage(content *messageContent, target int, forceJPEG bool) (bool, error) {
	mimeType, encoded, err := splitImageDataURL(content.ImageURL)
	if err != nil {
		return false, err
	}
	if !forceJPEG && base64.StdEncoding.DecodedLen(len(encoded)) <= target {
		return false, nil
	}
	if forceJPEG && mimeType == "image/jpeg" && base64.StdEncoding.DecodedLen(len(encoded)) <= target {
		return false, nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false, err
	}
	compressed, err := compressImageToJPEG(data, target)
	if err != nil {
		return false, fmt.Errorf("compress Codex request image: %w", err)
	}
	content.ImageURL = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(compressed)
	return true, nil
}

func compressImageToJPEG(data []byte, target int) ([]byte, error) {
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := decoded.Bounds()
	flattened := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(flattened, flattened.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(flattened, flattened.Bounds(), decoded, bounds.Min, draw.Over)
	var current image.Image = flattened
	for {
		for _, quality := range []int{75, 60, 45, 30, 20} {
			var output bytes.Buffer
			if err := jpeg.Encode(&output, current, &jpeg.Options{Quality: quality}); err != nil {
				return nil, err
			}
			if output.Len() <= target {
				return output.Bytes(), nil
			}
		}
		bounds = current.Bounds()
		if bounds.Dx() <= 1 && bounds.Dy() <= 1 {
			return nil, fmt.Errorf("image cannot be reduced below %d bytes", target)
		}
		current = imaging.Resize(current, max(1, bounds.Dx()*4/5), max(1, bounds.Dy()*4/5), imaging.Lanczos)
	}
}

func splitImageDataURL(value string) (string, string, error) {
	metadata, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(metadata, "data:image/") || !strings.HasSuffix(metadata, ";base64") {
		return "", "", fmt.Errorf("invalid Codex image data URL")
	}
	mimeType := strings.TrimSuffix(strings.TrimPrefix(metadata, "data:"), ";base64")
	return mimeType, encoded, nil
}

func requestSizeError(size, maximumBytes int) error {
	return fmt.Errorf("Codex request is too large after image compression (%d bytes; safe maximum is %d bytes); compact the session before retrying", size, maximumBytes)
}
