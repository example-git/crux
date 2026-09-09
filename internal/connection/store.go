package connection

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/lock"
)

const storeVersion = 1

var renameStoreFile = replaceConnectionFile

type Identity struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
}

type Connection struct {
	Name              string   `json:"name"`
	Address           string   `json:"address"`
	ServerCertificate string   `json:"server_certificate"`
	Client            Identity `json:"client"`
}

type Summary struct {
	Name    string
	Address string
}

type AuthorizedClient struct {
	Name        string
	Fingerprint string
}

type store struct {
	Version           int                   `json:"version"`
	Server            *Identity             `json:"server,omitempty"`
	AuthorizedClients map[string]string     `json:"authorized_clients,omitempty"`
	Connections       map[string]Connection `json:"connections,omitempty"`
}

func EnsureServerIdentity(ctx context.Context) (string, error) {
	var code string
	err := update(ctx, func(data *store) error {
		if data.Server == nil {
			identity, err := generateIdentity("Crux server", x509.ExtKeyUsageServerAuth)
			if err != nil {
				return err
			}
			data.Server = &identity
		}
		code = data.Server.Certificate
		return nil
	})
	return code, err
}

func ServerIdentity(ctx context.Context) (Identity, bool, error) {
	data, err := load(ctx)
	if err != nil {
		return Identity{}, false, err
	}
	if data.Server == nil {
		return Identity{}, false, nil
	}
	return *data.Server, true, nil
}

func NormalizeConnectionAddress(address string) (string, error) {
	if address != strings.TrimSpace(address) {
		return "", errors.New("connection address cannot contain surrounding whitespace")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "tcp" || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("connection address must use tcp://host:port: %s", address)
	}
	host := parsed.Hostname()
	portText := parsed.Port()
	if host == "" || portText == "" {
		return "", fmt.Errorf("connection address must include a host and port: %s", address)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("connection address has an invalid port: %s", address)
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsUnspecified() {
		return "", fmt.Errorf("connection address cannot use a wildcard host: %s", address)
	}
	if strings.EqualFold(host, "localhost") {
		host = "localhost"
	} else if ip != nil {
		host = ip.String()
	} else {
		host = strings.ToLower(host)
	}
	return "tcp://" + net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func NewClientIdentity(name string) (Identity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Identity{}, errors.New("connection name cannot be empty")
	}
	return generateIdentity("Crux client "+name, x509.ExtKeyUsageClientAuth)
}

func SaveConnection(ctx context.Context, created Connection) error {
	created.Name = strings.TrimSpace(created.Name)
	if created.Name == "" {
		return errors.New("connection name cannot be empty")
	}
	address, err := NormalizeConnectionAddress(created.Address)
	if err != nil {
		return err
	}
	created.Address = address
	created.ServerCertificate = strings.TrimSpace(created.ServerCertificate)
	if _, err := parseCertificate(created.ServerCertificate, x509.ExtKeyUsageServerAuth); err != nil {
		return fmt.Errorf("invalid server pairing code: %w", err)
	}
	if _, _, err := parseIdentity(created.Client, x509.ExtKeyUsageClientAuth); err != nil {
		return fmt.Errorf("invalid client identity: %w", err)
	}
	path, err := filepath.Abs(storePath())
	if err != nil {
		return err
	}
	return updateStoreAt(ctx, path, func(data *store) error {
		if _, exists := data.Connections[created.Name]; exists {
			return fmt.Errorf("connection already exists: %s", created.Name)
		}
		pending, _, err := readPendingPairings(path)
		if err != nil {
			return err
		}
		for _, entry := range pending.Entries {
			if pendingReservesName(entry, created.Name) {
				return &PairingPendingError{OperationID: entry.OperationID, Cause: errors.New("connection name is reserved by a pending identity")}
			}
		}
		data.Connections[created.Name] = created
		return nil
	}, nil)
}

func Add(ctx context.Context, name, address, serverCertificate string) (Connection, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Connection{}, "", errors.New("connection name cannot be empty")
	}
	address, err := NormalizeConnectionAddress(address)
	if err != nil {
		return Connection{}, "", err
	}
	if _, err := parseCertificate(serverCertificate, x509.ExtKeyUsageServerAuth); err != nil {
		return Connection{}, "", fmt.Errorf("invalid server pairing code: %w", err)
	}
	clientIdentity, err := NewClientIdentity(name)
	if err != nil {
		return Connection{}, "", err
	}
	created := Connection{
		Name:              name,
		Address:           address,
		ServerCertificate: strings.TrimSpace(serverCertificate),
		Client:            clientIdentity,
	}
	if err := SaveConnection(ctx, created); err != nil {
		return Connection{}, "", err
	}
	return created, clientIdentity.Certificate, nil
}

