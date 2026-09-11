package connection

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type authorizationTreeEntry struct {
	Mode     fs.FileMode
	Size     int64
	Modified time.Time
	Digest   [32]byte
}

func authorizationUseTree(t *testing.T, root string) map[string]authorizationTreeEntry {
	t.Helper()
	result := map[string]authorizationTreeEntry{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := authorizationTreeEntry{Mode: info.Mode()}
		if !entry.IsDir() {
			value.Size, value.Modified = info.Size(), info.ModTime()
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.Digest = sha256.Sum256(contents)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = value
		return nil
	}))
	return result
}

func authorizationUseFixture(t *testing.T) (*ClientAuthorization, func(), string) {
	t.Helper()
	root := t.TempDir()
	setConnectionRoot(t, root)
	serverCode, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	saved, clientCode, err := Add(t.Context(), "observed-client", "tcp://127.0.0.1:1", serverCode)
	require.NoError(t, err)
	require.NoError(t, AuthorizeClient(t.Context(), "observed-client", clientCode))
	// A preexisting stored timestamp remains historical compatibility data.
	require.NoError(t, update(t.Context(), func(data *store) error {
		for principal, record := range data.AuthorizationRecords {
			at := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
			record.LastUsedAt = &at
			data.AuthorizationRecords[principal] = record
		}
		return nil
	}))
	tlsConfig, authority, err := ServerTLSConfigWithAuthorization(t.Context())
	require.NoError(t, err)
	require.NoError(t, authority.StartLive(t.Context(), func(string) error { return nil }, func(context.Context, string) error { return nil }))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, authority.CloseLive(ctx))
	})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, done, err := authority.AdmitRequest(r.Context(), *r.TLS, nil)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		defer done()
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = tlsConfig
	server.StartTLS()
	t.Cleanup(server.Close)
	clientTLS, err := ClientTLSConfig(saved)
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: clientTLS, Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request := func() {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusNoContent, response.StatusCode)
	}
	return authority, request, root
}

func TestAuthorizationUseAdmissionAndReadLeaveStoreTreeUnchanged(t *testing.T) {
	authority, request, root := authorizationUseFixture(t)
	before := authorizationUseTree(t, root)
	records, err := ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "not-observed", records[0].LiveUseState)
	require.Nil(t, records[0].LiveLastUsedAt)
	historical := *records[0].LastUsedAt
	started := time.Now().UTC()
	request()
	request() // Reused mTLS connection still records per-request admission.
	records, err = ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Equal(t, "observed", records[0].LiveUseState)
	require.NotNil(t, records[0].LiveLastUsedAt)
	require.False(t, records[0].LiveLastUsedAt.Before(started))
	require.Equal(t, historical, *records[0].LastUsedAt)
	// Exceed the former one-second persistence interval. Every file, mode,
	// modification time and directory entry must remain exactly unchanged.
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("test canceled before former flush interval")
	}
	_, err = ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, authorizationUseTree(t, root))
	stored, err := readStoreAt(authority.path)
	require.NoError(t, err)
	require.Equal(t, historical, *stored.AuthorizationRecords[records[0].Fingerprint].LastUsedAt)
}

func TestAuthorizationUseControlScopeReapprovalAndDaemonExit(t *testing.T) {
	authority, request, _ := authorizationUseFixture(t)
	request()
	daemon, err := readAuthorizationDaemon(authority.live.control.path)
	require.NoError(t, err)
	reply, err := requestAuthorizationUses(t.Context(), daemon)
	require.NoError(t, err)
	require.Len(t, reply.Observations, 1)
	old := reply.Observations[0]
	wrong := daemon
	wrong.Token = strings.Repeat("x", len(daemon.Token))
	_, err = requestAuthorizationUses(t.Context(), wrong)
	require.Error(t, err)
	wrong = daemon
	wrong.InstanceID = uuid.NewString()
	_, err = requestAuthorizationUses(t.Context(), wrong)
	require.Error(t, err)
	wrong = daemon
	wrong.ServerFingerprint = strings.Repeat("a", 64)
	_, err = requestAuthorizationUses(t.Context(), wrong)
	require.Error(t, err)
	data, err := readStoreAt(authority.path)
	require.NoError(t, err)
	certificate := data.AuthorizedClients["observed-client"]
	// Explicit reapproval writes a new grant. It must not inherit the old
	// grant's stored or in-memory use even if the certificate is identical.
	require.NoError(t, RevokeClient(t.Context(), "observed-client"))
	require.NoError(t, AuthorizeClient(t.Context(), "observed-client", certificate))
	records, err := ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, old.GrantID, records[0].GrantID)
	require.Nil(t, records[0].LastUsedAt)
	require.Nil(t, records[0].LiveLastUsedAt)
	require.Equal(t, "not-observed", records[0].LiveUseState)
	require.NoError(t, authority.CloseLive(t.Context()))
	records, err = ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Equal(t, "no-live-daemons", records[0].LiveUseState)
	require.Nil(t, records[0].LiveLastUsedAt)
	// A stopped daemon's stale registration is unavailable, not evidence
	// that no request was observed. Listing must not clean up that file.
	encoded, err := json.Marshal(daemon)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(authority.live.control.path, encoded, 0o600))
	before := authorizationUseTree(t, filepath.Dir(authority.path))
	records, err = ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Equal(t, "unavailable", records[0].LiveUseState)
	require.Equal(t, before, authorizationUseTree(t, filepath.Dir(authority.path)))
}

