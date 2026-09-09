package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func inactiveRefreshFixture(t *testing.T) (string, Entry, Snapshot) {
	t.Helper()
	path, _ := accountSnapshotFixture(t)
	target := Entry{ID: "inactive", DisplayName: "Inactive account", AccessToken: "synthetic-inactive-old", RefreshToken: "synthetic-inactive-refresh", ExpiresAt: 1, Raw: json.RawMessage(`{"number":1.0,"nullable":null}`)}
	require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, target))
	before := captureAccountSnapshot(t, path)
	for _, entry := range before.Entries(snapshotNamespace) {
		if entry.ID == target.ID {
			target = entry
		}
	}
	return path, target, before
}

func inactiveOwnerValid() error { return nil }

type inactiveHTTPSFixture struct {
	refresh Refresher
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	once    sync.Once
}

func inactiveHTTPSRefresh(t *testing.T) *inactiveHTTPSFixture {
	t.Helper()
	fixture := &inactiveHTTPSFixture{started: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.calls.Add(1)
		if r.TLS == nil || r.Method != http.MethodPost || r.ParseForm() != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-inactive-refresh" {
			t.Error("unexpected disposable refresh request")
			http.Error(w, "invalid exchange", http.StatusBadRequest)
			return
		}
		fixture.once.Do(func() { close(fixture.started) })
		select {
		case <-fixture.release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rotatedToken())
	}))
	t.Cleanup(server.Close)
	fixture.refresh = func(ctx context.Context, refresh string) (*oauth.Token, error) {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := providertransport.ClientWithContextOwnerValidator(ctx, server.Client()).Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, errors.New("disposable exchange failed")
		}
		var token oauth.Token
		if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
			return nil, err
		}
		return &token, nil
	}
	return fixture
}

type inactiveRefreshOutcome struct {
	result InactiveRefreshResult
	err    error
}

func startInactiveRefresh(ctx context.Context, before Snapshot, target Entry, fixture *inactiveHTTPSFixture, validate Validator) <-chan inactiveRefreshOutcome {
	outcome := make(chan inactiveRefreshOutcome, 1)
	go func() {
		result, err := before.RefreshInactive(ctx, snapshotNamespace, target.ID, fixture.refresh, validate)
		outcome <- inactiveRefreshOutcome{result, err}
	}()
	return outcome
}

func TestInactiveRefreshHTTPSKeepsSelectionAndReturnsExactSuccessor(t *testing.T) {
	path, target, before := inactiveRefreshFixture(t)
	fixture := inactiveHTTPSRefresh(t)
	outcome := startInactiveRefresh(t.Context(), before, target, fixture, inactiveOwnerValid)
	<-fixture.started
	// Ordinary account readers/writers remain available during the exchange.
	require.True(t, before.SameObservation(captureAccountSnapshot(t, path)))
	active, err := Active(t.Context(), snapshotNamespace)
	require.NoError(t, err)
	require.Equal(t, "selected", active.ID)
	close(fixture.release)
	got := <-outcome
	require.NoError(t, got.err)
	require.True(t, got.result.Written)
	require.Equal(t, "synthetic-new", got.result.Entry.AccessToken)
	require.Equal(t, target.Raw, got.result.Entry.Raw)
	require.True(t, got.result.Snapshot.SameObservation(captureAccountSnapshot(t, path)))
	require.Equal(t, "selected", got.result.Snapshot.ActiveID(snapshotNamespace))
	var original, final store
	require.NoError(t, json.Unmarshal(before.document, &original))
	require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &final))
	require.Equal(t, original.Active, final.Active)
	require.Equal(t, original.Selections, final.Selections)
	require.Equal(t, original.Mutations, final.Mutations)
	require.Equal(t, []rotation{{Before: CredentialID(target), After: CredentialID(got.result.Entry)}}, final.Rotations[snapshotNamespace][target.ID])
	// Returned mutable entry bytes cannot mutate the authorized snapshot.
	got.result.Entry.Raw[0] = '['
	require.True(t, got.result.Snapshot.SameObservation(captureAccountSnapshot(t, path)))
	_, err = before.RefreshInactive(t.Context(), snapshotNamespace, target.ID, fixture.refresh, inactiveOwnerValid)
	require.ErrorIs(t, err, ErrStateChanged, "known peer rotation is rejected before another exchange")
	require.EqualValues(t, 1, fixture.calls.Load())
}

