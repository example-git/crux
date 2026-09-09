package accounts

import (
	"context"
	"errors"
)

// SaveState fences an external import against account edits and selection
// changes, including replacement with identical values and switch-away/back.
// Its private fields can only be obtained by reading the account store.
type SaveState struct {
	provider, id, active, credential, activeCredential string
	selection, mutation, activeMutation                uint64
}

func CaptureSaveState(ctx context.Context, provider, id string) (SaveState, error) {
	if provider == "" || id == "" {
		return SaveState{}, errors.New("account save requires provider and account identity")
	}
	var state SaveState
	err := withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		state = captureSaveState(s, provider, id)
		return nil
	})
	return state, err
}

func captureSaveState(s *store, provider, id string) SaveState {
	state := SaveState{provider: provider, id: id, active: s.Active[provider], selection: s.Selections[provider], mutation: s.Mutations[provider][id]}
	if active := findActive(s, provider); active != nil {
		state.activeCredential = CredentialID(*active)
		state.activeMutation = s.Mutations[provider][active.ID]
	}
	for _, entry := range s.Accounts[provider] {
		if entry.ID == id {
			state.credential = CredentialID(entry)
			break
		}
	}
	return state
}

// SaveForOwner commits the imported account only if the selected account and
// target entry still match the state captured before external work began.
func (before SaveState) SaveForOwner(ctx context.Context, entry Entry, validate Validator) error {
	if validate == nil || before.provider == "" || before.id == "" || entry.ID != before.id {
		return errors.New("conditional account save requires its captured identity and owner validator")
	}
	registerSecrets(entry)
	return mutateStore(ctx, validate, func(s *store) error {
		if captureSaveState(s, before.provider, before.id) != before {
			return ErrCredentialChanged
		}
		delete(s.Rotations[before.provider], entry.ID)
		s.markMutation(before.provider, entry.ID)
		s.markSelection(before.provider, entry.ID)
		s.Accounts[before.provider] = upsert(s.Accounts[before.provider], entry)
		s.Active[before.provider] = entry.ID
		return nil
	})
}
