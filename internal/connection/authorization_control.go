package connection

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/google/uuid"
)

const authorizationControlPath = "/v1/reconcile-authorization"
const maxAuthorizationDaemons = 32

// This file is a private local control capability, never an API discovery DTO.
// Network peers cannot reach the loopback-only listener without its random key.
type authorizationDaemon struct {
	Version           int    `json:"version"`
	InstanceID        string `json:"instance_id"`
	Address           string `json:"address"`
	Token             string `json:"token"`
	ServerFingerprint string `json:"server_fingerprint"`
}

type authorizationControl struct {
	server *http.Server
	path   string
}

type authorizationControlReply struct {
	Version           int    `json:"version"`
	InstanceID        string `json:"instance_id"`
	OperationID       string `json:"operation_id"`
	Principal         string `json:"principal"`
	ServerFingerprint string `json:"server_fingerprint"`
	Joined            bool   `json:"joined"`
}

func authorizationDaemonDir(path string) string { return path + ".daemons" }

func startAuthorizationControl(l *liveAuthorization) (*authorizationControl, error) {
	release, err := lock.File(l.ctx, l.authorization.path+".lock")
	if err != nil {
		return nil, err
	}
	defer release()
	dir := authorizationDaemonDir(l.authorization.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) >= maxAuthorizationDaemons {
		return nil, errors.New("too many registered authorization daemons; inspect stale local daemon records")
	}
	listener, err := new(net.ListenConfig).Listen(l.ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		listener.Close()
		return nil, err
	}
	serverHash := sha256.Sum256(l.authorization.serverCertificate)
	record := authorizationDaemon{Version: 1, InstanceID: uuid.NewString(), Address: "http://" + listener.Addr().String(), Token: base64.RawURLEncoding.EncodeToString(token), ServerFingerprint: hex.EncodeToString(serverHash[:])}
	path := filepath.Join(dir, record.InstanceID+".json")
	data, err := json.Marshal(record)
	if err != nil {
		listener.Close()
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		listener.Close()
		return nil, err
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	mux := http.NewServeMux()
	registerAuthorizationUseControl(mux, l, record)
	mux.HandleFunc("POST "+authorizationControlPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+record.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		var receipt RevocationRecord
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&receipt) != nil || ensureJSONEnd(decoder) != nil || receipt.ServerFingerprint != record.ServerFingerprint {
			http.Error(w, "invalid revocation receipt", http.StatusBadRequest)
			return
		}
		if err := l.reconcileReceipt(r.Context(), receipt); err != nil {
			http.Error(w, "revocation is saved but this daemon has not confirmed the exact drain", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authorizationControlReply{Version: 1, InstanceID: record.InstanceID, OperationID: receipt.OperationID, Principal: receipt.Principal, ServerFingerprint: record.ServerFingerprint, Joined: true})
	})
	control := &authorizationControl{path: path, server: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}}
	go func() { _ = control.server.Serve(listener) }()
	return control, nil
}

func (c *authorizationControl) remove() { _ = os.Remove(c.path) }

func reconcileAuthorizationDaemons(ctx context.Context, path string, receipt RevocationRecord, captured []DaemonRevocation) ([]DaemonRevocation, error) {
	var outcomes []DaemonRevocation
	var result error
	for _, target := range captured {
		if target.Acknowledged {
			outcomes = append(outcomes, target)
			continue
		}
		outcome := DaemonRevocation{InstanceID: target.InstanceID}
		daemonPath, err := revocationDaemonPath(path, target.InstanceID)
		var daemon authorizationDaemon
		if err == nil {
			daemon, err = readAuthorizationDaemon(daemonPath)
		}
		if err == nil && daemon.ServerFingerprint != receipt.ServerFingerprint {
			err = errors.New("daemon server identity differs from the revoked grant")
		}
		if err == nil {
			err = requestAuthorizationDrain(ctx, daemon, receipt)
		}
		if err != nil {
			outcome.Error = "live cancellation has not been acknowledged"
			result = errors.Join(result, fmt.Errorf("daemon %s: %w", outcome.InstanceID, err))
		} else {
			outcome.Acknowledged = true
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, result
}

func readAuthorizationDaemon(path string) (authorizationDaemon, error) {
	var value authorizationDaemon
	file, err := openClientAuthorization(path)
	if err != nil {
		return value, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return value, errors.New("invalid authorization daemon record")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 8193))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || ensureJSONEnd(decoder) != nil {
		return value, errors.New("invalid authorization daemon record")
	}
	if value.Version != 1 || filepath.Base(path) != value.InstanceID+".json" {
		return value, errors.New("invalid authorization daemon identity")
	}
	if _, err := uuid.Parse(value.InstanceID); err != nil {
		return value, errors.New("invalid authorization daemon identity")
	}
	address, err := url.Parse(value.Address)
	if err != nil || address.Scheme != "http" || address.User != nil || address.Hostname() != "127.0.0.1" || address.Path != "" || address.RawQuery != "" || address.Fragment != "" {
		return value, errors.New("authorization control must be local")
	}
	port, err := strconv.Atoi(address.Port())
	if err != nil || port < 1 || port > 65535 {
		return value, errors.New("authorization control port is invalid")
	}
	token, err := base64.RawURLEncoding.DecodeString(value.Token)
	if err != nil || len(token) != 32 {
		return value, errors.New("invalid authorization control key")
	}
	return value, nil
}

func requestAuthorizationDrain(ctx context.Context, daemon authorizationDaemon, receipt RevocationRecord) error {
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(wait, http.MethodPost, daemon.Address+authorizationControlPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+daemon.Token)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("local daemon is unavailable or its drain did not finish")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("local daemon did not acknowledge the exact revocation")
	}
	var reply authorizationControlReply
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8193))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&reply) != nil || ensureJSONEnd(decoder) != nil || reply.Version != 1 || !reply.Joined || reply.InstanceID != daemon.InstanceID || reply.OperationID != receipt.OperationID || reply.Principal != receipt.Principal || reply.ServerFingerprint != receipt.ServerFingerprint {
		return errors.New("invalid local revocation acknowledgement")
	}
	return nil
}
