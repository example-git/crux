package imagegen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/redact"
)

type imageUploadCache struct {
	path     string
	entries  map[string]providertransport.ImageUploadReference
	release  func()
	validate func() error
}

func openImageUploadCache(ctx context.Context, directory string, owner providerplugin.ImageOwner, identity [32]byte, validate func() error) (*imageUploadCache, error) {
	if directory == "" || !filepath.IsAbs(directory) || validate == nil {
		return nil, errors.New("persistent image upload storage is unavailable")
	}
	if err := validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(struct {
		Owner    providerplugin.ImageOwner
		Identity [32]byte
	}{owner, identity})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	path := filepath.Join(directory, hex.EncodeToString(digest[:])+".json")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	cache := &imageUploadCache{path: path, entries: map[string]providertransport.ImageUploadReference{}, release: release, validate: validate}
	if err := cache.load(); err != nil {
		release()
		return nil, err
	}
	return cache, nil
}

func validUploadIdentifier(value string) bool {
	return value != "" && len(value) <= 512 && !strings.Contains(value, "://") && !strings.ContainsAny(value, "?&#") && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0 && redact.String(value) == value
}

var uploadCredentialIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func validUploadReference(key string, value providertransport.ImageUploadReference) bool {
	digest, err := hex.DecodeString(key)
	if err != nil || len(digest) != sha256.Size || strings.ToLower(key) != key || !validUploadIdentifier(value.Identifier) || len(value.Credentials) > 32 {
		return false
	}
	seen := make(map[string]bool, len(value.Credentials))
	for _, credential := range value.Credentials {
		if !uploadCredentialIdentifier.MatchString(credential) || seen[credential] {
			return false
		}
		seen[credential] = true
	}
	return true
}

func (c *imageUploadCache) load() error {
	if err := c.validate(); err != nil {
		return err
	}
	info, err := os.Lstat(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return errors.New("invalid image upload registry file")
	}
	file, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, (2<<20)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c.entries) != nil || decoder.Decode(new(any)) != io.EOF || c.entries == nil || len(c.entries) > 1024 {
		return errors.New("invalid image upload registry JSON")
	}
	for key, value := range c.entries {
		if !validUploadReference(key, value) {
			return errors.New("invalid image upload registry entry")
		}
	}
	return nil
}

func (c *imageUploadCache) save(key string, value providertransport.ImageUploadReference) error {
	if !validUploadReference(key, value) {
		return errors.New("persistent image uploads require a bounded non-secret opaque identifier")
	}
	if err := c.validate(); err != nil {
		return err
	}
	entries := maps.Clone(c.entries)
	if _, exists := entries[key]; !exists && len(entries) >= 1024 {
		delete(entries, slices.Sorted(maps.Keys(entries))[0])
	}
	value.Credentials = slices.Clone(value.Credentials)
	entries[key] = value
	data, err := json.Marshal(entries)
	if err != nil || len(data) > 2<<20 {
		return errors.New("image upload registry exceeds its bound")
	}
	file, err := os.CreateTemp(filepath.Dir(c.path), ".image-uploads-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), c.path); err != nil {
		return err
	}
	c.entries = entries
	return nil
}