func TestAuthorizationUseListDoesNotCreateMissingStoreOrLocks(t *testing.T) {
	root := t.TempDir()
	setConnectionRoot(t, root)
	before := authorizationUseTree(t, root)
	records, err := ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Empty(t, records)
	require.Equal(t, before, authorizationUseTree(t, root))
	_, err = os.Stat(storePath() + ".lock")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAuthorizationUseRejectsUnboundOrOversizedReplies(t *testing.T) {
	for _, kind := range []string{"instance", "server", "duplicate", "oversized", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			id, serverID := uuid.NewString(), strings.Repeat("a", 64)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "redirect" {
					http.Redirect(w, r, "/unfollowed", http.StatusTemporaryRedirect)
					return
				}
				if kind == "oversized" {
					_, _ = io.WriteString(w, strings.Repeat(" ", maxAuthorizationUseBytes+1))
					return
				}
				reply := authorizationUseReply{authorizationUseRequest: authorizationUseRequest{1, id, serverID}, CheckedAt: time.Now().UTC()}
				if kind == "instance" {
					reply.InstanceID = uuid.NewString()
				}
				if kind == "server" {
					reply.ServerFingerprint = strings.Repeat("b", 64)
				}
				if kind == "duplicate" {
					observation := authorizationUseObservation{strings.Repeat("c", 64), uuid.NewString(), reply.CheckedAt}
					reply.Observations = []authorizationUseObservation{observation, observation}
				}
				_ = json.NewEncoder(w).Encode(reply)
			}))
			defer endpoint.Close()
			_, err := requestAuthorizationUses(t.Context(), authorizationDaemon{Version: 1, InstanceID: id, Address: endpoint.URL, Token: "synthetic-control-token", ServerFingerprint: serverID})
			require.Error(t, err, fmt.Sprintf("%s reply must not be adopted", kind))
		})
	}
}

func TestAuthorizationUsePartialAndOversizedDaemonRegistry(t *testing.T) {
	authority, request, root := authorizationUseFixture(t)
	request()
	daemon, err := readAuthorizationDaemon(authority.live.control.path)
	require.NoError(t, err)
	// A second exact-server registration with a rejected control token makes
	// the result partial while preserving the responding daemon's timestamp.
	daemon.InstanceID = uuid.NewString()
	daemon.Token = strings.Repeat("A", len(daemon.Token))
	encoded, err := json.Marshal(daemon)
	require.NoError(t, err)
	directory := authorizationDaemonDir(authority.path)
	require.NoError(t, os.WriteFile(filepath.Join(directory, daemon.InstanceID+".json"), encoded, 0o600))
	before := authorizationUseTree(t, root)
	records, err := ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "partial", records[0].LiveUseState)
	require.NotNil(t, records[0].LiveLastUsedAt)
	require.Equal(t, before, authorizationUseTree(t, root))
	// More than the finite daemon limit is unknown, never a truncated claim
	// that the selected subset represented all live observations.
	for range maxAuthorizationDaemons - 1 {
		daemon.InstanceID = uuid.NewString()
		encoded, err = json.Marshal(daemon)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(directory, daemon.InstanceID+".json"), encoded, 0o600))
	}
	before = authorizationUseTree(t, root)
	records, err = ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Equal(t, "unavailable", records[0].LiveUseState)
	require.Nil(t, records[0].LiveLastUsedAt)
	require.Equal(t, before, authorizationUseTree(t, root))
}
