package imagegen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/lock"
)

const (
	flowUploadRegistryVersion = 1
	flowUploadRegistryLimit   = int64(2 << 20)
	flowUploadRegistryEntries = 1024
)

type flowUploadInput struct {
	data      []byte
	hash      string
	label     string
	mediaType string
	size      int64
}

type flowUploadRegistry struct {
	Version int                       `json:"version"`
	Entries []flowUploadRegistryEntry `json:"entries"`
}

type flowUploadRegistryEntry struct {
	ProjectScope string `json:"project_scope"`
	SHA256       string `json:"sha256"`
	Label        string `json:"label"`
	MediaType    string `json:"media_type"`
	Size         int64  `json:"size"`
	MediaID      string `json:"media_id"`
}

func defaultFlowUploadRegistryPath() string {
	return filepath.Join(config.GlobalWorkspaceDir(), "imagegen", "flow-uploads.json")
}

func prepareFlowUpload(image EditImage, index int) (flowUploadInput, error) {
	mediaType, _, err := mime.ParseMediaType(image.MIMEType)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return flowUploadInput{}, fmt.Errorf("input image %d has unsupported media type", index+1)
	}
	if len(image.Data) == 0 {
		return flowUploadInput{}, fmt.Errorf("input image %d is empty", index+1)
	}
	if int64(len(image.Data)) > maxInputImageBytes {
		return flowUploadInput{}, fmt.Errorf("input image %d exceeds %d bytes", index+1, maxInputImageBytes)
	}
	digest := sha256.Sum256(image.Data)
	return flowUploadInput{
		data:      image.Data,
		hash:      hex.EncodeToString(digest[:]),
		label:     flowUploadLabel(image.Filename, index),
		mediaType: strings.ToLower(mediaType),
		size:      int64(len(image.Data)),
	}, nil
}

func (s *flowSession) resolveUpload(ctx context.Context, image flowUploadInput) (string, error) {
	path := s.uploadRegistryPath
	if path == "" {
		return "", errors.New("Google Flow upload registry path is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create Google Flow upload registry directory: %w", err)
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return "", fmt.Errorf("lock Google Flow upload registry: %w", err)
	}
	defer release()
	registry, err := loadFlowUploadRegistry(path)
	if err != nil {
		return "", err
	}
	entryIndex := registry.find(s.project, image.hash)
	if entryIndex >= 0 {
		entry := registry.Entries[entryIndex]
		if _, verifyErr := s.getMedia(ctx, entry.MediaID); verifyErr == nil {
			return entry.MediaID, nil
		} else if !errors.Is(verifyErr, errFlowMediaUnavailable) {
			return "", fmt.Errorf("verify cached Google Flow upload: %w", verifyErr)
		}
	}
	media, err := s.upload(ctx, image)
	if err != nil {
		return "", fmt.Errorf("upload to Google Flow: %w", err)
	}
	entry := flowUploadRegistryEntry{
		ProjectScope: s.project,
		SHA256:       image.hash,
		Label:        image.label,
		MediaType:    image.mediaType,
		Size:         image.size,
		MediaID:      media.id,
	}
	if entryIndex >= 0 {
		registry.Entries[entryIndex] = entry
	} else {
		if len(registry.Entries) >= flowUploadRegistryEntries {
			registry.Entries = append([]flowUploadRegistryEntry(nil), registry.Entries[1:]...)
		}
		registry.Entries = append(registry.Entries, entry)
	}
	if err := writeFlowUploadRegistry(path, registry); err != nil {
		return "", err
	}
	return media.id, nil
}

func (r flowUploadRegistry) find(project, hash string) int {
	for index, entry := range r.Entries {
		if entry.ProjectScope == project && entry.SHA256 == hash {
			return index
		}
	}
	return -1
}

func loadFlowUploadRegistry(path string) (flowUploadRegistry, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return flowUploadRegistry{Version: flowUploadRegistryVersion}, nil
	}
	if err != nil {
		return flowUploadRegistry{}, fmt.Errorf("open Google Flow upload registry: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return flowUploadRegistry{}, fmt.Errorf("inspect Google Flow upload registry: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > flowUploadRegistryLimit {
		return flowUploadRegistry{}, errors.New("Google Flow upload registry is invalid")
	}
	decoder := json.NewDecoder(io.LimitReader(file, flowUploadRegistryLimit+1))
	decoder.DisallowUnknownFields()
	var registry flowUploadRegistry
	if err := decoder.Decode(&registry); err != nil {
		return flowUploadRegistry{}, errors.New("Google Flow upload registry is invalid")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return flowUploadRegistry{}, errors.New("Google Flow upload registry is invalid")
	}
	if err := validateFlowUploadRegistry(registry); err != nil {
		return flowUploadRegistry{}, err
	}
	return registry, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func validateFlowUploadRegistry(registry flowUploadRegistry) error {
	if registry.Version != flowUploadRegistryVersion || len(registry.Entries) > flowUploadRegistryEntries {
		return errors.New("Google Flow upload registry is invalid")
	}
	seen := make(map[string]struct{}, len(registry.Entries))
	for _, entry := range registry.Entries {
		key := entry.ProjectScope + "\x00" + entry.SHA256
		_, digestErr := hex.DecodeString(entry.SHA256)
		mediaType, _, mediaTypeErr := mime.ParseMediaType(entry.MediaType)
		if entry.ProjectScope == "" || len(entry.ProjectScope) > 512 || !flowProjectIDPattern.MatchString(entry.ProjectScope) ||
			len(entry.SHA256) != sha256.Size*2 || digestErr != nil ||
			entry.Label == "" || len(entry.Label) > 255 || filepath.Base(entry.Label) != entry.Label ||
			mediaTypeErr != nil || !strings.HasPrefix(strings.ToLower(mediaType), "image/") ||
			entry.Size < 1 || entry.Size > maxInputImageBytes || entry.MediaID == "" || len(entry.MediaID) > 512 ||
			strings.ContainsAny(entry.MediaID, "\r\n\t ") {
			return errors.New("Google Flow upload registry is invalid")
		}
		if _, exists := seen[key]; exists {
			return errors.New("Google Flow upload registry is invalid")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func writeFlowUploadRegistry(path string, registry flowUploadRegistry) error {
	if err := validateFlowUploadRegistry(registry); err != nil {
		return err
	}
	content, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return errors.New("encode Google Flow upload registry failed")
	}
	content = append(content, '\n')
	if int64(len(content)) > flowUploadRegistryLimit {
		return errors.New("Google Flow upload registry exceeds its size limit")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".flow-uploads-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary Google Flow upload registry: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary Google Flow upload registry: %w", err)
	}
	if _, err := io.Copy(temporary, bytes.NewReader(content)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary Google Flow upload registry: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary Google Flow upload registry: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary Google Flow upload registry: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace Google Flow upload registry: %w", err)
	}
	return nil
}
