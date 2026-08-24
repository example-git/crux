package connection

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

func ServerTLSConfig(ctx context.Context) (*tls.Config, error) {
	data, err := load(ctx)
	if err != nil {
		return nil, err
	}
	if data.Server == nil {
		return nil, errors.New("server identity is not initialized; run `crux connections server-init`")
	}
	if len(data.AuthorizedClients) == 0 {
		return nil, errors.New("no clients are authorized; run `crux connections authorize`")
	}
	serverCertificate, serverPrivateKey, err := parseIdentity(*data.Server, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, fmt.Errorf("load server identity: %w", err)
	}
	clientRoots := x509.NewCertPool()
	for name, code := range data.AuthorizedClients {
		certificate, err := parseCertificate(code, x509.ExtKeyUsageClientAuth)
		if err != nil {
			return nil, fmt.Errorf("load authorized client %s: %w", name, err)
		}
		clientRoots.AddCert(certificate)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{serverCertificate.Raw},
			PrivateKey:  serverPrivateKey,
		}},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientRoots,
	}, nil
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
