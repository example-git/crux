package connection

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"unicode/utf8"

	"github.com/example-git/crux/internal/lock"
)

// ErrClientAuthorization deliberately does not identify a client, certificate,
// store path, or parse failure to an unauthenticated peer.
var ErrClientAuthorization = errors.New("client authorization is unavailable or revoked")

// ClientAuthorization reads current authorization from the path captured when
// network authentication was enabled. It never resolves process environment
// during admission and does not grant authority to a previously verified peer.
type ClientAuthorization struct {
	path              string
	serverCertificate []byte
	liveMu            sync.Mutex
	live              *liveAuthorization
}

func (a *ClientAuthorization) roots(data *store) (*x509.CertPool, error) {
	if a == nil || data == nil || data.Server == nil {
		return nil, ErrClientAuthorization
	}
	server, _, err := parseIdentity(*data.Server, x509.ExtKeyUsageServerAuth)
	if err != nil || !bytes.Equal(server.Raw, a.serverCertificate) {
		return nil, ErrClientAuthorization
	}
	roots := x509.NewCertPool()
	for _, code := range data.AuthorizedClients {
		certificate, err := parseCertificate(code, x509.ExtKeyUsageClientAuth)
		if err != nil {
			return nil, ErrClientAuthorization
		}
		roots.AddCert(certificate)
	}
	return roots, nil
}

// Authorize rechecks both the verified leaf and its current persisted grant.
// Call for each request: a successful handshake does not authorize later work
// after a grant has been removed. Already admitted work is a separate lifecycle.
func (a *ClientAuthorization) Authorize(ctx context.Context, state tls.ConnectionState) error {
	if a == nil || len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return ErrClientAuthorization
	}
	peer := state.PeerCertificates[0]
	if peer == nil || state.VerifiedChains[0][0] == nil || !bytes.Equal(peer.Raw, state.VerifiedChains[0][0].Raw) {
		return ErrClientAuthorization
	}
	data, err := readClientAuthorization(ctx, a.path)
	if err != nil {
		return ErrClientAuthorization
	}
	if _, err := a.roots(data); err != nil {
		return ErrClientAuthorization
	}
	for _, code := range data.AuthorizedClients {
		certificate, err := parseCertificate(code, x509.ExtKeyUsageClientAuth)
		if err != nil {
			return ErrClientAuthorization
		}
		if bytes.Equal(peer.Raw, certificate.Raw) {
			return nil
		}
	}
	return ErrClientAuthorization
}

func readClientAuthorization(ctx context.Context, path string) (*store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, ErrClientAuthorization
	}
	release, err := lock.SharedFile(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	defer release()
	file, err := openClientAuthorization(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nil, ErrClientAuthorization
	}
	var content bytes.Buffer
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := file.Read(buffer)
		content.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	after, err := file.Stat()
	if err != nil || !sameAuthorizationFile(before, after) {
		return nil, ErrClientAuthorization
	}
	current, err := os.Stat(path)
	if err != nil || !sameAuthorizationFile(after, current) {
		return nil, ErrClientAuthorization
	}
	if err := validateClientAuthorizationJSON(content.Bytes()); err != nil {
		return nil, err
	}
	var data store
	decoder := json.NewDecoder(bytes.NewReader(content.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil || data.Version != storeVersion {
		return nil, ErrClientAuthorization
	}
	if err := validateRevocationResolutions(&data); err != nil {
		return nil, ErrClientAuthorization
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &data, nil
}

func sameAuthorizationFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Reject ambiguous duplicate keys and trailing documents as malformed state.
func validateClientAuthorizationJSON(data []byte) error {
	if !utf8.Valid(data) {
		return ErrClientAuthorization
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func(string) error
	value = func(shape string) error {
		token, err := decoder.Token()
		if err != nil {
			return ErrClientAuthorization
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return ErrClientAuthorization
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrClientAuthorization
				}
				seen[name] = true
				childShape, err := clientAuthorizationMemberShape(shape, name)
				if err != nil {
					return err
				}
				if err := value(childShape); err != nil {
					return err
				}
			}
		case '[':
			childShape := ""
			if shape == "daemon_revocations" {
				childShape = "daemon_revocation"
			}
			for decoder.More() {
				if err := value(childShape); err != nil {
					return err
				}
			}
		default:
			return ErrClientAuthorization
		}
		if _, err := decoder.Token(); err != nil {
			return ErrClientAuthorization
		}
		return nil
	}
	if err := value("store"); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrClientAuthorization
	}
	return nil
}

// encoding/json accepts case-insensitive aliases for struct fields and merges
// map values across them. Require the persisted schema's exact field names at
// struct positions while leaving client and connection map names untouched.
func clientAuthorizationMemberShape(shape, name string) (string, error) {
	switch shape {
	case "store":
		switch name {
		case "version", "authorized_clients":
			return "", nil
		case "server":
			return "identity", nil
		case "connections":
			return "connections", nil
		case "authorization_records":
			return "authorization_records", nil
		case "revocations":
			return "revocations", nil
		case "revocation_resolutions":
			return "revocation_resolutions", nil
		}
	case "identity":
		if name == "certificate" || name == "private_key" {
			return "", nil
		}
	case "connections":
		return "connection", nil
	case "authorization_records":
		return "authorization_record", nil
	case "revocations":
		return "revocation_record", nil
	case "revocation_resolutions":
		return "revocation_resolution", nil
	case "revocation_resolution":
		switch name {
		case "state", "captured", "recorded_at":
			return "", nil
		case "daemons":
			return "daemon_revocations", nil
		}
	case "daemon_revocation":
		if name == "instance_id" || name == "acknowledged" || name == "error" {
			return "", nil
		}
	case "authorization_record":
		switch name {
		case "name", "fingerprint", "grant_id", "created_at", "approved_at", "last_used_at", "revoked_at":
			return "", nil
		}
	case "revocation_record":
		switch name {
		case "operation_id", "name", "principal", "grant_id", "server_fingerprint", "revoked_at":
			return "", nil
		}
	case "connection":
		switch name {
		case "name", "address", "server_certificate":
			return "", nil
		case "client":
			return "identity", nil
		}
	default:
		return "", nil
	}
	return "", ErrClientAuthorization
}