func AuthorizeClient(ctx context.Context, name, clientCertificate string) error {
	return authorizeClientWithCommit(ctx, name, clientCertificate, nil)
}

// authorizationCommit serializes the final file replacement with enrollment
// terminal state. Preparing and syncing the candidate does not grant access.
type authorizationCommit func(persist func() error) error

func authorizeClientWithCommit(ctx context.Context, name, clientCertificate string, commit authorizationCommit) error {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return err
	}
	return authorizeClientAt(ctx, path, nil, name, clientCertificate, commit)
}

func authorizeClientAt(ctx context.Context, path string, expectedServer *Identity, name, clientCertificate string, commit authorizationCommit) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("client name cannot be empty")
	}
	clientCertificate = strings.TrimSpace(clientCertificate)
	certificate, err := parseCertificate(clientCertificate, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return fmt.Errorf("invalid client pairing code: %w", err)
	}
	fingerprint := certificateFingerprint(certificate)
	return updateStoreAt(ctx, path, func(data *store) error {
		if data.Server == nil {
			return errors.New("server identity is not initialized")
		}
		if expectedServer != nil && *data.Server != *expectedServer {
			return errors.New("enrollment server identity changed")
		}
		if _, exists := data.AuthorizedClients[name]; exists {
			return fmt.Errorf("client is already authorized: %s", name)
		}
		for existingName, existingCode := range data.AuthorizedClients {
			existing, parseErr := parseCertificate(existingCode, x509.ExtKeyUsageClientAuth)
			if parseErr != nil {
				return fmt.Errorf("load authorized client %s: %w", existingName, parseErr)
			}
			if certificateFingerprint(existing) == fingerprint {
				return fmt.Errorf("client certificate is already authorized as %s", existingName)
			}
		}
		data.AuthorizedClients[name] = clientCertificate
		return nil
	}, commit)
}

func ListAuthorizedClients(ctx context.Context) ([]AuthorizedClient, error) {
	data, err := load(ctx)
	if err != nil {
		return nil, err
	}
	clients := make([]AuthorizedClient, 0, len(data.AuthorizedClients))
	for name, code := range data.AuthorizedClients {
		certificate, err := parseCertificate(code, x509.ExtKeyUsageClientAuth)
		if err != nil {
			return nil, fmt.Errorf("load authorized client %s: %w", name, err)
		}
		clients = append(clients, AuthorizedClient{Name: name, Fingerprint: certificateFingerprint(certificate)})
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].Name < clients[j].Name })
	return clients, nil
}

func RevokeClient(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("client name cannot be empty")
	}
	return update(ctx, func(data *store) error {
		if _, exists := data.AuthorizedClients[name]; !exists {
			return fmt.Errorf("authorized client not found: %s", name)
		}
		delete(data.AuthorizedClients, name)
		return nil
	})
}

func Get(ctx context.Context, name string) (Connection, bool, error) {
	data, err := load(ctx)
	if err != nil {
		return Connection{}, false, err
	}
	connection, ok := data.Connections[name]
	return connection, ok, nil
}

func List(ctx context.Context) ([]Summary, error) {
	data, err := load(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Summary, 0, len(data.Connections))
	for _, item := range data.Connections {
		result = append(result, Summary{Name: item.Name, Address: item.Address})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func load(ctx context.Context) (*store, error) {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return nil, err
	}
	return loadStoreAt(ctx, path)
}

func loadStoreAt(ctx context.Context, path string) (*store, error) {
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock connection store: %w", err)
	}
	defer release()
	return readStoreAt(path)
}

func update(ctx context.Context, apply func(*store) error) error {
	return updateWithCommit(ctx, apply, nil)
}

func updateWithCommit(ctx context.Context, apply func(*store) error, commit authorizationCommit) error {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return err
	}
	return updateStoreAt(ctx, path, apply, commit)
}

func updateStoreAt(ctx context.Context, path string, apply func(*store) error, commit authorizationCommit) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create connection store directory: %w", err)
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return fmt.Errorf("lock connection store: %w", err)
	}
	defer release()
	data, err := readStoreAt(path)
	if err != nil {
		return err
	}
	if err := apply(data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeStoreAt(path, data, commit)
}

func readStore() (*store, error) {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return nil, err
	}
	return readStoreAt(path)
}

