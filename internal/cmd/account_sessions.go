package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/workspace"
)

// Account management only needs an active exact owner, not an implemented
// interactive login adapter. A stored account can still be selected or removed
// when the provider's interactive login declaration is unavailable.
func selectAccountTarget(ws workspace.Workspace, snapshot providerauth.Snapshot, name string) (providerauth.Target, error) {
	if err := snapshot.Validate(); err != nil {
		return providerauth.Target{}, err
	}
	canonical := name
	var aliasOwner *providerauth.Owner
	if cfg := ws.Config(); cfg != nil {
		if registration, ok := cfg.ProviderRegistrationForAccount(name); ok {
			owner := providerauth.PublicOwner(registration.Owner())
			canonical = owner.ProviderID
			aliasOwner = &owner
		}
	}
	for _, status := range snapshot.Providers {
		if status.Owner.ProviderID != canonical {
			continue
		}
		surface, ok := providerregistry.LookupSurface(ws.ProviderSurfaces(), canonical)
		if !ok || surface.Owner == nil || providerauth.PublicOwner(*surface.Owner) != status.Owner || canonical != name && (aliasOwner == nil || *aliasOwner != status.Owner) {
			return providerauth.Target{}, providerauth.ErrOwner
		}
		return providerauth.Target{WorkspaceID: snapshot.WorkspaceID, Owner: status.Owner, Generation: snapshot.Generation}, nil
	}
	return providerauth.Target{}, fmt.Errorf("provider %q is unavailable in this workspace", name)
}

func listWorkspaceAccounts(ctx context.Context, ws workspace.Workspace, output io.Writer) error {
	snapshot, err := ws.ProviderAuthentication(ctx)
	if err != nil {
		return err
	}
	if err = snapshot.Validate(); err != nil {
		return err
	}
	found := false
	for _, status := range snapshot.Providers {
		if !status.Owner.HasOAuth {
			continue
		}
		target, err := selectAccountTarget(ws, snapshot, status.Owner.ProviderID)
		if err != nil {
			return err
		}
		state, err := ws.ProviderAccounts(ctx, target)
		if err != nil {
			return err
		}
		if err = state.Validate(); err != nil {
			return err
		}
		if state.Target != target {
			return providerauth.ErrStale
		}
		if len(state.Accounts) == 0 {
			continue
		}
		found = true
		fmt.Fprintf(output, "%s:\n", status.Owner.ProviderID)
		for _, account := range state.Accounts {
			marker := " "
			if account.Active {
				marker = "*"
			}
			name := account.DisplayName
			if name == "" {
				name = account.ID
			} else if name != account.ID {
				name = fmt.Sprintf("%s (%s)", name, account.ID)
			}
			detail := ""
			if account.ExpiresAt > 0 {
				if time.Now().UnixMilli() >= account.ExpiresAt {
					detail = " (expired)"
					if account.Refreshable {
						detail = " (expired, refreshable)"
					}
				} else {
					detail = fmt.Sprintf(" (expires %s)", time.UnixMilli(account.ExpiresAt).Format(time.RFC3339))
				}
			} else if account.CredentialState == "refresh-only" {
				detail = " (refresh required)"
			} else if account.CredentialState == "absent" {
				detail = " (credential absent)"
			}
			fmt.Fprintf(output, "  %s %s%s\n", marker, name, detail)
		}
	}
	if !found {
		fmt.Fprintln(output, "No stored accounts in this workspace. Use `crux login <provider>` to add one.")
	}
	return nil
}

func switchWorkspaceAccount(ctx context.Context, ws workspace.Workspace, providerID, accountID string, input io.Reader, output io.Writer) error {
	snapshot, err := ws.ProviderAuthentication(ctx)
	if err != nil {
		return err
	}
	target, err := selectAccountTarget(ws, snapshot, providerID)
	if err != nil {
		return err
	}
	request := providerauth.SwitchRequest{OperationID: oauthActionID(), Target: target, AccountID: accountID}
	if err = request.Validate(); err != nil {
		return err
	}
	state, err := ws.ProviderAccounts(ctx, target)
	if err != nil {
		return err
	}
	if err = state.Validate(); err != nil {
		return err
	}
	if state.Target != target {
		return providerauth.ErrStale
	}
	found := false
	for _, account := range state.Accounts {
		found = found || account.ID == accountID
	}
	if !found {
		return providerauth.ErrAccount
	}
	console, closeInput, err := newAuthenticationConsole(ctx, input, output, nil, nil)
	if err != nil {
		return err
	}
	defer closeInput()
	_, err = console.mutateAuthentication(ctx, ws, request.OperationID, target, "Account switch", func() (providerauth.MutationOutcome, error) { return ws.SwitchProviderAccount(ctx, request) }, func(outcome providerauth.MutationOutcome) error { return outcome.ValidateSwitch(request) })
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Active %s account is now %s.\n", target.Owner.ProviderID, accountID)
	return err
}

func logoutWorkspaceProvider(ctx context.Context, ws workspace.Workspace, providerID string, input io.Reader, output io.Writer) error {
	snapshot, err := ws.ProviderAuthentication(ctx)
	if err != nil {
		return err
	}
	target, err := selectAccountTarget(ws, snapshot, providerID)
	if err != nil {
		return err
	}
	request := providerauth.LogoutRequest{OperationID: oauthActionID(), Target: target}
	if err = request.Validate(); err != nil {
		return err
	}
	console, closeInput, err := newAuthenticationConsole(ctx, input, output, nil, nil)
	if err != nil {
		return err
	}
	defer closeInput()
	_, err = console.mutateAuthentication(ctx, ws, request.OperationID, target, "Logout", func() (providerauth.MutationOutcome, error) { return ws.LogoutProvider(ctx, request) }, func(outcome providerauth.MutationOutcome) error { return outcome.ValidateLogout(request) })
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Logged out of %s.\n", target.Owner.ProviderID)
	return err
}
