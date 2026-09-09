package config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func oauthValidationJournal(t *testing.T) AuthenticationJournal {
	t.Helper()
	root := t.TempDir()
	scope, err := OpenOAuthLoginJournalScope(t.Context(), filepath.Join(root, "global.json"), "", root)
	require.NoError(t, err)
	return scope.journal
}

func oauthValidationKey(id int) AuthenticationJournalKey {
	return AuthenticationJournalKey{Kind: AuthenticationJournalOAuth, WorkspaceID: "original-workspace", OperationID: fmt.Sprintf("%032x", id)}
}

func oauthValidationRecord(j AuthenticationJournal) oauthLoginJournalRecord {
	return oauthLoginJournalRecord{Version: 1, Scope: j.ScopeID(), Owner: providerregistry.RegistrationOwner{ProviderID: "removed-provider"}, Capture: strings.Repeat("a", 64)}
}

func oauthValidationStoreRecord(t *testing.T, j AuthenticationJournal, key AuthenticationJournalKey, record oauthLoginJournalRecord) AuthenticationJournalEntry {
	t.Helper()
	data, err := json.Marshal(record)
	require.NoError(t, err)
	entry, err := j.Store(t.Context(), key, 0, data, false)
	require.NoError(t, err)
	return entry
}

func TestOAuthJournalValidationRetirementStatesAndRestart(t *testing.T) {
	for _, state := range []string{"not-started", "unknown", "recorded"} {
		t.Run(state, func(t *testing.T) {
			j := oauthValidationJournal(t)
			key := oauthValidationKey(1)
			record := oauthValidationRecord(j)
			record.Started = state != "not-started"
			if state == "recorded" {
				record.Token = &oauth.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Client: &oauth.OAuthClient{ClientID: "synthetic-client", ClientSecret: "synthetic-secret"}}
			}
			oauthValidationStoreRecord(t, j, key, record)
			// A fresh path-only scope has no registration or evaluated config.
			peer, err := OpenOAuthLoginJournalScope(t.Context(), j.store.globalDataPath, "", j.store.workingDir)
			require.NoError(t, err)
			listed, err := peer.Pending(t.Context())
			require.NoError(t, err)
			require.Len(t, listed, 1)
			public, err := json.Marshal(listed)
			require.NoError(t, err)
			for _, secret := range []string{"removed-provider", "synthetic-access", "synthetic-refresh", "synthetic-secret", record.Capture, j.path} {
				require.NotContains(t, string(public), secret)
			}
			capture, err := peer.CaptureRetirement(t.Context(), key.WorkspaceID, key.OperationID)
			require.NoError(t, err)
			before, err := os.ReadFile(j.path)
			require.NoError(t, err)
			result, err := capture.Retire(t.Context(), false)
			if state == "recorded" {
				require.Error(t, err)
				after, readErr := os.ReadFile(j.path)
				require.NoError(t, readErr)
				require.Equal(t, before, after)
				result, err = capture.Retire(t.Context(), true)
			}
			require.NoError(t, err)
			require.True(t, result.Abandoned)
			require.Equal(t, listed[0].State, result.State)
			entry, found, err := j.Load(t.Context(), key)
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, entry.Completed())
			stored, err := decodeOAuthLoginJournal(entry)
			require.NoError(t, err)
			require.Equal(t, record.Token, stored.Token)
			require.Equal(t, record.Started, stored.Started)
			require.Equal(t, record.Owner, stored.Owner)
			require.Equal(t, state == "recorded", stored.TokenRecoveryAbandoned)
			replayed, err := peer.CaptureRetirement(t.Context(), key.WorkspaceID, key.OperationID)
			require.NoError(t, err)
			again, err := replayed.Retire(t.Context(), state == "recorded")
			require.NoError(t, err)
			require.Equal(t, result, again)
			_, err = j.store.RecoverOAuthLoginResult(t.Context(), AuthenticationCapture{}, record.Owner, key.WorkspaceID, key.OperationID)
			require.ErrorContains(t, err, "no durable result")
		})
	}
}

