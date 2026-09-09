package connection

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

const authorizationUsePath = "/v1/authorization-use"
const maxAuthorizationUseBytes = 128 << 10
const authorizationUseTimeout = 2 * time.Second

type authorizationUseRequest struct {
	Version           int    `json:"version"`
	InstanceID        string `json:"instance_id"`
	ServerFingerprint string `json:"server_fingerprint"`
}
type authorizationUseObservation struct {
	Principal  string    `json:"principal"`
	GrantID    string    `json:"grant_id"`
	LastUsedAt time.Time `json:"last_used_at"`
}
type authorizationUseReply struct {
	authorizationUseRequest
	CheckedAt    time.Time                     `json:"checked_at"`
	Observations []authorizationUseObservation `json:"observations"`
}

func registerAuthorizationUseControl(mux *http.ServeMux, live *liveAuthorization, daemon authorizationDaemon) {
	mux.HandleFunc("POST "+authorizationUsePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+daemon.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		var request authorizationUseRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || ensureJSONEnd(decoder) != nil || request.Version != 1 || request.InstanceID != daemon.InstanceID || request.ServerFingerprint != daemon.ServerFingerprint {
			http.Error(w, "invalid observation request", http.StatusBadRequest)
			return
		}
		observations, err := live.authorizationUses(r.Context())
		if err != nil {
			http.Error(w, "live observations unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authorizationUseReply{authorizationUseRequest: request, CheckedAt: time.Now().UTC(), Observations: observations})
	})
}

func (l *liveAuthorization) authorizationUses(ctx context.Context) ([]authorizationUseObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// This is an observation, not admission: no store lock creation, rewrite,
	// grant reconciliation, or authority callback belongs on this route.
	data, err := readStoreAt(l.authorization.path)
	if err != nil {
		return nil, err
	}
	grants, err := l.authorization.grants(data)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ctx.Err(); err != nil {
		return nil, err
	}
	if len(grants) > authorizationReservationLimit {
		return nil, errors.New("observation capacity exceeded")
	}
	var result []authorizationUseObservation
	for principal, use := range l.uses {
		if grant, ok := grants[principal]; ok && grant == use.grantID {
			result = append(result, authorizationUseObservation{principal, grant, use.at})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Principal < result[j].Principal })
	return result, nil
}

type collectedAuthorizationUses struct {
	server       string
	state        string
	grants       map[string]string
	observations map[string]authorizationUseObservation
}

func authorizationUseServerFingerprint(data *store) string {
	if data == nil || data.Server == nil {
		return ""
	}
	certificate, err := parseCertificate(data.Server.Certificate, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return ""
	}
	return certificateFingerprint(certificate)
}

func collectAuthorizationUses(ctx context.Context, path string, data *store) collectedAuthorizationUses {
	result := collectedAuthorizationUses{server: authorizationUseServerFingerprint(data), state: "unavailable", grants: map[string]string{}, observations: map[string]authorizationUseObservation{}}
	for _, code := range data.AuthorizedClients {
		certificate, err := authorizationRecordCertificate(code)
		if err != nil {
			return result
		}
		principal := certificateFingerprint(certificate)
		result.grants[principal] = data.AuthorizationRecords[principal].GrantID
	}
	if result.server == "" {
		return result
	}
	directory, err := os.Open(authorizationDaemonDir(path))
	if errors.Is(err, os.ErrNotExist) {
		result.state = "no-live-daemons"
		return result
	}
	if err != nil {
		return result
	}
	entries, err := directory.ReadDir(maxAuthorizationDaemons + 1)
	_ = directory.Close()
	if err != nil && !errors.Is(err, io.EOF) || len(entries) > maxAuthorizationDaemons {
		return result
	}
	if len(entries) == 0 {
		result.state = "no-live-daemons"
		return result
	}
	var mu sync.Mutex
	var workers sync.WaitGroup
	succeeded := 0
	for _, entry := range entries {
		workers.Go(func() {
			daemon, err := readAuthorizationDaemon(filepath.Join(authorizationDaemonDir(path), entry.Name()))
			if err != nil || daemon.ServerFingerprint != result.server {
				return
			}
			reply, err := requestAuthorizationUses(ctx, daemon)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			succeeded++
			for _, observation := range reply.Observations {
				// Never replace another grant's observation based only on time.
				grant, ok := result.grants[observation.Principal]
				if !ok || grant != observation.GrantID {
					continue
				}
				if old, exists := result.observations[observation.Principal]; !exists || old.LastUsedAt.Before(observation.LastUsedAt) {
					result.observations[observation.Principal] = observation
				}
			}
		})
	}
	workers.Wait()
	if succeeded == len(entries) {
		result.state = "not-observed"
	} else if succeeded > 0 {
		result.state = "partial"
	}
	return result
}

func requestAuthorizationUses(ctx context.Context, daemon authorizationDaemon) (authorizationUseReply, error) {
	var reply authorizationUseReply
	body, err := json.Marshal(authorizationUseRequest{1, daemon.InstanceID, daemon.ServerFingerprint})
	if err != nil {
		return reply, err
	}
	wait, cancel := context.WithTimeout(ctx, authorizationUseTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(wait, http.MethodPost, daemon.Address+authorizationUsePath, bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	request.Header.Set("Authorization", "Bearer "+daemon.Token)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return reply, errors.New("live observations unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return reply, errors.New("live observations unavailable")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maxAuthorizationUseBytes+1))
	if err != nil || len(content) > maxAuthorizationUseBytes {
		return reply, errors.New("invalid live observations")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&reply) != nil || ensureJSONEnd(decoder) != nil || reply.Version != 1 || reply.InstanceID != daemon.InstanceID || reply.ServerFingerprint != daemon.ServerFingerprint || reply.CheckedAt.IsZero() || len(reply.Observations) > authorizationReservationLimit {
		return authorizationUseReply{}, errors.New("invalid live observation identity")
	}
	seen := map[string]bool{}
	for _, observation := range reply.Observations {
		principal, err := hex.DecodeString(observation.Principal)
		if err != nil || len(principal) != 32 || seen[observation.Principal] || observation.LastUsedAt.IsZero() || observation.LastUsedAt.After(reply.CheckedAt) {
			return authorizationUseReply{}, errors.New("invalid live observation")
		}
		if observation.GrantID != "" {
			if _, err := uuid.Parse(observation.GrantID); err != nil {
				return authorizationUseReply{}, errors.New("invalid observed grant")
			}
		}
		seen[observation.Principal] = true
	}
	return reply, nil
}
