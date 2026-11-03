package pathtranslate

import (
	"errors"
	"os"

	"github.com/example-git/crux/internal/fsext"
)

var ErrInternalStateAccess = errors.New("path translation identity state is not available to agent tools")

func (t *Translator) CheckInternalPath(path string) error {
	if t.names == nil || t.names.databasePath == "" || path == "" {
		return nil
	}
	path = t.absolute(path)
	if t.internalPath(path) {
		return ErrInternalStateAccess
	}
	if canonical, err := fsext.CanonicalPath(path); err == nil {
		if t.internalPath(canonical) {
			return ErrInternalStateAccess
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		private, err := os.Stat(t.names.databasePath + suffix)
		if err == nil && os.SameFile(info, private) {
			return ErrInternalStateAccess
		}
	}
	return nil
}

func (t *Translator) internalPath(path string) bool {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if path == t.names.databasePath+suffix {
			return true
		}
	}
	return false
}

func (t *Translator) CheckToolInput(input string) error {
	_, err := transformJSONStrings(input, func(key, value string) (string, error) {
		if pathField(key) {
			return value, t.CheckInternalPath(value)
		}
		return value, nil
	})
	return err
}