func TestInactiveRefreshUnexpiredAndNonrefreshableAreExactNoops(t *testing.T) {
	for _, branch := range []string{"unexpired", "no expiry", "expired nonrefreshable", "empty nonrefreshable"} {
		t.Run(branch, func(t *testing.T) {
			path, target, _ := inactiveRefreshFixture(t)
			switch branch {
			case "unexpired":
				target.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
			case "no expiry":
				target.ExpiresAt = 0
			case "expired nonrefreshable":
				target.RefreshToken = ""
			case "empty nonrefreshable":
				target.AccessToken, target.RefreshToken = "", ""
			}
			require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, target))
			before := captureAccountSnapshot(t, path)
			result, err := before.RefreshInactive(t.Context(), snapshotNamespace, target.ID, nil, inactiveOwnerValid)
			require.NoError(t, err)
			require.False(t, result.Written)
			require.True(t, before.SameObservation(result.Snapshot))
			require.True(t, before.SameObservation(captureAccountSnapshot(t, path)))
			require.Equal(t, target, result.Entry)
		})
	}
}

func TestInactiveRefreshHTTPSRefreshOnlyWithoutExpiryObtainsAccessToken(t *testing.T) {
	path, target, _ := inactiveRefreshFixture(t)
	target.AccessToken, target.ExpiresAt = "", 0
	require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, target))
	before := captureAccountSnapshot(t, path)
	fixture := inactiveHTTPSRefresh(t)
	outcome := startInactiveRefresh(t.Context(), before, target, fixture, inactiveOwnerValid)
	select {
	case <-fixture.started:
	case got := <-outcome:
		t.Fatalf("refresh-only account returned without exchange: result=%v error=%v", got.result, got.err)
	}
	require.Equal(t, "selected", captureAccountSnapshot(t, path).ActiveID(snapshotNamespace))
	close(fixture.release)
	got := <-outcome
	require.NoError(t, got.err)
	require.True(t, got.result.Written)
	require.EqualValues(t, 1, fixture.calls.Load())
	require.Equal(t, "synthetic-new", got.result.Entry.AccessToken)
	require.Equal(t, "synthetic-new-refresh", got.result.Entry.RefreshToken)
	require.True(t, got.result.Snapshot.SameObservation(captureAccountSnapshot(t, path)))
	require.Equal(t, "selected", got.result.Snapshot.ActiveID(snapshotNamespace))
	stored := find(got.result.Snapshot.Entries(snapshotNamespace), target.ID)
	require.NotNil(t, stored)
	require.Equal(t, got.result.Entry, *stored)
}

