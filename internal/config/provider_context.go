package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const MaxProviderContextInstructionBytes = 1 << 20

func validProviderContextID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00") && filepath.IsLocal(id+".txt")
}

// collectProviderContextInstructions reads only the selected models' client
// files. The returned strings belong to this proposal, independent of later
// edits or changes to the process environment.
func collectProviderContextInstructions(ctx context.Context, snapshot RuntimeSnapshot, models map[SelectedModelType]SelectedModel) (map[string]string, error) {
	selected := make(map[string]bool)
	for _, model := range models {
		selected[model.Provider] = true
	}
	result := make(map[string]string, len(selected))
	directory := filepath.Join(homeDirFromEnvironment(snapshot.environment), ".ai-cli", "instructions")
	for _, id := range slices.Sorted(maps.Keys(selected)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !validProviderContextID(id) {
			return nil, errors.New("invalid selected provider ID for client instructions")
		}
		text, err := readProviderContextInstructions(filepath.Join(directory, id+".txt"))
		if err != nil {
			return nil, fmt.Errorf("read selected client provider instructions for %q: %w", id, err)
		}
		result[id] = text
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func readProviderContextInstructions(path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("client instruction file cannot be inspected")
	}
	if !info.Mode().IsRegular() || info.Size() > MaxProviderContextInstructionBytes {
		return "", errors.New("client instructions must be a regular file of at most 1 MiB")
	}
	file, err := openProviderContextFile(path)
	if err != nil {
		return "", errors.New("client instruction file cannot be opened")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("client instruction file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxProviderContextInstructionBytes+1))
	if err != nil {
		return "", errors.New("client instruction file cannot be read")
	}
	if len(data) > MaxProviderContextInstructionBytes || !utf8.Valid(data) {
		return "", errors.New("client instructions must be valid UTF-8 of at most 1 MiB")
	}
	after, err := file.Stat()
	if err != nil || info.Size() != int64(len(data)) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return "", errors.New("client instruction file changed while reading; retry publication")
	}
	return string(data), nil
}

func validateProviderContextInstructions(proposal RemoteRuntimeProposal) error {
	selected := make(map[string]bool)
	for _, model := range proposal.Models {
		selected[model.Provider] = true
	}
	for id, content := range proposal.ProviderContextInstructions {
		if !selected[id] || !validProviderContextID(id) || len(content) > MaxProviderContextInstructionBytes || !utf8.ValidString(content) {
			return errors.New("invalid, unselected or oversized client provider instructions")
		}
	}
	return nil
}

// ClientProviderContextInstructions distinguishes a captured empty client file
// from a server-owned prompt. A missing client entry is empty; it must never
// enable a lookup in the execution host's instruction directory.
func (s RuntimeSnapshot) ClientProviderContextInstructions(provider string) (text string, handled bool, err error) {
	if !s.IsClientOwned() {
		return "", false, nil
	}
	for _, selected := range s.clientRuntime.proposal.Models {
		if selected.Provider == provider {
			return s.clientRuntime.proposal.ProviderContextInstructions[provider], true, nil
		}
	}
	return "", true, errors.New("client provider instructions have no selected model binding")
}
