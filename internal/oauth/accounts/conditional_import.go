package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"

	"github.com/tidwall/sjson"
)

// BeginImport stages a fixed upsert and selection on the captured database.
// The caller must perform external work before acquiring this lease, and defer
// Close immediately. The owner validator must not acquire config write locks.
// Commit retains the lease and its exact saved observation until Close.
func (before Snapshot) BeginImport(ctx context.Context, namespace string, entry Entry, validate Validator) (*PendingChange, error) {
	if namespace == "" || entry.ID == "" || entry.AccessToken == "" || validate == nil || !slices.Contains(before.namespaces, namespace) {
		return nil, errors.New("account import requires its captured namespace, credential and owner validator")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validate(); err != nil {
		return nil, err
	}
	pending, err := before.BeginCheck(ctx)
	if err != nil {
		return nil, err
	}
	ready := false
	defer func() {
		if !ready {
			pending.Close()
		}
	}()
	if err := validate(); err != nil {
		return nil, err
	}
	staged, selected, err := stageAccountImport(before.document, namespace, entry)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	registerSecrets(*selected)
	pending.state.kind = accountImport
	pending.state.staged = staged
	pending.state.selected = selected
	pending.state.validate = validate
	ready = true
	return pending, nil
}

func stageAccountImport(document []byte, namespace string, entry Entry) ([]byte, *Entry, error) {
	if len(document) == 0 || bytes.Equal(bytes.TrimSpace(document), []byte("null")) {
		document = []byte("{}")
	}
	fields, err := accountObject(document)
	if err != nil {
		return nil, nil, err
	}
	names, err := inactiveRefreshFieldNames(fields, "active", "accounts", "mutations", "rotations", "selections")
	if err != nil {
		return nil, nil, err
	}
	objects := make(map[string]map[string]json.RawMessage)
	for key, name := range names {
		object, err := accountObject(fields[name])
		if err != nil {
			return nil, nil, err
		}
		objects[key] = object
	}
	var entries []json.RawMessage
	if raw := objects["accounts"][namespace]; len(raw) > 0 && json.Unmarshal(raw, &entries) != nil {
		return nil, nil, errors.New("invalid account import namespace")
	}
	index := -1
	for i, raw := range entries {
		var current Entry
		if json.Unmarshal(raw, &current) != nil {
			return nil, nil, errors.New("invalid account import entry")
		}
		if current.ID == entry.ID {
			if index != -1 {
				return nil, nil, errors.New("account import target is not unique")
			}
			index = i
		}
	}
	raw := []byte("{}")
	if index != -1 {
		raw = bytes.Clone(entries[index])
	}
	entryFields, err := accountObject(raw)
	if err != nil {
		return nil, nil, err
	}
	entryNames, err := inactiveRefreshFieldNames(entryFields, "id", "displayName", "accessToken", "refreshToken", "expiresAt", "raw")
	if err != nil {
		return nil, nil, err
	}
	for _, field := range []struct {
		name   string
		value  any
		remove bool
	}{
		{"id", entry.ID, false}, {"displayName", entry.DisplayName, false}, {"accessToken", entry.AccessToken, false},
		{"refreshToken", entry.RefreshToken, entry.RefreshToken == ""}, {"expiresAt", entry.ExpiresAt, entry.ExpiresAt == 0}, {"raw", entry.Raw, len(entry.Raw) == 0},
	} {
		path := accountChangePath(entryNames[field.name])
		if field.remove {
			raw, err = sjson.DeleteBytes(raw, path)
		} else {
			raw, err = sjson.SetBytes(raw, path, field.value)
		}
		if err != nil {
			return nil, nil, errors.New("cannot stage imported account")
		}
	}
	if index == -1 {
		entries = append(entries, raw)
	} else {
		entries[index] = raw
	}
	var state store
	if json.Unmarshal(document, &state) != nil {
		return nil, nil, errors.New("invalid account import database")
	}
	if state.Mutations[namespace][entry.ID] == math.MaxUint64 {
		return nil, nil, errors.New("account mutation counter exhausted")
	}
	if _, err := accountObject(objects["mutations"][namespace]); err != nil {
		return nil, nil, err
	}
	if _, err := accountObject(objects["rotations"][namespace]); err != nil {
		return nil, nil, err
	}
	staged, err := sjson.SetBytes(document, accountChangePath(names["accounts"], namespace), entries)
	if err == nil {
		staged, err = sjson.SetBytes(staged, accountChangePath(names["mutations"], namespace, entry.ID), state.Mutations[namespace][entry.ID]+1)
	}
	if err == nil {
		staged, err = sjson.DeleteBytes(staged, accountChangePath(names["rotations"], namespace, entry.ID))
	}
	if err != nil {
		return nil, nil, errors.New("cannot stage imported account database")
	}
	return stageAccountChange(staged, accountSwitch, namespace, entry.ID)
}
