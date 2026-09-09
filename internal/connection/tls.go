package connection

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"path/filepath"
)

func ServerTLSConfig(ctx context.Context) (*tls.Config, error) {
	tlsConfig, _, err := ServerTLSConfigWithAuthorization(ctx)
	return tlsConfig, err
}

// ServerTLSConfigWithAuthorization captures one authorization store path for
// both TLS admission and subsequent requests on authenticated connections.
func ServerTLSConfigWithAuthorization(ctx context.Context) (*tls.Config, *ClientAuthorization, error) {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return nil, nil, ErrClientAuthorization
	}
	data, err := readClientAuthorization(ctx, path)
	if err != nil {
		return nil, nil, ErrClientAuthorization
	}
	if data.Server == nil {
		return nil, nil, errors.New("server identity is not initialized; run `crux connections server-init`")
	}
	if len(data.AuthorizedClients) == 0 {
		return nil, nil, errors.New("no clients are authorized; run `crux connections authorize`")
	}
	serverCertificate, serverPrivateKey, err := parseIdentity(*data.Server, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, nil, fmt.Errorf("load server identity: %w", err)
	}
	authorization := &ClientAuthorization{path: path, serverCertificate: serverCertificate.Raw}
	clientRoots, err := authorization.roots(data)
	if err != nil {
		return nil, nil, ErrClientAuthorization
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{serverCertificate.Raw},
			PrivateKey:  serverPrivateKey,
		}},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientRoots,
	}
	tlsConfig.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		current, err := readClientAuthorization(hello.Context(), authorization.path)
		if err != nil {
			return nil, ErrClientAuthorization
		}
		roots, err := authorization.roots(current)
		if err != nil {
			return nil, ErrClientAuthorization
		}
		candidate := tlsConfig.Clone()
		candidate.GetConfigForClient = nil
		candidate.ClientCAs = roots
		verifyConnection := candidate.VerifyConnection
		// VerifyConnection also runs for resumed sessions, whose verified
		// certificate chains otherwise come from the earlier handshake.
		candidate.VerifyConnection = func(state tls.ConnectionState) error {
			if err := authorization.Authorize(hello.Context(), state); err != nil {
				return err
			}
			if verifyConnection != nil {
				return verifyConnection(state)
			}
			return nil
		}
		return candidate, nil
	}
	return tlsConfig, authorization, nil
}

func ClientTLSConfig(connection Connection) (*tls.Config, error) {
	serverCertificate, err := parseCertificate(connection.ServerCertificate, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, fmt.Errorf("load pinned server identity: %w", err)
	}
	clientCertificate, clientPrivateKey, err := parseIdentity(connection.Client, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, fmt.Errorf("load client identity: %w", err)
	}
	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(serverCertificate)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: "crux-server",
		RootCAs:    serverRoots,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{clientCertificate.Raw},
			PrivateKey:  clientPrivateKey,
		}},
	}, nil
}
