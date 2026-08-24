package providerplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	trustStoreVersion = 1
	maxTrustBytes     = 1 << 20
)

type trustRecord struct {
	PluginID    string     `json:"plugin_id"`
	ProviderID  string     `json:"provider_id"`
	PublisherID string     `json:"publisher_id"`
	Digest      string     `json:"digest"`
	ApprovedAt  time.Time  `json:"approved_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

type trustStore struct {
	Version int                    `json:"version"`
	Records map[string]trustRecord `json:"records"`
}

func loadTrustStore(path string) (trustStore, error) {
	value := trustStore{Version: trustStoreVersion, Records: map[string]trustRecord{}}
	data, err := readSecureBoundedFile(path, maxTrustBytes)
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return trustStore{}, fmt.Errorf("read plugin trust state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return trustStore{}, fmt.Errorf("decode plugin trust state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return trustStore{}, errors.New("plugin trust state contains trailing data")
	}
	if value.Version != trustStoreVersion {
		return trustStore{}, fmt.Errorf("unsupported plugin trust state version %d", value.Version)
	}
	if value.Records == nil {
		value.Records = map[string]trustRecord{}
	}
	for key, record := range value.Records {
		if key == "" || key != record.Digest || len(record.Digest) != 64 || record.PluginID == "" || record.ProviderID == "" || record.PublisherID == "" {
			return trustStore{}, errors.New("plugin trust state contains an invalid record")
		}
	}
	return value, nil
}

func saveTrustStore(path string, value trustStore) error {
	value.Version = trustStoreVersion
	if value.Records == nil {
		value.Records = map[string]trustRecord{}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plugin trust state: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxTrustBytes {
		return errors.New("plugin trust state exceeds host limit")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create plugin state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect plugin state directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("refusing to replace unsafe plugin trust state")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect plugin trust state: %w", err)
	}
	file, err := os.CreateTemp(dir, ".trust-*.tmp")
	if err != nil {
		return fmt.Errorf("create plugin trust temporary file: %w", err)
	}
	temporary := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("protect plugin trust temporary file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write plugin trust state: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync plugin trust state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close plugin trust state: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("commit plugin trust state: %w", err)
	}
	remove = false
	return syncDirectory(dir)
}

func (s trustStore) state(value validatedBundle) TrustState {
	record, ok := s.Records[value.digest]
	if !ok || record.PluginID != value.id() || record.ProviderID != value.providerID() || record.PublisherID != value.publisherID() {
		return TrustUnknown
	}
	if record.RevokedAt != nil {
		return TrustRevoked
	}
	return TrustTrusted
}
