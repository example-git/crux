package connection

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPendingPairingExpiredEvidenceRemainsManageable(t *testing.T) {
	for _, expiredSide := range []string{"server", "client"} {
		t.Run(expiredSide, func(t *testing.T) {
			setConnectionRoot(t, t.TempDir())
			server, err := generateIdentity("historical server", x509.ExtKeyUsageServerAuth)
			require.NoError(t, err)
			client, err := NewClientIdentity("historical client")
			require.NoError(t, err)
			connection := Connection{Name: "historical", Address: "tcp://127.0.0.1:1", ServerCertificate: server.Certificate, Client: client}
			path := storePath()
			original, err := stagePendingPairing(t.Context(), path, connection)
			require.NoError(t, err)
			healthy := connection
			healthy.Name = "healthy"
			retained, err := stagePendingPairing(t.Context(), path, healthy)
			require.NoError(t, err)

			// Model an originally valid retained certificate after its validity
			// interval has elapsed. Preserve the private key and sign the past
			// interval; corrupting DER would test a different failure boundary.
			if expiredSide == "server" {
				connection.ServerCertificate = expiredPairingIdentity(t, server, x509.ExtKeyUsageServerAuth).Certificate
			} else {
				connection.Client = expiredPairingIdentity(t, client, x509.ExtKeyUsageClientAuth)
			}
			pending, before, err := readPendingPairings(path)
			require.NoError(t, err)
			entry := pending.Entries[original.OperationID]
			entry.Connection = connection
			entry.CreatedAt = time.Now().Add(-36 * time.Hour).Unix()
			pending.Entries[original.OperationID] = entry
			require.NoError(t, writePendingPairings(path, pending, before))

			listed, err := ListPendingPairings(t.Context())
			require.NoError(t, err)
			require.Len(t, listed, 2)
			ids := map[string]bool{}
			for _, result := range listed {
				ids[result.OperationID] = true
				require.NotEmpty(t, result.ClientFingerprint)
				require.NotEmpty(t, result.ServerFingerprint)
			}
			require.True(t, ids[original.OperationID] && ids[retained.OperationID])

			_, err = RecoverPairing(t.Context(), original.OperationID, "")
			require.ErrorContains(t, err, "not currently valid")
			content, err := os.ReadFile(pendingPairingPath(path))
			require.NoError(t, err)
			connection.Name = "new-expired-attempt"
			_, err = stagePendingPairing(t.Context(), path, connection)
			require.ErrorContains(t, err, "not currently valid")
			after, err := os.ReadFile(pendingPairingPath(path))
			require.NoError(t, err)
			require.True(t, string(content) == string(after), "refused staging must preserve the pending journal")

			require.NoError(t, ForgetPendingPairing(t.Context(), original.OperationID))
			listed, err = ListPendingPairings(t.Context())
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.Equal(t, retained.OperationID, listed[0].OperationID)
		})
	}
}

func expiredPairingIdentity(t *testing.T, identity Identity, usage x509.ExtKeyUsage) Identity {
	t.Helper()
	certificate, key, err := parseIdentity(identity, usage)
	require.NoError(t, err)
	certificate.NotBefore = time.Now().Add(-48 * time.Hour)
	certificate.NotAfter = time.Now().Add(-24 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, key.Public(), key)
	require.NoError(t, err)
	identity.Certificate = base64.RawURLEncoding.EncodeToString(der)
	return identity
}
