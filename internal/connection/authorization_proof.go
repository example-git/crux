package connection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const AuthorizationProofPath = "/v1/authorization"

// AuthorizationProof describes one fresh authorization decision. It is not an
// enrollment grant or authority for a later request, which must be rechecked.
type AuthorizationProof struct {
	Version           int    `json:"version"`
	Principal         string `json:"principal"`
	ServerFingerprint string `json:"server_fingerprint"`
}

func (a *ClientAuthorization) Proof(ctx context.Context, state tls.ConnectionState) (AuthorizationProof, error) {
	if err := a.Authorize(ctx, state); err != nil {
		return AuthorizationProof{}, err
	}
	server := sha256.Sum256(a.serverCertificate)
	return AuthorizationProof{Version: 1, Principal: certificateFingerprint(state.PeerCertificates[0]), ServerFingerprint: hex.EncodeToString(server[:])}, nil
}

func (p AuthorizationProof) ValidateFor(saved Connection) error {
	server, err := parseCertificate(saved.ServerCertificate, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return ErrClientAuthorization
	}
	client, err := parseCertificate(saved.Client.Certificate, x509.ExtKeyUsageClientAuth)
	if err != nil || p.Version != 1 || p.Principal != certificateFingerprint(client) || p.ServerFingerprint != certificateFingerprint(server) {
		return ErrClientAuthorization
	}
	return nil
}

// ConfirmAuthorization requires a direct authenticated proof from the pinned
// server for this exact private client identity. Health/version/capability
// responses and redirect destinations cannot substitute for it.
func ConfirmAuthorization(ctx context.Context, saved Connection) (AuthorizationProof, error) {
	address, err := NormalizeConnectionAddress(saved.Address)
	if err != nil {
		return AuthorizationProof{}, err
	}
	tlsConfig, err := ClientTLSConfig(saved)
	if err != nil {
		return AuthorizationProof{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+strings.TrimPrefix(address, "tcp://")+AuthorizationProofPath, nil)
	if err != nil {
		return AuthorizationProof{}, err
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return AuthorizationProof{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return AuthorizationProof{}, ErrClientAuthorization
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 {
		return AuthorizationProof{}, ErrClientAuthorization
	}
	proof, err := decodeAuthorizationProof(data)
	if err != nil || proof.ValidateFor(saved) != nil {
		return AuthorizationProof{}, ErrClientAuthorization
	}
	return proof, nil
}

func decodeAuthorizationProof(data []byte) (AuthorizationProof, error) {
	var proof AuthorizationProof
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return proof, ErrClientAuthorization
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return AuthorizationProof{}, ErrClientAuthorization
		}
		seen[name] = true
		switch name {
		case "version":
			err = decoder.Decode(&proof.Version)
		case "principal":
			err = decoder.Decode(&proof.Principal)
		case "server_fingerprint":
			err = decoder.Decode(&proof.ServerFingerprint)
		default:
			return AuthorizationProof{}, ErrClientAuthorization
		}
		if err != nil {
			return AuthorizationProof{}, ErrClientAuthorization
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 3 {
		return AuthorizationProof{}, ErrClientAuthorization
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return AuthorizationProof{}, ErrClientAuthorization
	}
	return proof, nil
}