func runInactiveRefreshChild(t *testing.T, action string) {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(t.Context(), binary, "-test.run=^TestInactiveRefreshConcurrentSubprocessChanges$", "-test.count=1")
	command.Env = append(os.Environ(), "CRUX_INACTIVE_REFRESH_CHILD="+action)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestInactiveRefreshConcurrentSubprocessChanges(t *testing.T) {
	if action := os.Getenv("CRUX_INACTIVE_REFRESH_CHILD"); action != "" {
		entries, err := List(t.Context(), snapshotNamespace)
		require.NoError(t, err)
		target := find(entries, "inactive")
		require.NotNil(t, target)
		switch action {
		case "other namespace":
			require.NoError(t, Save(t.Context(), "other-namespace", Entry{ID: "peer", AccessToken: "peer-token"}))
		case "inactive addition":
			require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, Entry{ID: "added", AccessToken: "added-token"}))
		case "selection ABA":
			require.NoError(t, SetActive(t.Context(), snapshotNamespace, target.ID))
			require.NoError(t, SetActive(t.Context(), snapshotNamespace, "selected"))
		case "identical Save":
			require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, *target))
		case "manual replacement":
			target.AccessToken = "manual-token"
			require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, *target))
		case "logout":
			require.NoError(t, RemoveProvider(t.Context(), snapshotNamespace))
		}
		return
	}
	for _, action := range []string{"other namespace", "inactive addition", "selection ABA", "identical Save", "manual replacement", "logout"} {
		t.Run(action, func(t *testing.T) {
			path, target, before := inactiveRefreshFixture(t)
			fixture := inactiveHTTPSRefresh(t)
			outcome := startInactiveRefresh(t.Context(), before, target, fixture, inactiveOwnerValid)
			<-fixture.started
			runInactiveRefreshChild(t, action)
			peer := readConditionalDocument(t, path)
			close(fixture.release)
			got := <-outcome
			require.ErrorIs(t, got.err, ErrStateChanged)
			require.False(t, got.result.Snapshot.valid, "rescue must not authorize the stale switch")
			rescue := action == "other namespace" || action == "inactive addition" || action == "selection ABA"
			require.Equal(t, rescue, got.result.Written)
			if !rescue {
				require.Equal(t, peer, readConditionalDocument(t, path), "manual edit/logout cannot be overwritten")
				return
			}
			var latest, saved store
			require.NoError(t, json.Unmarshal(peer, &latest))
			require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &saved))
			require.Equal(t, latest.Active, saved.Active)
			require.Equal(t, latest.Selections, saved.Selections)
			require.Equal(t, latest.Mutations, saved.Mutations)
			require.Equal(t, "synthetic-new", find(saved.Accounts[snapshotNamespace], target.ID).AccessToken)
			if action == "other namespace" {
				require.Equal(t, latest.Accounts["other-namespace"], saved.Accounts["other-namespace"])
			}
			if action == "inactive addition" {
				require.Equal(t, find(latest.Accounts[snapshotNamespace], "added"), find(saved.Accounts[snapshotNamespace], "added"))
			}
		})
	}
}

func TestInactiveRefreshStaleBeforeExchangeNeverCallsRefresher(t *testing.T) {
	for _, action := range []string{"other namespace", "inactive addition", "selection ABA", "identical Save", "manual replacement", "logout"} {
		t.Run(action, func(t *testing.T) {
			path, target, before := inactiveRefreshFixture(t)
			runInactiveRefreshChild(t, action)
			peer := readConditionalDocument(t, path)
			result, err := before.RefreshInactive(t.Context(), snapshotNamespace, target.ID, func(context.Context, string) (*oauth.Token, error) {
				t.Fatal("stale request started an exchange")
				return nil, nil
			}, inactiveOwnerValid)
			require.ErrorIs(t, err, ErrStateChanged)
			require.False(t, result.Written)
			require.False(t, result.Snapshot.valid)
			require.Equal(t, peer, readConditionalDocument(t, path))
		})
	}
}

func TestInactiveRefreshDetectsFullTargetMetadataAndReplacement(t *testing.T) {
	for _, change := range []string{"display", "number spelling", "raw absent", "raw null", "foreign target field", "identical file", "formatting only", "rotation without counter", "owner"} {
		t.Run(change, func(t *testing.T) {
			path, target, before := inactiveRefreshFixture(t)
			fixture := inactiveHTTPSRefresh(t)
			var ownerChanged atomic.Bool
			outcome := startInactiveRefresh(t.Context(), before, target, fixture, func() error {
				if ownerChanged.Load() {
					return errors.New("owner changed")
				}
				return nil
			})
			<-fixture.started
			data := readConditionalDocument(t, path)
			prefix := accountChangePath("accounts", snapshotNamespace) + ".1."
			var err error
			switch change {
			case "display":
				data, err = sjson.SetBytes(data, prefix+"displayName", "Manually renamed")
			case "number spelling":
				data, err = sjson.SetRawBytes(data, prefix+"raw.number", []byte("1"))
			case "raw absent":
				data, err = sjson.DeleteBytes(data, prefix+"raw")
			case "raw null":
				data, err = sjson.SetRawBytes(data, prefix+"raw", []byte("null"))
			case "foreign target field":
				data, err = sjson.SetBytes(data, prefix+"foreign", "new target metadata")
			case "rotation without counter":
				data, err = sjson.SetBytes(data, accountChangePath("rotations", snapshotNamespace, target.ID), []rotation{{Before: "peer", After: "changed"}})
			case "formatting only":
				var pretty bytes.Buffer
				err = json.Indent(&pretty, data, "", "   ")
				data = pretty.Bytes()
			case "owner":
				ownerChanged.Store(true)
			}
			require.NoError(t, err)
			if change != "owner" {
				temporary := path + ".peer"
				require.NoError(t, os.WriteFile(temporary, data, 0o600))
				require.NoError(t, os.Rename(temporary, path))
			}
			close(fixture.release)
			got := <-outcome
			if change == "owner" {
				require.ErrorContains(t, got.err, "owner changed")
			} else {
				require.ErrorIs(t, got.err, ErrStateChanged)
			}
			require.False(t, got.result.Written)
			require.False(t, got.result.Snapshot.valid)
			require.Equal(t, data, readConditionalDocument(t, path))
		})
	}
}

