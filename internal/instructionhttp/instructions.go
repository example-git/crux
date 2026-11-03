package instructionhttp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"unicode/utf8"

	"github.com/example-git/crux/internal/agent/prompt"
)

const FileLimit = 2 << 20

var ErrFileChanged = errors.New("instructions changed on disk; reload before saving")

type Document struct {
	Path     string `json:"path"`
	Text     string `json:"text"`
	Revision string `json:"revision"`
	Exists   bool   `json:"exists"`
}

type Files func(context.Context, string) ([]prompt.InstructionFile, error)

type Saved func(context.Context, string, prompt.InstructionFile) error

func Handler(filesFor Files, saved Saved) http.HandlerFunc {
	var mutex sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		files, err := filesFor(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		path := r.URL.Query().Get("path")
		if r.Method == http.MethodGet && path == "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(files)
			return
		}
		if !slices.ContainsFunc(files, func(file prompt.InstructionFile) bool { return file.Path == path }) {
			http.Error(w, "select a configured instruction file", http.StatusBadRequest)
			return
		}
		mutex.Lock()
		defer mutex.Unlock()
		var document Document
		if r.Method == http.MethodPut {
			var request struct {
				Text     string `json:"text"`
				Revision string `json:"revision"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6*FileLimit))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || len(request.Text) > FileLimit {
				http.Error(w, "invalid instruction document (maximum 2 MiB)", http.StatusBadRequest)
				return
			}
			document, err = SaveFile(path, request.Text, request.Revision)
			if err == nil && saved != nil {
				for _, file := range files {
					if file.Path == path {
						err = saved(r.Context(), r.PathValue("id"), file)
						if err != nil {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadGateway)
							_ = json.NewEncoder(w).Encode(struct {
								Message string   `json:"message"`
								Saved   Document `json:"saved_document"`
							}{Message: err.Error(), Saved: document})
							return
						}
						break
					}
				}
			}
		} else {
			document, err = ReadFile(path)
		}
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrFileChanged) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(document)
	}
}

func ReadFile(path string) (Document, error) {
	document := Document{Path: path}
	root, err := os.OpenRoot(filepath.Dir(path))
	if os.IsNotExist(err) {
		return document, nil
	}
	if err != nil {
		return document, err
	}
	defer root.Close()
	return ReadRooted(root, filepath.Base(path), path)
}

func ReadRooted(root *os.Root, name, path string) (Document, error) {
	document := Document{Path: path}
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return document, nil
	}
	if err != nil {
		return document, err
	}
	if !info.Mode().IsRegular() {
		return document, errors.New("instruction editing requires a regular file, not a link or directory")
	}
	file, err := openAttachment(root, name)
	if err != nil {
		return document, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return document, ErrFileChanged
	}
	content, err := io.ReadAll(io.LimitReader(file, FileLimit+1))
	if err != nil {
		return document, err
	}
	if len(content) > FileLimit || !utf8.Valid(content) {
		return document, errors.New("instruction editing requires UTF-8 text of at most 2 MiB")
	}
	digest := sha256.Sum256(content)
	document.Text = string(content)
	document.Revision = hex.EncodeToString(digest[:])
	document.Exists = true
	return document, nil
}

func SaveFile(path, text, revision string) (Document, error) {
	if len(text) > FileLimit || !utf8.ValidString(text) {
		return Document{}, errors.New("instruction editing requires UTF-8 text of at most 2 MiB")
	}
	current, err := ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	if current.Revision != revision {
		return Document{}, ErrFileChanged
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Document{}, err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return Document{}, err
	}
	defer root.Close()
	name := filepath.Base(path)
	mode := os.FileMode(0o600)
	if current.Exists {
		info, err := root.Lstat(name)
		if err != nil {
			return Document{}, err
		}
		mode = info.Mode().Perm()
	}
	temporary := ".crux-instructions-" + rand.Text()
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return Document{}, err
	}
	defer root.Remove(temporary)
	var writeErr error
	if current.Exists {
		writeErr = file.Chmod(mode)
	}
	if writeErr == nil {
		_, writeErr = io.WriteString(file, text)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return Document{}, err
	}
	latest, err := ReadRooted(root, name, path)
	if err != nil {
		return Document{}, err
	}
	if latest.Revision != revision {
		return Document{}, ErrFileChanged
	}
	if err := root.Rename(temporary, name); err != nil {
		return Document{}, fmt.Errorf("saving instructions: %w", err)
	}
	return ReadRooted(root, name, path)
}
