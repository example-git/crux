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

const provenanceVersion = 1

type provenanceRecord struct {
	PluginID   string    `json:"plugin_id"`
	Digest     string    `json:"digest"`
	SourceKind string    `json:"source_kind"`
	Commit     string    `json:"commit,omitempty"`
	Installed  time.Time `json:"installed_at"`
}

type provenanceStore struct {
	Version int                         `json:"version"`
	Records map[string]provenanceRecord `json:"records"`
}

func loadProvenance(path string) (provenanceStore, error) {
	value := provenanceStore{Version: provenanceVersion, Records: map[string]provenanceRecord{}}
	data, err := readSecureBoundedFile(path, maxTrustBytes)
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return provenanceStore{}, fmt.Errorf("read plugin provenance: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return provenanceStore{}, fmt.Errorf("decode plugin provenance: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return provenanceStore{}, errors.New("plugin provenance contains trailing data")
	}
	if value.Version != provenanceVersion {
		return provenanceStore{}, fmt.Errorf("unsupported plugin provenance version %d", value.Version)
	}
	if value.Records == nil {
		value.Records = map[string]provenanceRecord{}
	}
	return value, nil
}

func saveProvenance(path string, value provenanceStore) error {
	value.Version = provenanceVersion
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plugin provenance: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxTrustBytes {
		return errors.New("plugin provenance exceeds host limit")
	}
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".provenance-*.tmp")
	if err != nil {
		return fmt.Errorf("create plugin provenance temporary file: %w", err)
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
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("commit plugin provenance: %w", err)
	}
	remove = false
	return syncDirectory(dir)
}