func TestInactiveRefreshPreservesForeignFieldsAndBoundedHistory(t *testing.T) {
	path, target, _ := inactiveRefreshFixture(t)
	data := readConditionalDocument(t, path)
	var err error
	for field, value := range map[string]string{
		"foreign": `{"large":9007199254740993,"number":1e3}`,
		accountChangePath("accounts", snapshotNamespace) + ".1.foreign": `{"exact":9007199254740993,"array":[false,0,""]}`,
	} {
		data, err = sjson.SetRawBytes(data, field, []byte(value))
		require.NoError(t, err)
	}
	history := make([]json.RawMessage, 8)
	for i := range history {
		history[i] = json.RawMessage(fmt.Sprintf(`{"before":"b%d","after":"a%d","foreign":9007199254740993}`, i, i))
	}
	data, err = sjson.SetBytes(data, accountChangePath("rotations", snapshotNamespace, target.ID), history)
	require.NoError(t, err)
	data, err = sjson.SetBytes(data, accountChangePath("mutations", snapshotNamespace, target.ID), uint64(math.MaxUint64))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	before := captureAccountSnapshot(t, path)
	result, err := before.RefreshInactive(t.Context(), snapshotNamespace, target.ID, func(context.Context, string) (*oauth.Token, error) { return rotatedToken(), nil }, inactiveOwnerValid)
	require.NoError(t, err)
	require.True(t, result.Written)
	original := decodeConditionalObject(t, data)
	final := decodeConditionalObject(t, readConditionalDocument(t, path))
	for _, field := range []string{"foreign", "active", "selections", "mutations"} {
		require.Equal(t, original[field], final[field])
	}
	priorTarget, err := readInactiveRefreshTarget(data, snapshotNamespace, target.ID)
	require.NoError(t, err)
	nextTarget, err := readInactiveRefreshTarget(readConditionalDocument(t, path), snapshotNamespace, target.ID)
	require.NoError(t, err)
	require.Equal(t, decodeConditionalObject(t, priorTarget.raw)["foreign"], decodeConditionalObject(t, nextTarget.raw)["foreign"])
	var nextHistory []json.RawMessage
	require.NoError(t, json.Unmarshal(nextTarget.history, &nextHistory))
	require.Len(t, nextHistory, 8)
	for i := range 7 {
		require.True(t, inactiveRefreshRawEqual(history[i+1], nextHistory[i]))
	}
}

