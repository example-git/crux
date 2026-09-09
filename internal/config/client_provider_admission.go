package config

import (
	"errors"
	"fmt"
	"maps"

	"github.com/example-git/crux/internal/providerregistry"
)

// clientProviderWithdrawals prepares immutable receiver-local history. Only
// publishing the candidate makes these withdrawals effective. It deliberately
// does not mutate captured snapshots or survive a recreated workspace.
func clientProviderWithdrawals(before, next RuntimeSnapshot) map[providerregistry.RegistrationOwner]uint64 {
	withdrawn := maps.Clone(before.clientRuntime.withdrawnAt)
	for _, definition := range before.clientRuntime.proposal.Providers {
		owner, ok := before.ProviderOwner(definition.Config.ID)
		if !ok {
			continue
		}
		current, found := next.ProviderOwner(owner.ProviderID)
		if found && current == owner && next.ClientProviderUnavailable(owner.ProviderID) == nil {
			continue
		}
		if withdrawn == nil {
			withdrawn = make(map[providerregistry.RegistrationOwner]uint64)
		}
		withdrawn[owner] = next.clientRuntime.authority.Revision
	}
	return withdrawn
}

// ValidateClientProviderAdmission fences a new request using captured client
// credentials against the receiver's current accepted authority. An ordinary
// available-to-available update retains the captured request's credentials. An
// intervening withdrawal permanently retires that capture even after login or
// import restores the owner. This is not cancellation of an admitted stream or
// a cross-workspace/restart epoch guarantee.
func (s RuntimeSnapshot) ValidateClientProviderAdmission(admitted RuntimeSnapshot, owner providerregistry.RegistrationOwner) error {
	if err := s.RuntimeRevocation(); err != nil {
		return err
	}
	if err := admitted.RuntimeRevocation(); err != nil {
		return err
	}
	if admitted.clientRuntime == nil || s.clientRuntime == nil {
		return errors.New("current client runtime authority changed")
	}
	if unavailable := admitted.ClientProviderUnavailable(owner.ProviderID); unavailable != nil {
		return unavailable
	}
	captured, current := admitted.clientRuntime.authority, s.clientRuntime.authority
	if current.Mode != "client" || current.Principal != captured.Principal ||
		current.Revision < captured.Revision || current.Revision == captured.Revision && current.Digest != captured.Digest {
		return errors.New("current client runtime authority changed")
	}
	capturedOwner, capturedFound := admitted.ProviderOwner(owner.ProviderID)
	currentOwner, found := s.ProviderOwner(owner.ProviderID)
	if !capturedFound || capturedOwner != owner || !found || currentOwner != owner {
		return errors.New("current client provider owner is unavailable or changed")
	}
	if unavailable := s.ClientProviderUnavailable(owner.ProviderID); unavailable != nil {
		return unavailable
	}
	if s.clientRuntime.withdrawnAt[owner] > captured.Revision {
		return fmt.Errorf("client provider %s was withdrawn after this request configuration was captured; start a request with the current model", owner.ProviderID)
	}
	return nil
}
