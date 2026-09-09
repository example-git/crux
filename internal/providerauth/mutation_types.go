package providerauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/config"
)

// Operation IDs are generated once per explicit user action and reused for
// transport retries. The service retains the last 128 admitted attempts, not a
// restart-persistent or lifetime-wide idempotency history. Never automatically
// replay a missed receipt with a fresh target or operation ID.
type SwitchRequest struct {
	OperationID string `json:"operation_id"`
	Target      Target `json:"target"`
	AccountID   string `json:"account_id"`
}

type LogoutRequest struct {
	OperationID string `json:"operation_id"`
	Target      Target `json:"target"`
}

type MutationProgress struct {
	AccountRefreshed bool `json:"account_refreshed"`
	AccountsSaved    bool `json:"accounts_saved"`
	ConfigSaved      bool `json:"config_saved"`
	RuntimePublished bool `json:"runtime_published"`
}

// Model owners use the public reference: config.AgentModelState contains a
// private account namespace and must never be embedded in an auth response.
// An unresolved existing selection has no Owner; authentication does not repair
// or replace it. Empty model state remains valid for unconfigured workspaces.
type OwnedModelState struct {
	Model config.SelectedModel `json:"model"`
	Owner *Owner               `json:"owner,omitempty"`
}

type ModelState struct {
	Large *OwnedModelState `json:"large,omitempty"`
	Small *OwnedModelState `json:"small,omitempty"`
}

type Change struct {
	OperationID string        `json:"operation_id"`
	Previous    Target        `json:"previous"`
	Current     AccountsState `json:"current"`
	Models      ModelState    `json:"models"`
}

// Progress describes established local durable effects even when the operation
// returns an error. False flags on an admission error do not prove that a lost
// or evicted earlier operation never ran. Change exists only for a complete
// coherent local transaction. A
// superseded Change is historical evidence and cannot authorize runtime/cache
// adoption. Owning clients still require their separate remote acknowledgement.
type MutationOutcome struct {
	OperationID string           `json:"operation_id"`
	Previous    Target           `json:"previous"`
	Progress    MutationProgress `json:"progress"`
	Change      *Change          `json:"change,omitempty"`
	Superseded  bool             `json:"superseded"`
}

// MutationResult retains private runtime authority. Serialize Outcome explicitly
// at transport boundaries; never serialize the full result or a Config snapshot.
type MutationResult struct {
	Outcome MutationOutcome
	runtime config.RuntimeSnapshot
	after   config.AuthenticationCapture
	current bool
}

func (MutationResult) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication mutation results are private")
}

func (MutationResult) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication mutation result]"))
}

// RuntimeSnapshot is available only after this service verified the exact
// successful receipt still matched current local state. It is not a remote
// acknowledgement or permission to adopt a subsequently superseded generation.
func (r MutationResult) RuntimeSnapshot() (config.RuntimeSnapshot, bool) {
	if !r.hasCurrentReceipt() {
		return config.RuntimeSnapshot{}, false
	}
	return r.runtime, true
}

// AuthenticationCapture returns only the exact successful transaction's retained
// observation, with the same point-in-time authority as RuntimeSnapshot. A later
// collector must revalidate this capture before constructing or publishing a
// remote runtime; it must not replace it with a fresh account observation.
func (r MutationResult) AuthenticationCapture() (config.AuthenticationCapture, bool) {
	if !r.hasCurrentReceipt() {
		return config.AuthenticationCapture{}, false
	}
	return r.after, true
}

func (r MutationResult) hasCurrentReceipt() bool {
	return r.current && r.Outcome.Change != nil && r.Outcome.Progress.RuntimePublished && !r.Outcome.Superseded
}

var (
	ErrOperationConflict = errors.New("authentication operation ID was already used for a different request")
	ErrAccount           = errors.New("authentication account is unavailable; reload the account list")
	ErrMutation          = errors.New("provider authentication change did not complete")
	ErrReceiptUnverified = errors.New("authentication change receipt could not be verified against current state")
)

func validOperationID(id string) bool {
	return len(id) == 32 && strings.IndexFunc(id, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) < 0
}

func (r SwitchRequest) Validate() error {
	if !validOperationID(r.OperationID) || !validText(r.AccountID, 4096, true) {
		return errors.New("invalid authentication switch request")
	}
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if !r.Target.Owner.HasOAuth {
		return errors.New("authentication switch requires an OAuth owner")
	}
	return nil
}

func (r LogoutRequest) Validate() error {
	if !validOperationID(r.OperationID) {
		return errors.New("invalid authentication logout request")
	}
	return r.Target.Validate()
}

func (s ModelState) Validate() error {
	for _, selected := range []*OwnedModelState{s.Large, s.Small} {
		if selected == nil {
			continue
		}
		if selected.Owner != nil {
			if err := selected.Owner.Validate(); err != nil {
				return err
			}
			if selected.Owner.ProviderID != selected.Model.Provider {
				return errors.New("authentication model owner does not match its provider")
			}
		}
	}
	if _, err := json.Marshal(s); err != nil {
		return errors.New("authentication model state cannot be encoded")
	}
	return nil
}

func (c Change) Validate() error {
	if !validOperationID(c.OperationID) {
		return errors.New("invalid authentication change operation")
	}
	if err := c.Previous.Validate(); err != nil {
		return err
	}
	if err := c.Current.Validate(); err != nil {
		return err
	}
	if c.Current.Target.WorkspaceID != c.Previous.WorkspaceID || c.Current.Target.Owner != c.Previous.Owner || c.Current.Target.Generation.Epoch != c.Previous.Generation.Epoch || c.Current.Target.Generation.Sequence <= c.Previous.Generation.Sequence {
		return errors.New("authentication change has inconsistent ownership or generation")
	}
	return c.Models.Validate()
}

func (o MutationOutcome) Validate() error {
	if !validOperationID(o.OperationID) {
		return errors.New("invalid authentication outcome operation")
	}
	if err := o.Previous.Validate(); err != nil {
		return err
	}
	if o.Change == nil {
		if o.Superseded {
			return errors.New("authentication outcome has no superseded receipt")
		}
		return nil // Partial publication may have no coherent post-observation.
	}
	if !o.Progress.RuntimePublished {
		return errors.New("authentication change has no completed local publication")
	}
	if o.Change.OperationID != o.OperationID || o.Change.Previous != o.Previous {
		return errors.New("authentication outcome and change refer to different operations")
	}
	return o.Change.Validate()
}

func (o MutationOutcome) ValidateSwitch(request SwitchRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return o.validateRequest(mutationRequest{operationID: request.OperationID, target: request.Target, accountID: request.AccountID})
}

func (o MutationOutcome) ValidateLogout(request LogoutRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return o.validateRequest(mutationRequest{operationID: request.OperationID, target: request.Target, logout: true})
}

func (o MutationOutcome) validateRequest(request mutationRequest) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.OperationID != request.operationID || o.Previous != request.target {
		return errors.New("authentication outcome does not match the requested operation")
	}
	if o.Change == nil {
		return nil
	}
	return validateMutationEffect(request, o)
}