func TestOAuthJournalValidationExactScopeOwnerAndCancellation(t *testing.T) {
	for _, mode := range []string{"scope", "legacy", "owner", "capture", "canceled", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			j := oauthValidationJournal(t)
			key, record := oauthValidationKey(2), oauthValidationRecord(j)
			entry := oauthValidationStoreRecord(t, j, key, record)
			scope := OAuthLoginJournalScope{journal: j}
			capture, err := scope.CaptureRetirement(t.Context(), key.WorkspaceID, key.OperationID)
			require.NoError(t, err)
			ctx := t.Context()
			switch mode {
			case "scope":
				other, err := OpenOAuthLoginJournalScope(ctx, j.store.globalDataPath, filepath.Join(j.store.workingDir, "other.json"), j.store.workingDir)
				require.NoError(t, err)
				list, err := other.Pending(ctx)
				require.NoError(t, err)
				require.Empty(t, list)
				_, err = other.CaptureRetirement(ctx, key.WorkspaceID, key.OperationID)
				require.Error(t, err)
				return
			case "legacy":
				record.Scope = ""
			case "owner":
				record.Owner.ProviderID = "different"
			case "capture":
				record.Capture = strings.Repeat("b", 64)
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "revoked":
				j.store.RevokeRuntime()
			}
			if mode == "legacy" || mode == "owner" || mode == "capture" {
				data, err := json.Marshal(record)
				require.NoError(t, err)
				_, err = j.Store(ctx, key, entry.Revision(), data, false)
				require.NoError(t, err)
			}
			before, err := os.ReadFile(j.path)
			require.NoError(t, err)
			_, err = capture.Retire(ctx, false)
			require.Error(t, err)
			after, err := os.ReadFile(j.path)
			require.NoError(t, err)
			require.Equal(t, before, after)
			if mode == "legacy" {
				_, err := scope.Pending(t.Context())
				require.ErrorContains(t, err, "no captured scope")
			}
		})
	}
}

func TestOAuthJournalValidationLeaseWaitAndBoundedBuckets(t *testing.T) {
	j := oauthValidationJournal(t)
	key := oauthValidationKey(3)
	release, err := j.AcquireOperation(t.Context(), key)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err = j.AcquireOperation(ctx, key)
	require.Error(t, err)
	// Different kinds must not deadlock a nested client -> local operation.
	other := key
	other.Kind = AuthenticationJournalLocal
	unlock, err := j.AcquireOperation(t.Context(), other)
	require.NoError(t, err)
	unlock()
	release()
	release()
	for i := range 300 {
		unlock, err := j.AcquireOperation(t.Context(), oauthValidationKey(i+10))
		require.NoError(t, err)
		unlock()
	}
	files, err := os.ReadDir(filepath.Join(j.path+".operation-locks", AuthenticationJournalOAuth))
	require.NoError(t, err)
	require.LessOrEqual(t, len(files), 256)
}

// Seed a valid near-full file using reservations, not a 256 MiB disk payload.
// Store/Load still perform real locked disk reads, CAS and durable writes.
func oauthValidationNearFull(t *testing.T, j AuthenticationJournal, selected authenticationJournalRecord, completedPayload int) {
	t.Helper()
	a, b := oauthValidationKey(100), oauthValidationKey(101)
	disk := authenticationJournalDisk{Version: 1, Sequence: 3, Records: map[string]authenticationJournalRecord{
		a.id():            {Key: a, Revision: 1, Reserved: 180 << 20, Payload: json.RawMessage(`{}`)},
		b.id():            {Key: b, Revision: 2, Payload: json.RawMessage(`{}`)},
		selected.Key.id(): selected,
	}}
	if completedPayload > 0 {
		selected.Payload = json.RawMessage(`{"padding":"` + strings.Repeat("x", completedPayload) + `"}`)
		disk.Records[selected.Key.id()] = selected
	}
	for range 5 {
		encoded, err := json.Marshal(disk)
		require.NoError(t, err)
		capacity := len(encoded)
		for _, r := range disk.Records {
			capacity += authenticationJournalReservation(r.Completed, r.Reserved)
		}
		r := disk.Records[b.id()]
		r.Reserved += maxAuthenticationJournalBytes - capacity
		disk.Records[b.id()] = r
	}
	encoded, err := json.Marshal(disk)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(j.path, encoded, 0600))
	_, _, err = readAuthenticationJournal(t.Context(), j.path)
	require.NoError(t, err)
}

