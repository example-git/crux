package providerauth

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/example-git/crux/internal/config"
)

// LocalRepairRequest names historical disk work independently of the current
// workspace incarnation. Review never applies; Apply requires its exact revision.
type LocalRepairRequest struct {
	WorkspaceID          string `json:"workspace_id"`
	OperationWorkspaceID string `json:"operation_workspace_id"`
	OperationID          string `json:"operation_id"`
	Revision             uint64 `json:"revision"`
	Apply                bool   `json:"apply"`
}

func (r LocalRepairRequest) Validate() error {
	for _, value := range []string{r.WorkspaceID, r.OperationWorkspaceID, r.OperationID} {
		if !validText(value, 512, true) || strings.ContainsAny(value, "/\\") {
			return errors.New("invalid local authentication repair identity")
		}
	}
	if r.Apply && r.Revision == 0 || !r.Apply && r.Revision != 0 {
		return errors.New("review has no revision; apply requires its reviewed revision")
	}
	return nil
}
func (s *Service) RepairLocalAuthentication(ctx context.Context, request LocalRepairRequest) (config.LocalAuthenticationRepairResult, error) {
	if err := request.Validate(); err != nil {
		return config.LocalAuthenticationRepairResult{}, err
	}
	if request.WorkspaceID != s.workspaceID {
		return config.LocalAuthenticationRepairResult{}, ErrStale
	}
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := s.acquire(ctx); err != nil {
		return config.LocalAuthenticationRepairResult{}, err
	}
	defer func() { <-s.gate }()
	if s.store == nil {
		return config.LocalAuthenticationRepairResult{}, errors.New("local authentication store is unavailable")
	}
	key := config.AuthenticationJournalKey{Kind: config.AuthenticationJournalLocal, WorkspaceID: request.OperationWorkspaceID, OperationID: request.OperationID}
	capture, found, err := s.store.LoadAuthenticationLocalChange(ctx, key)
	if err != nil {
		return config.LocalAuthenticationRepairResult{}, err
	}
	if !found {
		return config.LocalAuthenticationRepairResult{}, errors.New("local authentication operation is unavailable")
	}
	if err := s.store.CheckLocalAuthenticationRepairScope(ctx, capture); err != nil {
		return config.LocalAuthenticationRepairResult{}, err
	}
	result := config.LocalAuthenticationRepairResult{Summary: capture.Summary()}
	if err := result.Summary.Validate(); err != nil {
		return config.LocalAuthenticationRepairResult{}, err
	}
	if !request.Apply {
		return result, ctx.Err()
	}
	if s.sequence == math.MaxUint64 {
		return result, errors.New("authentication generation exhausted; reopen workspace")
	}
	result, err = s.store.RepairAuthenticationLocalChange(ctx, capture, request.Revision)
	// Any actual repair invalidates admission based on the old account/config
	// observation. It does not produce a new runtime or mutation acknowledgement.
	if result.AccountsWritten || result.ConfigWritten {
		s.sequence++
		s.last = config.AuthenticationCapture{}
	}
	return result, err
}

// WorkspaceID is the immutable service incarnation identity, requiring no
// account/config read. Historical repair uses it even after partial disk writes.
func (s *Service) WorkspaceID() string {
	if s == nil {
		return ""
	}
	return s.workspaceID
}
