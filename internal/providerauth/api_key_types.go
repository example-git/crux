package providerauth

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/config"
)

// APIKeyCheckRequest contains secret input. It may cross the authenticated
// owner RPC boundary, but formatted diagnostics must never print its Source.
// CheckID is created once for an explicit check and reused for transport retry.
type APIKeyCheckRequest struct {
	CheckID      string `json:"check_id"`
	Target       Target `json:"target"`
	CredentialID string `json:"credential_id"`
	Source       string `json:"source"`
}

func (APIKeyCheckRequest) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private API key check request]"))
}

func (r APIKeyCheckRequest) Validate() error {
	if !validOperationID(r.CheckID) || !validText(r.CredentialID, 128, true) ||
		len(r.Source) > 64*1024 || !utf8.ValidString(r.Source) ||
		strings.ContainsRune(r.Source, '\x00') || strings.TrimSpace(r.Source) == "" {
		return errors.New("invalid API key check request")
	}
	return r.Target.Validate()
}

// CheckedTarget is present only after the preparation and its observed probe
// policy completed successfully against the exact initiating configuration.
// Probe describes that policy's evidence; it does not assert inference access.
// A retained check is historical. Save must still verify its target and capture.
type APIKeyCheckOutcome struct {
	CheckID       string                       `json:"check_id"`
	Previous      Target                       `json:"previous"`
	CredentialID  string                       `json:"credential_id"`
	Probe         config.ConnectionProbeResult `json:"probe"`
	CheckedTarget *Target                      `json:"checked_target,omitempty"`
}

func (o APIKeyCheckOutcome) Validate() error {
	if !validOperationID(o.CheckID) || !validText(o.CredentialID, 128, true) {
		return errors.New("invalid API key check outcome")
	}
	if err := o.Previous.Validate(); err != nil {
		return err
	}
	if err := o.Probe.Validate(); err != nil {
		return err
	}
	if o.CheckedTarget != nil {
		current := *o.CheckedTarget
		if err := current.Validate(); err != nil {
			return err
		}
		if current.WorkspaceID != o.Previous.WorkspaceID || current.Owner != o.Previous.Owner ||
			current.Generation.Epoch != o.Previous.Generation.Epoch || current.Generation.Sequence <= o.Previous.Generation.Sequence {
			return errors.New("API key check has inconsistent ownership or generation")
		}
		if o.Probe.Kind == config.ConnectionProbeUnsupported || o.Probe.Kind == config.ConnectionProbeHTTPAttempt {
			return errors.New("API key check has no successful probe policy result")
		}
		if o.Probe.Kind == config.ConnectionProbeNotProbed && o.Probe.Policy != config.ConnectionProbePolicyNone ||
			o.Probe.Kind == config.ConnectionProbeHTTPResponse && (o.Probe.Policy == config.ConnectionProbePolicyHTTP200 && o.Probe.HTTPStatus != 200 ||
				o.Probe.Policy == config.ConnectionProbePolicyNon401 && o.Probe.HTTPStatus == 401) {
			return errors.New("API key check did not satisfy its reported probe policy")
		}
	}
	return nil
}

// APIKeySaveRequest deliberately has no source field. Save can only commit the
// private preparation retained by this owner's exact CheckID/CheckedTarget.
type APIKeySaveRequest struct {
	OperationID string `json:"operation_id"`
	Target      Target `json:"target"`
	CheckID     string `json:"check_id"`
}

func (r APIKeySaveRequest) Validate() error {
	if !validOperationID(r.OperationID) || !validOperationID(r.CheckID) {
		return errors.New("invalid checked API key save request")
	}
	return r.Target.Validate()
}

func (o MutationOutcome) ValidateAPIKeySave(request APIKeySaveRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return o.validateRequest(mutationRequest{operationID: request.OperationID, target: request.Target, checkID: request.CheckID})
}

var (
	ErrAPIKeyCheck            = errors.New("provider API key check did not complete")
	ErrAPIKeyCheckUnavailable = errors.New("checked API key receipt is unavailable; check the key again")
)