func TestOAuthJournalValidationTerminalRoomAndBytePressure(t *testing.T) {
	t.Run("terminal allowance", func(t *testing.T) {
		j := oauthValidationJournal(t)
		key := oauthValidationKey(102)
		oauthValidationNearFull(t, j, authenticationJournalRecord{Key: key, Revision: 3, Payload: json.RawMessage(`{}`)}, 0)
		_, err := j.Store(t.Context(), oauthValidationKey(103), 0, json.RawMessage(`{}`), false)
		require.Error(t, err)
		payload := json.RawMessage(`{"terminal":"` + strings.Repeat("x", authenticationJournalTransitionBytes-128) + `"}`)
		entry, err := j.Store(t.Context(), key, 3, payload, true)
		require.NoError(t, err)
		require.True(t, entry.Completed())
		_, found, err := j.Load(t.Context(), oauthValidationKey(100))
		require.NoError(t, err)
		require.True(t, found)
	})
	t.Run("completed bytes", func(t *testing.T) {
		j := oauthValidationJournal(t)
		old := oauthValidationKey(102)
		oauthValidationNearFull(t, j, authenticationJournalRecord{Key: old, Revision: 3, Completed: true}, 512<<10)
		entry, err := j.Store(t.Context(), oauthValidationKey(103), 0, json.RawMessage(`{}`), false, 64<<10)
		require.NoError(t, err)
		require.False(t, entry.Completed())
		_, found, err := j.Load(t.Context(), old)
		require.NoError(t, err)
		require.False(t, found)
		_, found, err = j.Load(t.Context(), oauthValidationKey(100))
		require.NoError(t, err)
		require.True(t, found)
	})
	t.Run("oversubscribed read is unchanged", func(t *testing.T) {
		j := oauthValidationJournal(t)
		key := oauthValidationKey(102)
		oauthValidationNearFull(t, j, authenticationJournalRecord{Key: key, Revision: 3, Payload: json.RawMessage(`{}`)}, 0)
		data, err := os.ReadFile(j.path)
		require.NoError(t, err)
		var disk authenticationJournalDisk
		require.NoError(t, json.Unmarshal(data, &disk))
		filler := oauthValidationKey(101)
		record := disk.Records[filler.id()]
		record.Reserved++
		disk.Records[filler.id()] = record
		data, err = json.Marshal(disk)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(j.path, data, 0600))
		_, _, err = j.Load(t.Context(), key)
		require.ErrorContains(t, err, "oversubscribed")
		after, err := os.ReadFile(j.path)
		require.NoError(t, err)
		require.Equal(t, data, after)
	})
}

func TestOAuthJournalValidationCountLimitAndImmutableCompletion(t *testing.T) {
	j := oauthValidationJournal(t)
	var oldest AuthenticationJournalEntry
	for i := range maxAuthenticationJournalRecords {
		entry, err := j.Store(t.Context(), oauthValidationKey(i), 0, json.RawMessage(`{}`), false)
		require.NoError(t, err)
		if i == 0 {
			oldest = entry
		}
	}
	before, err := os.ReadFile(j.path)
	require.NoError(t, err)
	_, err = j.Store(t.Context(), oauthValidationKey(999), 0, json.RawMessage(`{}`), false)
	require.Error(t, err)
	after, err := os.ReadFile(j.path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, after))
	complete, err := j.Store(t.Context(), oauthValidationKey(0), oldest.Revision(), json.RawMessage(`{"retired":true}`), true)
	require.NoError(t, err)
	_, err = j.Store(t.Context(), oauthValidationKey(0), complete.Revision(), json.RawMessage(`{}`), false)
	require.Error(t, err)
	_, err = j.Store(t.Context(), oauthValidationKey(999), 0, json.RawMessage(`{}`), false)
	require.NoError(t, err)
	_, found, err := j.Load(t.Context(), oauthValidationKey(0))
	require.NoError(t, err)
	require.False(t, found)
}

func TestOAuthJournalValidationLeaseProcessExit(t *testing.T) {
	if root := os.Getenv("CRUX_OAUTH_JOURNAL_LEASE_HELPER"); root != "" {
		scope, err := OpenOAuthLoginJournalScope(t.Context(), filepath.Join(root, "global.json"), "", root)
		require.NoError(t, err)
		release, err := scope.journal.AcquireOperation(t.Context(), oauthValidationKey(500))
		require.NoError(t, err)
		defer release()
		fmt.Println("lease-acquired")
		<-time.After(time.Hour)
		return
	}
	root := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestOAuthJournalValidationLeaseProcessExit$")
	child.Env = append(os.Environ(), "CRUX_OAUTH_JOURNAL_LEASE_HELPER="+root)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	child.Stderr = &stderr
	require.NoError(t, child.Start())
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	ready := make(chan bool, 1)
	go func() {
		reader := bufio.NewScanner(stdout)
		ready <- reader.Scan() && reader.Text() == "lease-acquired"
	}()
	select {
	case acquired := <-ready:
		require.True(t, acquired)
	case <-time.After(5 * time.Second):
		t.Fatal("child did not acquire operation lease")
	}
	scope, err := OpenOAuthLoginJournalScope(t.Context(), filepath.Join(root, "global.json"), "", root)
	require.NoError(t, err)
	blocked, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	_, err = scope.journal.AcquireOperation(blocked, oauthValidationKey(500))
	cancel()
	require.Error(t, err)
	require.NoError(t, child.Process.Kill())
	_ = child.Wait()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	release, err := scope.journal.AcquireOperation(ctx, oauthValidationKey(500))
	require.NoError(t, err)
	release()
}

