package connection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const authorizationReservationLimit = 256
const authorizationHistoryLimit = 256

// Kept separately so the original exact revocation receipt never changes.
// Missing legacy metadata is unknown, never evidence that work drained.
type RevocationResolution struct {
	State      string             `json:"state"`
	Captured   bool               `json:"captured"`
	Daemons    []DaemonRevocation `json:"daemons,omitempty"`
	RecordedAt time.Time          `json:"recorded_at"`
}
type RevocationHistoryEntry struct {
	RevocationRecord
	Resolution RevocationResolution `json:"resolution"`
}
type AuthorizationHistory struct {
	ActiveGrants int                      `json:"active_grants"`
	Unresolved   int                      `json:"unresolved"`
	Capacity     int                      `json:"capacity"`
	OverBudget   bool                     `json:"over_budget"`
	Revocations  []RevocationHistoryEntry `json:"revocations"`
}

func resolvedRevocation(value RevocationResolution) bool {
	switch value.State {
	case "acknowledged", "no-live-daemons-observed", "abandoned":
		return true
	default:
		return false
	}
}
func unresolvedRevocations(data *store) int {
	count := 0
	for operation := range data.Revocations {
		if !resolvedRevocation(data.RevocationResolutions[operation]) {
			count++
		}
	}
	return count
}
func admitAuthorizationHistory(data *store, name string) error {
	if len(name) > 256 || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n\t") {
		return errors.New("client name must be valid text of at most 256 bytes without control separators")
	}
	// Each active grant reserves one future revocation. Removing it and
	// adding an unresolved receipt cannot consume additional capacity.
	if len(data.AuthorizedClients)+unresolvedRevocations(data) >= authorizationReservationLimit {
		return errors.New("authorization history capacity is reserved; inspect connections revocations and recover or explicitly abandon unresolved receipts before authorizing a new client")
	}
	return nil
}

// Caller holds the captured store lock. Preserve all active grant IDs and
// unresolved legacy debt even when the old store exceeds the new budget.
func pruneAuthorizationHistory(data *store, keep int) {
	var completed []RevocationRecord
	for operation, record := range data.Revocations {
		if resolvedRevocation(data.RevocationResolutions[operation]) {
			completed = append(completed, record)
		}
	}
	slices.SortFunc(completed, func(a, b RevocationRecord) int {
		if order := a.RevokedAt.Compare(b.RevokedAt); order != 0 {
			return order
		}
		return strings.Compare(a.OperationID, b.OperationID)
	})
	for _, record := range completed[:max(0, len(completed)-keep)] {
		delete(data.Revocations, record.OperationID)
		delete(data.RevocationResolutions, record.OperationID)
	}
	protected := map[string]bool{}
	for _, code := range data.AuthorizedClients {
		cert, err := authorizationRecordCertificate(code)
		if err != nil {
			return
		} // Do not guess which active metadata is needed.
		protected[certificateFingerprint(cert)] = true
	}
	for operation, record := range data.Revocations {
		if !resolvedRevocation(data.RevocationResolutions[operation]) {
			protected[record.Principal] = true
		}
	}
	var inactive []string
	for principal := range data.AuthorizationRecords {
		if !protected[principal] {
			inactive = append(inactive, principal)
		}
	}
	slices.SortFunc(inactive, func(a, b string) int {
		left, right := data.AuthorizationRecords[a], data.AuthorizationRecords[b]
		var x, y time.Time
		if left.RevokedAt != nil {
			x = *left.RevokedAt
		}
		if right.RevokedAt != nil {
			y = *right.RevokedAt
		}
		if order := x.Compare(y); order != 0 {
			return order
		}
		return strings.Compare(a, b)
	})
	for _, principal := range inactive[:max(0, len(inactive)-keep)] {
		delete(data.AuthorizationRecords, principal)
	}
}

func ListRevocationHistory(ctx context.Context) (AuthorizationHistory, error) {
	data, err := load(ctx)
	if err != nil {
		return AuthorizationHistory{}, err
	}
	result := AuthorizationHistory{ActiveGrants: len(data.AuthorizedClients), Unresolved: unresolvedRevocations(data), Capacity: authorizationReservationLimit}
	result.OverBudget = result.ActiveGrants+result.Unresolved > result.Capacity
	for operation, record := range data.Revocations {
		resolution := data.RevocationResolutions[operation]
		if resolution.State == "" {
			resolution.State = "pending"
		}
		resolution.Daemons = slices.Clone(resolution.Daemons)
		result.Revocations = append(result.Revocations, RevocationHistoryEntry{record, resolution})
	}
	slices.SortFunc(result.Revocations, func(a, b RevocationHistoryEntry) int {
		if order := a.RevokedAt.Compare(b.RevokedAt); order != 0 {
			return order
		}
		return strings.Compare(a.OperationID, b.OperationID)
	})
	return result, nil
}
func exactRevocation(data *store, receipt RevocationRecord) error {
	if stored, found := data.Revocations[receipt.OperationID]; !found || stored != receipt {
		return errors.New("the exact revocation receipt is unavailable or changed")
	}
	return nil
}

