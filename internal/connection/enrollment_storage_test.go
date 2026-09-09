package connection

import (
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrollmentCapturesServerStoreAcrossClientEnvironment(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	serverCode, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	serverPath := storePath()
	e, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })
	setConnectionRoot(t, t.TempDir())
	clientPath := storePath()
	require.NotEqual(t, serverPath, clientPath)
	saved, err := Pair(t.Context(), "separate-client", e.SetupCode())
	require.NoError(t, err)
	result, err := e.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "separate-client", result.Name)
	serverData, err := loadStoreAt(t.Context(), serverPath)
	require.NoError(t, err)
	require.Equal(t, serverCode, serverData.Server.Certificate)
	require.Equal(t, saved.Client.Certificate, serverData.AuthorizedClients["separate-client"])
	require.Empty(t, serverData.Connections)
	clientData, err := loadStoreAt(t.Context(), clientPath)
	require.NoError(t, err)
	require.Nil(t, clientData.Server)
	require.Empty(t, clientData.AuthorizedClients)
	require.True(t, saved == clientData.Connections["separate-client"], "the client store must retain the exact saved identity")
}

func TestEnrollmentRejectsReplacedServerIdentityBeforeAuthorization(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	serverPath := storePath()
	e, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })
	replacement, err := generateIdentity("replacement server", x509.ExtKeyUsageServerAuth)
	require.NoError(t, err)
	require.NoError(t, update(t.Context(), func(data *store) error { data.Server = &replacement; return nil }))
	before, err := os.ReadFile(serverPath)
	require.NoError(t, err)
	setConnectionRoot(t, t.TempDir())
	_, err = Pair(t.Context(), "must-not-authorize", e.SetupCode())
	require.ErrorContains(t, err, "server identity changed")
	after, err := os.ReadFile(serverPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
	data, err := loadStoreAt(t.Context(), serverPath)
	require.NoError(t, err)
	require.Empty(t, data.AuthorizedClients)
}

func TestConnectionStoreCommitRetainsCapturedDestination(t *testing.T) {
	originalRoot := t.TempDir()
	otherRoot := t.TempDir()
	setConnectionRoot(t, originalRoot)
	serverCode, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	originalPath := storePath()
	identity, err := NewClientIdentity("retained-destination")
	require.NoError(t, err)
	created := Connection{Name: "retained-destination", Address: "tcp://fixture.example:9090", ServerCertificate: serverCode, Client: identity}
	require.NoError(t, updateWithCommit(context.Background(), func(data *store) error {
		data.Connections[created.Name] = created
		return nil
	}, func(persist func() error) error {
		t.Setenv("CRUX_GLOBAL_DATA", otherRoot)
		return persist()
	}))
	data, err := loadStoreAt(t.Context(), originalPath)
	require.NoError(t, err)
	require.True(t, created == data.Connections[created.Name], "the original store must contain the exact connection")
	_, err = os.Stat(filepath.Join(otherRoot, "connections.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(otherRoot, "connections.json.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
