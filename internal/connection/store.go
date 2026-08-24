package connection

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/lock"
)

const storeVersion = 1

var renameStoreFile = os.Rename

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

func Add(ctx context.Context, name, address, serverCertificate string) (Connection, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Connection{}, "", errors.New("connection name cannot be empty")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "tcp" || parsed.Host == "" || parsed.Path != "" {
		return Connection{}, "", fmt.Errorf("connection address must use tcp://host:port: %s", address)
	}
	if _, err := parseCertificate(serverCertificate, x509.ExtKeyUsageServerAuth); err != nil {
		return Connection{}, "", fmt.Errorf("invalid server pairing code: %w", err)
	}
	clientIdentity, err := generateIdentity("Crux client "+name, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return Connection{}, "", err
	}
	created := Connection{
		Name:              name,
		Address:           address,
		ServerCertificate: serverCertificate,
		Client:            clientIdentity,
	}
	err = update(ctx, func(data *store) error {
		if _, exists := data.Connections[name]; exists {
			return fmt.Errorf("connection already exists: %s", name)
		}
		data.Connections[name] = created
		return nil
	})
	if err != nil {
		return Connection{}, "", err
	}
	return created, clientIdentity.Certificate, nil
}

func AuthorizeClient(ctx context.Context, name, clientCertificate string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("client name cannot be empty")
	}
	if _, err := parseCertificate(clientCertificate, x509.ExtKeyUsageClientAuth); err != nil {
		return fmt.Errorf("invalid client pairing code: %w", err)
	}
	return update(ctx, func(data *store) error {
		if data.Server == nil {
			return errors.New("server identity is not initialized")
		}
		if _, exists := data.AuthorizedClients[name]; exists {
			return fmt.Errorf("client is already authorized: %s", name)
		}
		data.AuthorizedClients[name] = clientCertificate
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
	release, err := lock.File(ctx, lockPath())
	if err != nil {
		return nil, fmt.Errorf("lock connection store: %w", err)
	}
	defer release()
	return readStore()
}

func update(ctx context.Context, apply func(*store) error) error {
	if err := os.MkdirAll(filepath.Dir(storePath()), 0o700); err != nil {
		return fmt.Errorf("create connection store directory: %w", err)
	}
	release, err := lock.File(ctx, lockPath())
	if err != nil {
		return fmt.Errorf("lock connection store: %w", err)
	}
	defer release()
	data, err := readStore()
	if err != nil {
		return err
	}
	if err := apply(data); err != nil {
		return err
	}
	return writeStore(data)
}

func readStore() (*store, error) {
	data := &store{
		Version:           storeVersion,
		AuthorizedClients: map[string]string{},
		Connections:       map[string]Connection{},
	}
	content, err := os.ReadFile(storePath())
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
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode connection store: %w", err)
	}
	content = append(content, '\n')
	parent := filepath.Dir(storePath())
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
	if err := replaceStoreFile(temporaryPath, storePath()); err != nil {
		return fmt.Errorf("replace connection store: %w", err)
	}
	return nil
}

func replaceStoreFile(temporaryPath, destination string) error {
	if err := renameStoreFile(temporaryPath, destination); err == nil {
		return nil
	}
	if _, err := os.Lstat(destination); err != nil {
		return err
	}
	backup, err := os.CreateTemp(filepath.Dir(destination), ".connections-backup-*.json")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		os.Remove(backupPath)
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	if err := renameStoreFile(destination, backupPath); err != nil {
		return err
	}
	if err := renameStoreFile(temporaryPath, destination); err != nil {
		if restoreErr := renameStoreFile(backupPath, destination); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore previous connection store: %w", restoreErr))
		}
		return err
	}
	_ = os.Remove(backupPath)
	return nil
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
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		return nil, errors.New("pairing code does not contain a certificate")
	}
	if time.Now().Before(certificate.NotBefore) || time.Now().After(certificate.NotAfter) {
		return nil, errors.New("pairing certificate is not currently valid")
	}
	if !slicesContains(certificate.ExtKeyUsage, usage) {
		return nil, errors.New("pairing certificate has the wrong purpose")
	}
	if _, ok := certificate.PublicKey.(ed25519.PublicKey); !ok {
		return nil, errors.New("pairing certificate does not use an Ed25519 key")
	}
	return certificate, nil
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
