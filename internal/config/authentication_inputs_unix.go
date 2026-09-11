//go:build !windows

package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"syscall"
)

func openAuthenticationInput(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func authenticationInputPrivateSuccessor(_ *os.File, mode os.FileMode) bool {
	return mode.IsRegular() && mode.Perm() == 0o600
}

func authenticationInputIdentity(_ *os.File, info os.FileInfo) ([sha256.Size]byte, error) {
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !stat.IsValid() || stat.Kind() != reflect.Struct || !stat.FieldByName("Dev").IsValid() || !stat.FieldByName("Ino").IsValid() {
		return [sha256.Size]byte{}, errors.New("authentication configuration file identity is unavailable")
	}
	hash := sha256.New()
	// Access timestamps change on our own read and must not invalidate status.
	for _, name := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "Ctime", "Ctimensec", "Birthtim", "Birthtimespec", "Birthtime", "Birthtimensec"} {
		field := stat.FieldByName(name)
		if field.IsValid() && field.CanInterface() {
			fmt.Fprintf(hash, "%s=%v\n", name, field.Interface())
		}
	}
	var identity [sha256.Size]byte
	copy(identity[:], hash.Sum(nil))
	return identity, nil
}
