package config

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/lock"
)

const (
	authenticationDigestKeySize = 32

	authenticationDigestInputProof         = "oauth-lineage-input-proof"
	authenticationDigestCredential         = "oauth-lineage-credential"
	authenticationDigestEnvironment        = "oauth-lineage-environment"
	authenticationDigestRuntime            = "oauth-lineage-runtime"
	authenticationDigestOperation          = "oauth-lineage-operation"
	authenticationDigestLoginCapture       = "oauth-login-capture"
	authenticationDigestDurableObservation = "authentication-observation"
)

type authenticationDigest struct {
	key []byte
}

func stableBytesID(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (digest authenticationDigest) bytesID(domain string, data []byte) string {
	return digest.legacyID(domain, stableBytesID(data))
}

func (digest authenticationDigest) legacyID(domain, legacy string) string {
	mac := hmac.New(sha256.New, digest.key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(legacy))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *ConfigStore) loadAuthenticationDigest(ctx context.Context) (authenticationDigest, error) {
	if s == nil || !filepath.IsAbs(s.globalDataPath) {
		return authenticationDigest{}, errors.New("authentication digest requires its captured owning configuration path")
	}
	s.authenticationDigestMu.Lock()
	defer s.authenticationDigestMu.Unlock()
	if len(s.authenticationDigestKey) == authenticationDigestKeySize {
		return authenticationDigest{key: s.authenticationDigestKey}, nil
	}
	path := filepath.Clean(s.globalDataPath) + ".authentication-hmac.key"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return authenticationDigest{}, errors.New("authentication digest key directory cannot be prepared")
	}
	limited, cancel := context.WithTimeout(ctx, configLockDeadline)
	defer cancel()
	release, err := lock.File(limited, path+".lock")
	if err != nil {
		return authenticationDigest{}, errors.New("authentication digest key cannot be locked")
	}
	defer release()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return authenticationDigest{}, errors.New("authentication digest key directory cannot be opened")
	}
	defer root.Close()
	name := filepath.Base(path)
	key, exists, err := readAuthenticationDigestKey(root, name)
	if err != nil {
		return authenticationDigest{}, err
	}
	if !exists {
		key = make([]byte, authenticationDigestKeySize)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return authenticationDigest{}, errors.New("authentication digest key cannot be generated")
		}
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return authenticationDigest{}, errors.New("authentication digest key cannot be created")
		}
		_, saveErr := file.Write(key)
		if saveErr == nil {
			saveErr = file.Sync()
		}
		closeErr := file.Close()
		if saveErr == nil {
			saveErr = closeErr
		}
		if saveErr != nil {
			return authenticationDigest{}, errors.New("authentication digest key cannot be saved")
		}
		key, exists, err = readAuthenticationDigestKey(root, name)
		if err != nil || !exists {
			return authenticationDigest{}, errors.New("authentication digest key cannot be verified")
		}
	}
	s.authenticationDigestKey = key
	return authenticationDigest{key: s.authenticationDigestKey}, nil
}

func readAuthenticationDigestKey(root *os.Root, name string) ([]byte, bool, error) {
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return nil, false, errors.New("authentication digest key is invalid")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, false, errors.New("authentication digest key cannot be opened")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || fsext.ValidatePrivateFile(file) != nil || after.Size() != authenticationDigestKeySize {
		return nil, false, errors.New("authentication digest key has invalid identity, size, or permissions")
	}
	key, err := io.ReadAll(io.LimitReader(file, authenticationDigestKeySize+1))
	if err != nil || len(key) != authenticationDigestKeySize {
		return nil, false, errors.New("authentication digest key cannot be read")
	}
	return key, true, nil
}

func (s *ConfigStore) authenticationDigestWithoutContext() (authenticationDigest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.loadAuthenticationDigest(ctx)
}
