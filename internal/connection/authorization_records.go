package connection

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Missing dates mean unknown. Certificate validity dates are never presented as
// a historical creation, approval or use event.
type AuthorizationRecord struct {
	Authorized  bool       `json:"-"`
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint"`
	GrantID     string     `json:"grant_id,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

type RevocationRecord struct {
	OperationID       string    `json:"operation_id"`
	Name              string    `json:"name"`
	Principal         string    `json:"principal"`
	GrantID           string    `json:"grant_id,omitempty"`
	ServerFingerprint string    `json:"server_fingerprint"`
	RevokedAt         time.Time `json:"revoked_at"`
}

type DaemonRevocation struct {
	InstanceID   string `json:"instance_id"`
	Acknowledged bool   `json:"acknowledged"`
	Error        string `json:"error,omitempty"`
}

type RevocationOutcome struct {
	RevocationRecord
	Saved      bool               `json:"saved"`
	Daemons    []DaemonRevocation `json:"daemons,omitempty"`
	Resolution string             `json:"resolution,omitempty"`
}

type explicitApprovalKey struct{}

func recordAuthorization(ctx context.Context, data *store, name, principal string) {
	if data.AuthorizationRecords == nil {
		data.AuthorizationRecords = map[string]AuthorizationRecord{}
	}
	now := time.Now().UTC()
	record := AuthorizationRecord{Name: name, Fingerprint: principal, GrantID: uuid.NewString(), CreatedAt: &now}
	if approved, _ := ctx.Value(explicitApprovalKey{}).(bool); approved {
		record.ApprovedAt = &now
	}
	data.AuthorizationRecords[principal] = record
}

func ListAuthorizationRecords(ctx context.Context) ([]AuthorizationRecord, error) {
	data, err := load(ctx)
	if err != nil {
		return nil, err
	}
	records := make(map[string]AuthorizationRecord, len(data.AuthorizationRecords))
	for principal, record := range data.AuthorizationRecords {
		records[principal] = record
	}
	for name, code := range data.AuthorizedClients {
		cert, err := authorizationRecordCertificate(code)
		if err != nil {
			return nil, err
		}
		principal := certificateFingerprint(cert)
		record := records[principal]
		record.Name, record.Fingerprint = name, principal
		record.Authorized = true
		records[principal] = record
	}
	result := make([]AuthorizationRecord, 0, len(records))
	for _, record := range records {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Fingerprint < result[j].Fingerprint
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func persistRevocation(ctx context.Context, name, operationID string) (string, RevocationOutcome, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", RevocationOutcome{}, errors.New("client name cannot be empty")
	}
	if operationID != "" {
		if _, err := uuid.Parse(operationID); err != nil {
			return "", RevocationOutcome{}, errors.New("invalid revocation operation")
		}
	}
	path, err := filepath.Abs(storePath())
	if err != nil {
		return "", RevocationOutcome{}, err
	}
	var outcome RevocationOutcome
	if operationID != "" {
		data, err := loadStoreAt(ctx, path)
		if err != nil {
			return path, outcome, err
		}
		record, ok := data.Revocations[operationID]
		if !ok || record.Name != name {
			return path, outcome, errors.New("revocation operation is unavailable or belongs to another client")
		}
		outcome.RevocationRecord, outcome.Saved = record, true
		return path, outcome, nil
	}
	err = updateStoreAt(ctx, path, func(data *store) error {
		code, ok := data.AuthorizedClients[name]
		if !ok {
			return fmt.Errorf("authorized client not found: %s", name)
		}
		cert, err := authorizationRecordCertificate(code)
		if err != nil {
			return err
		}
		if data.Server == nil {
			return errors.New("server identity is unavailable")
		}
		server, _, err := parseIdentity(*data.Server, x509.ExtKeyUsageServerAuth)
		if err != nil {
			return err
		}
		principal := certificateFingerprint(cert)
		now := time.Now().UTC()
		record := data.AuthorizationRecords[principal]
		record.Name, record.Fingerprint, record.RevokedAt = name, principal, &now
		if data.AuthorizationRecords == nil {
			data.AuthorizationRecords = map[string]AuthorizationRecord{}
		}
		data.AuthorizationRecords[principal] = record
		receipt := RevocationRecord{OperationID: uuid.NewString(), Name: name, Principal: principal, GrantID: record.GrantID, ServerFingerprint: certificateFingerprint(server), RevokedAt: now}
		if data.Revocations == nil {
			data.Revocations = map[string]RevocationRecord{}
		}
		data.Revocations[receipt.OperationID] = receipt
		delete(data.AuthorizedClients, name)
		outcome.RevocationRecord = receipt
		return nil
	}, nil)
	outcome.Saved = err == nil
	return path, outcome, err
}

// RevokeClientWithOutcome retries acknowledgement by the original operation
// when operationID is supplied. It never removes a later replacement grant.
func RevokeClientWithOutcome(ctx context.Context, name, operationID string) (RevocationOutcome, error) {
	path, outcome, err := persistRevocation(ctx, name, operationID)
	if err != nil {
		return outcome, err
	}
	resolution, err := prepareRevocationResolution(ctx, path, outcome.RevocationRecord)
	if err != nil {
		return outcome, err
	}
	if !resolvedRevocation(resolution) {
		observed, drainErr := reconcileAuthorizationDaemons(ctx, path, outcome.RevocationRecord, resolution.Daemons)
		stored, saveErr := saveRevocationResolution(ctx, path, outcome.RevocationRecord, observed)
		if saveErr != nil {
			outcome.Daemons = observed
			return outcome, errors.Join(drainErr, saveErr)
		}
		resolution = stored
	}
	outcome.Daemons, outcome.Resolution = resolution.Daemons, resolution.State
	return outcome, revocationResolutionError(resolution)
}

// Reading/removing a historical identity must not require its certificate to
// remain within its validity period. This parser cannot grant authentication.
func authorizationRecordCertificate(code string) (*x509.Certificate, error) {
	der, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(code))
	if err != nil || len(der) == 0 || len(der) > 8192 {
		return nil, errors.New("invalid recorded client certificate")
	}
	return x509.ParseCertificate(der)
}