func readStoreAt(path string) (*store, error) {
	data := &store{
		Version:           storeVersion,
		AuthorizedClients: map[string]string{},
		Connections:       map[string]Connection{},
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return data, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read connection store: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(data); err != nil {
		return nil, fmt.Errorf("decode connection store: %w", err)
	}
	if data.Version != storeVersion {
		return nil, fmt.Errorf("unsupported connection store version: %d", data.Version)
	}
	if data.AuthorizedClients == nil {
		data.AuthorizedClients = map[string]string{}
	}
	if data.Connections == nil {
		data.Connections = map[string]Connection{}
	}
	return data, nil
}

func writeStore(data *store) error {
	return writeStoreWithCommit(data, nil)
}

func writeStoreWithCommit(data *store, commit authorizationCommit) error {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return err
	}
	return writeStoreAt(path, data, commit)
}

func writeStoreAt(path string, data *store, commit authorizationCommit) error {
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode connection store: %w", err)
	}
	content = append(content, '\n')
	parent := filepath.Dir(path)
	temporary, err := os.CreateTemp(parent, ".connections-*.json")
	if err != nil {
		return fmt.Errorf("create temporary connection store: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	persist := func() error {
		if err := replaceStoreFile(temporaryPath, path); err != nil {
			return fmt.Errorf("replace connection store: %w", err)
		}
		return nil
	}
	if commit != nil {
		return commit(persist)
	}
	return persist()
}

func replaceStoreFile(temporaryPath, destination string) error {
	// One replacement operation preserves the original file on failure. Never
	// move it aside first: a process exit in that gap loses the live store.
	return renameStoreFile(temporaryPath, destination)
}

func generateIdentity(commonName string, usage x509.ExtKeyUsage) (Identity, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate identity key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return Identity{}, fmt.Errorf("generate certificate serial: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	if usage == x509.ExtKeyUsageServerAuth {
		template.DNSNames = []string{"crux-server"}
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return Identity{}, fmt.Errorf("create identity certificate: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return Identity{}, fmt.Errorf("encode identity key: %w", err)
	}
	return Identity{
		Certificate: base64.RawURLEncoding.EncodeToString(certificate),
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: privateDER,
		})),
	}, nil
}

func parseCertificate(code string, usage x509.ExtKeyUsage) (*x509.Certificate, error) {
	certificateDER, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(code))
	if err != nil {
		return nil, errors.New("pairing code is not valid base64")
	}
	if len(certificateDER) == 0 || len(certificateDER) > 8192 {
		return nil, errors.New("pairing certificate has an invalid size")
	}
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		return nil, errors.New("pairing code does not contain a certificate")
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return nil, errors.New("pairing certificate is not currently valid")
	}
	if certificate.IsCA || len(certificate.UnhandledCriticalExtensions) != 0 {
		return nil, errors.New("pairing certificate has unsupported constraints")
	}
	if certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return nil, errors.New("pairing certificate cannot authenticate signatures")
	}
	if !slicesContains(certificate.ExtKeyUsage, usage) {
		return nil, errors.New("pairing certificate has the wrong purpose")
	}
	if certificate.PublicKeyAlgorithm != x509.Ed25519 || certificate.SignatureAlgorithm != x509.PureEd25519 {
		return nil, errors.New("pairing certificate does not use Ed25519")
	}
	if _, ok := certificate.PublicKey.(ed25519.PublicKey); !ok {
		return nil, errors.New("pairing certificate does not use an Ed25519 key")
	}
	if err := certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature); err != nil {
		return nil, errors.New("pairing certificate signature is invalid")
	}
	if usage == x509.ExtKeyUsageServerAuth {
		if err := certificate.VerifyHostname("crux-server"); err != nil {
			return nil, errors.New("pairing certificate is not a Crux server identity")
		}
	}
	return certificate, nil
}

func certificateFingerprint(certificate *x509.Certificate) string {
	digest := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(digest[:])
}

func parseIdentity(identity Identity, usage x509.ExtKeyUsage) (*x509.Certificate, ed25519.PrivateKey, error) {
	certificate, err := parseCertificate(identity.Certificate, usage)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode([]byte(identity.PrivateKey))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, nil, errors.New("identity private key is invalid")
	}
	privateKeyValue, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, errors.New("identity private key is invalid")
	}
	privateKey, ok := privateKeyValue.(ed25519.PrivateKey)
	if !ok || !privateKey.Public().(ed25519.PublicKey).Equal(certificate.PublicKey) {
		return nil, nil, errors.New("identity private key does not match its certificate")
	}
	return certificate, privateKey, nil
}

func slicesContains(values []x509.ExtKeyUsage, target x509.ExtKeyUsage) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func storePath() string {
	return filepath.Join(config.GlobalWorkspaceDir(), "connections.json")
}

func lockPath() string {
	return storePath() + ".lock"
}