func TestInactiveRefreshCancellationAndCapturedPath(t *testing.T) {
	t.Run("disconnect after exchange starts", func(t *testing.T) {
		path, target, before := inactiveRefreshFixture(t)
		other := filepath.Join(t.TempDir(), "must-not-exist")
		t.Setenv("AI_CLI_DIR", other)
		fixture := inactiveHTTPSRefresh(t)
		ctx, cancel := context.WithCancel(t.Context())
		outcome := startInactiveRefresh(ctx, before, target, fixture, inactiveOwnerValid)
		<-fixture.started
		cancel()
		close(fixture.release)
		got := <-outcome
		require.ErrorIs(t, got.err, context.Canceled)
		require.True(t, got.result.Written)
		require.False(t, got.result.Snapshot.valid)
		saved := captureAccountSnapshot(t, path)
		require.Equal(t, "selected", saved.ActiveID(snapshotNamespace))
		require.Equal(t, "synthetic-new", find(saved.Entries(snapshotNamespace), target.ID).AccessToken)
		_, err := os.Stat(other)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	for _, wait := range []string{"before", "refresh flock", "account mutex", "account flock"} {
		t.Run(wait, func(t *testing.T) {
			path, target, before := inactiveRefreshFixture(t)
			var unlock func()
			switch wait {
			case "refresh flock":
				refreshPath := inactiveRefreshLockPath(path, snapshotNamespace, target.ID)
				require.NoError(t, os.MkdirAll(filepath.Dir(refreshPath), 0o700))
				var err error
				unlock, err = lock.File(t.Context(), refreshPath)
				require.NoError(t, err)
			case "account mutex":
				mu.Lock()
				unlock = mu.Unlock
			case "account flock":
				var err error
				unlock, err = lock.File(t.Context(), path+".lock")
				require.NoError(t, err)
			}
			if unlock != nil {
				defer unlock()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			if wait == "before" {
				cancel()
			}
			result, err := before.RefreshInactive(ctx, snapshotNamespace, target.ID, func(context.Context, string) (*oauth.Token, error) {
				t.Error("canceled admission started exchange")
				return nil, nil
			}, inactiveOwnerValid)
			require.Error(t, err)
			require.True(t, errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
			require.False(t, result.Written)
			require.False(t, result.Snapshot.valid)
		})
	}
}

func TestInactiveRefreshRejectsUnavailableAndInvalidResults(t *testing.T) {
	for _, failure := range []string{"missing capability", "nil result", "empty token", "expiry overflow", "exchange error", "missing validator", "wrong namespace", "missing account"} {
		t.Run(failure, func(t *testing.T) {
			path, target, before := inactiveRefreshFixture(t)
			refresher := Refresher(func(context.Context, string) (*oauth.Token, error) { return rotatedToken(), nil })
			validate := Validator(inactiveOwnerValid)
			namespace, id := snapshotNamespace, target.ID
			switch failure {
			case "missing capability":
				refresher = nil
			case "nil result":
				refresher = func(context.Context, string) (*oauth.Token, error) { return nil, nil }
			case "empty token":
				refresher = func(context.Context, string) (*oauth.Token, error) { return &oauth.Token{}, nil }
			case "expiry overflow":
				refresher = func(context.Context, string) (*oauth.Token, error) {
					return &oauth.Token{AccessToken: "fresh", ExpiresAt: math.MaxInt64}, nil
				}
			case "exchange error":
				refresher = func(context.Context, string) (*oauth.Token, error) { return nil, io.ErrUnexpectedEOF }
			case "missing validator":
				validate = nil
			case "wrong namespace":
				namespace = "not-captured"
			case "missing account":
				id = "absent"
			}
			result, err := before.RefreshInactive(t.Context(), namespace, id, refresher, validate)
			require.Error(t, err)
			require.False(t, result.Written)
			require.False(t, result.Snapshot.valid)
			require.True(t, before.SameObservation(captureAccountSnapshot(t, path)))
		})
	}
}

func TestInactiveRefreshResultIsPrivate(t *testing.T) {
	path, target, before := inactiveRefreshFixture(t)
	privateTarget, err := readInactiveRefreshTarget(before.document, snapshotNamespace, target.ID)
	require.NoError(t, err)
	result := InactiveRefreshResult{Entry: target, Snapshot: before, Written: true}
	for _, value := range []any{result, &result, privateTarget, &privateTarget} {
		_, err := json.Marshal(value)
		require.Error(t, err)
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%f"} {
			text := fmt.Sprintf(format, value)
			require.Contains(t, text, "(private)")
			require.NotContains(t, text, "synthetic")
			require.NotContains(t, text, path)
			require.NotContains(t, text, snapshotNamespace)
		}
	}
}

func TestInactiveRefreshCoordinatesConcurrentRequestsAndLegacyAdoption(t *testing.T) {
	path, target, before := inactiveRefreshFixture(t)
	fixture := inactiveHTTPSRefresh(t)
	first := startInactiveRefresh(t.Context(), before, target, fixture, inactiveOwnerValid)
	<-fixture.started
	legacyRoot, err := dir()
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(snapshotNamespace + "\x00" + target.ID))
	legacyPath := filepath.Join(legacyRoot, "locks", fmt.Sprintf("%x.account-refresh.lock", digest))
	require.Equal(t, legacyPath, inactiveRefreshLockPath(path, snapshotNamespace, target.ID))
	unlock, err := lock.TryFile(legacyPath)
	if unlock != nil {
		unlock()
	}
	require.ErrorIs(t, err, lock.ErrContended, "new refresh must retain the exact legacy per-account lock during exchange")
	second := startInactiveRefresh(t.Context(), before, target, fixture, inactiveOwnerValid)
	close(fixture.release)
	one, two := <-first, <-second
	require.NoError(t, one.err)
	require.ErrorIs(t, two.err, ErrStateChanged)
	require.False(t, two.result.Written)
	require.False(t, two.result.Snapshot.valid)
	require.EqualValues(t, 1, fixture.calls.Load())
	adopted, err := EnsureFreshForOwner(t.Context(), snapshotNamespace, &target, func(context.Context, string) (*oauth.Token, error) {
		t.Fatal("legacy consumer exchanged a proven completed rotation again")
		return nil, nil
	}, inactiveOwnerValid)
	require.NoError(t, err)
	require.Equal(t, one.result.Entry, *adopted)
	require.Equal(t, "selected", captureAccountSnapshot(t, path).ActiveID(snapshotNamespace))
}

func TestInactiveRefreshValidatesAtCommitAndPreservesWrittenErrors(t *testing.T) {
	for _, failure := range []string{"owner before rename", "owner after rename", "cancel during validator", "postcapture corruption"} {
		t.Run(failure, func(t *testing.T) {
			path, target, before := inactiveRefreshFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var once sync.Once
			validator := func() error {
				temporary, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".accounts-change-*"))
				if err != nil {
					return err
				}
				if len(temporary) > 0 {
					switch failure {
					case "owner before rename":
						return errors.New("owner replaced at commit")
					case "cancel during validator":
						once.Do(cancel)
					}
				}
				file, err := openAccountSnapshotFile(path)
				if err != nil {
					return err
				}
				observed, err := observeAccountFile(file)
				_ = file.Close()
				if err == nil && observed != before.file && failure == "owner after rename" {
					return errors.New("owner replaced after account write")
				}
				return err
			}
			privateTarget, err := readInactiveRefreshTarget(before.document, snapshotNamespace, target.ID)
			require.NoError(t, err)
			fresh := FromToken(target.ID, target.DisplayName, rotatedToken(), &target)
			if failure == "postcapture corruption" {
				ctx = &afterAccountRenameContext{Context: ctx, path: path, before: before.file, action: func() {
					require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
				}}
			}
			result, err := before.saveInactiveRefresh(ctx, snapshotNamespace, target.ID, privateTarget, fresh, validator)
			require.Error(t, err)
			written := failure == "owner after rename" || failure == "postcapture corruption"
			require.Equal(t, written, result.Written)
			require.False(t, result.Snapshot.valid)
			if !written {
				require.True(t, before.SameObservation(captureAccountSnapshot(t, path)))
			} else if failure == "owner after rename" {
				require.Equal(t, "selected", captureAccountSnapshot(t, path).ActiveID(snapshotNamespace))
				var saved store
				require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &saved))
				require.Equal(t, fresh.AccessToken, find(saved.Accounts[snapshotNamespace], target.ID).AccessToken)
			}
		})
	}
	pathErr := &os.LinkError{Op: "rename", Old: "/private/source-token", New: "/private/destination-token", Err: os.ErrPermission}
	err := privateSnapshotError(pathErr)
	require.ErrorIs(t, err, os.ErrPermission)
	require.NotContains(t, err.Error(), "/private")
}

func TestInactiveRefreshCancellationInsideOwnerValidationPreventsExchange(t *testing.T) {
	path, target, before := inactiveRefreshFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var observedHeld atomic.Bool
	validator := func() error {
		// The final pre-exchange validation is the first call after the account
		// mutex is released following initial validation under the lease.
		if len(mu.held) > 0 {
			observedHeld.Store(true)
		} else if observedHeld.Load() {
			cancel()
		}
		return nil
	}
	result, err := before.RefreshInactive(ctx, snapshotNamespace, target.ID, func(context.Context, string) (*oauth.Token, error) {
		t.Error("validation canceled the caller before the exchange")
		return rotatedToken(), nil
	}, validator)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, result.Written)
	require.False(t, result.Snapshot.valid)
	require.True(t, before.SameObservation(captureAccountSnapshot(t, path)))
}