func TestOAuthJournalValidationProductionExchangeRoutesHoldLease(t *testing.T) {
	for _, route := range []string{"browser", "device", "code"} {
		t.Run(route, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			entered, respond := make(chan struct{}), make(chan struct{})
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-respond:
					_ = json.NewEncoder(w).Encode(oauthLoginToken())
				case <-r.Context().Done():
				}
			}))
			defer host.Close()
			defer func() {
				select {
				case <-respond:
				default:
					close(respond)
				}
			}()
			fetch := func(ctx context.Context) (*oauth.Token, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, host.URL+"/token", nil)
				if err != nil {
					return nil, err
				}
				response, err := providertransport.ClientWithContextOwnerValidator(ctx, host.Client()).Do(req)
				if err != nil {
					return nil, err
				}
				defer response.Body.Close()
				var token oauth.Token
				err = json.NewDecoder(response.Body).Decode(&token)
				return &token, err
			}
			f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
				r.Identity = nil
				r.OAuth.Adapter = providerregistry.LoginBrowser
				r.OAuth.Authorize = func(ctx context.Context, _ providerregistry.OpenURL, _ providerregistry.ReadCode) (*oauth.Token, error) {
					return fetch(ctx)
				}
				if route == "device" {
					r.OAuth.Adapter = providerregistry.LoginDeviceCode
					r.OAuth.RequestDeviceCode = func(context.Context) (*providerregistry.DeviceAuthorization, error) {
						return &providerregistry.DeviceAuthorization{UserCode: "synthetic-code", VerificationURL: "https://example.invalid/device"}, nil
					}
					r.OAuth.PollDeviceCode = func(ctx context.Context, _ *providerregistry.DeviceAuthorization) (*oauth.Token, error) {
						return fetch(ctx)
					}
				}
				if route == "code" {
					r.OAuth.Adapter = providerregistry.LoginHostedPaste
					r.OAuth.Callback = &oauth.CallbackRequirement{Mode: "hosted-paste"}
					r.OAuth.PrepareCode = func(ctx context.Context, _ uint16) (*oauth.CodeChallenge, error) {
						return oauth.NewCodeChallenge(ctx, "https://example.invalid/authorize", time.Time{}, func(ctx context.Context, _ string) (*oauth.Token, error) { return fetch(ctx) })
					}
				}
			})
			key := oauthValidationKey(501)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			ctx = ContextWithAuthenticationOperation(ctx, key)
			prepared, err := f.store.PrepareOAuthLogin(ctx, f.capture(t), f.owner)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				var err error
				switch route {
				case "browser":
					_, err = f.store.AuthorizeOAuthLogin(ctx, prepared, nil, nil)
				case "device":
					var device OAuthDeviceLogin
					device, err = f.store.RequestOAuthDeviceCode(ctx, prepared)
					if err == nil {
						_, err = f.store.PollOAuthDeviceCode(ctx, device)
					}
				case "code":
					var code OAuthCodeLogin
					code, err = f.store.PrepareOAuthCodeChallenge(ctx, prepared, 0)
					if err == nil {
						defer code.Close()
						_, err = f.store.ExchangeOAuthCode(ctx, code, "synthetic-code")
					}
				}
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("exchange stopped before HTTPS: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			scope, err := OpenOAuthLoginJournalScope(ctx, f.store.globalDataPath, f.store.workspacePath, f.store.workingDir)
			require.NoError(t, err)
			retire, err := scope.CaptureRetirement(ctx, key.WorkspaceID, key.OperationID)
			require.NoError(t, err)
			blocked, stop := context.WithTimeout(ctx, 30*time.Millisecond)
			_, err = retire.Retire(blocked, false)
			stop()
			require.Error(t, err)
			close(respond)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			_, err = retire.Retire(ctx, false)
			require.ErrorContains(t, err, "recorded OAuth token")
			list, err := scope.Pending(ctx)
			require.NoError(t, err)
			require.Len(t, list, 1)
			require.Equal(t, "token-result-recorded", list[0].State)
			require.False(t, list[0].Abandoned)
		})
	}
}