// Capture after grant removal and before drain calls. Later daemon instances
// cannot admit the removed grant. Missing captured instances remain unknown.
func prepareRevocationResolution(ctx context.Context, path string, receipt RevocationRecord) (RevocationResolution, error) {
	var result RevocationResolution
	err := updateStoreAt(ctx, path, func(data *store) error {
		if err := exactRevocation(data, receipt); err != nil {
			return err
		}
		result = data.RevocationResolutions[receipt.OperationID]
		if result.Captured || resolvedRevocation(result) {
			return nil
		}
		entries, err := os.ReadDir(authorizationDaemonDir(path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("revocation saved; cannot capture registered daemons")
		}
		if len(entries) > maxAuthorizationDaemons {
			return errors.New("revocation saved; daemon registry exceeds its bound")
		}
		result = RevocationResolution{State: "pending", Captured: true, RecordedAt: time.Now().UTC()}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if _, err := uuid.Parse(id); err != nil {
				return errors.New("revocation saved; daemon registry contains an invalid identity")
			}
			result.Daemons = append(result.Daemons, DaemonRevocation{InstanceID: id})
		}
		if len(result.Daemons) == 0 {
			result.State = "no-live-daemons-observed"
		}
		if data.RevocationResolutions == nil {
			data.RevocationResolutions = map[string]RevocationResolution{}
		}
		data.RevocationResolutions[receipt.OperationID] = result
		return nil
	}, nil)
	return result, err
}
func saveRevocationResolution(ctx context.Context, path string, receipt RevocationRecord, observed []DaemonRevocation) (RevocationResolution, error) {
	var result RevocationResolution
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := updateStoreAt(finish, path, func(data *store) error {
		if err := exactRevocation(data, receipt); err != nil {
			return err
		}
		result = data.RevocationResolutions[receipt.OperationID]
		if !result.Captured {
			return errors.New("revocation has no captured daemon set")
		}
		if resolvedRevocation(result) {
			return nil
		}
		result.Daemons = slices.Clone(result.Daemons)
		all := true
		for i := range result.Daemons {
			for _, observation := range observed {
				if observation.InstanceID == result.Daemons[i].InstanceID && !result.Daemons[i].Acknowledged {
					result.Daemons[i] = observation
				}
			}
			all = all && result.Daemons[i].Acknowledged
		}
		if all && len(result.Daemons) > 0 {
			result.State = "acknowledged"
		}
		result.RecordedAt = time.Now().UTC()
		data.RevocationResolutions[receipt.OperationID] = result
		return nil
	}, nil)
	return result, err
}
func AbandonRevocationAcknowledgement(ctx context.Context, name, operationID string) error {
	if _, err := uuid.Parse(operationID); err != nil || strings.TrimSpace(name) == "" {
		return errors.New("an exact client name and revocation operation are required")
	}
	return update(ctx, func(data *store) error {
		receipt, found := data.Revocations[operationID]
		if !found || receipt.Name != strings.TrimSpace(name) {
			return errors.New("revocation operation is unavailable or belongs to another client")
		}
		resolution := data.RevocationResolutions[operationID]
		if resolvedRevocation(resolution) {
			return nil
		}
		resolution.State, resolution.RecordedAt = "abandoned", time.Now().UTC()
		if data.RevocationResolutions == nil {
			data.RevocationResolutions = map[string]RevocationResolution{}
		}
		data.RevocationResolutions[operationID] = resolution
		return nil
	})
}
func PruneAuthorizationHistory(ctx context.Context) error {
	return update(ctx, func(data *store) error { pruneAuthorizationHistory(data, 0); return nil })
}
func revocationResolutionError(value RevocationResolution) error {
	if value.State == "abandoned" {
		return errors.New("acknowledgement of this historical revocation was explicitly abandoned; no live drain is asserted")
	}
	if !resolvedRevocation(value) {
		return errors.New("revocation is saved but captured daemon cancellation remains unacknowledged")
	}
	return nil
}
func revocationDaemonPath(path, id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", errors.New("invalid captured daemon identity")
	}
	return filepath.Join(authorizationDaemonDir(path), id+".json"), nil
}

func validateRevocationResolutions(data *store) error {
	for operation, resolution := range data.RevocationResolutions {
		if _, found := data.Revocations[operation]; !found || resolution.RecordedAt.IsZero() || len(resolution.Daemons) > maxAuthorizationDaemons {
			return errors.New("invalid revocation resolution metadata")
		}
		seen := map[string]bool{}
		all := len(resolution.Daemons) > 0
		for _, daemon := range resolution.Daemons {
			if _, err := uuid.Parse(daemon.InstanceID); err != nil || seen[daemon.InstanceID] || len(daemon.Error) > 256 || daemon.Acknowledged && daemon.Error != "" {
				return errors.New("invalid revocation daemon metadata")
			}
			seen[daemon.InstanceID] = true
			all = all && daemon.Acknowledged
		}
		if !resolution.Captured && len(resolution.Daemons) != 0 {
			return errors.New("revocation daemon metadata lacks its capture")
		}
		switch resolution.State {
		case "pending":
			if !resolution.Captured || len(resolution.Daemons) == 0 || all {
				return errors.New("invalid pending revocation metadata")
			}
		case "acknowledged":
			if !resolution.Captured || !all {
				return errors.New("invalid revocation acknowledgement metadata")
			}
		case "no-live-daemons-observed":
			if !resolution.Captured || len(resolution.Daemons) != 0 {
				return errors.New("invalid empty revocation daemon capture")
			}
		case "abandoned":
		default:
			return errors.New("unknown revocation resolution state")
		}
	}
	return nil
}
