package openairesponses

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ContinuationRequest contains only the generic state needed to decide whether
// previous_response_id can safely replace full-history replay.
type ContinuationRequest struct {
	Stable json.RawMessage
	Input  []json.RawMessage
	Store  bool
}

// ContinuationPlan is the request history selected for one operation.
type ContinuationPlan struct {
	PreviousResponseID string
	Input              []json.RawMessage
	Incremental        bool
	FallbackReason     string
}

// ContinuationState tracks one public previous-response chain. Callers scope a
// state to their provider, account, model, and conversation identities.
type ContinuationState struct {
	responseID  string
	stable      json.RawMessage
	represented []json.RawMessage
}

func (s *ContinuationState) Plan(request ContinuationRequest) (ContinuationPlan, error) {
	input, err := cloneJSONValues(request.Input)
	if err != nil {
		return ContinuationPlan{}, err
	}
	full := ContinuationPlan{Input: input}
	if !request.Store {
		full.FallbackReason = "storage_disabled"
		return full, nil
	}
	if s.responseID == "" {
		full.FallbackReason = "no_previous_response"
		return full, nil
	}
	equal, err := equalJSON(request.Stable, s.stable)
	if err != nil {
		return ContinuationPlan{}, fmt.Errorf("compare stable Responses fields: %w", err)
	}
	if !equal {
		s.Reset()
		full.FallbackReason = "request_properties_changed"
		return full, nil
	}
	prefix, err := hasJSONPrefix(request.Input, s.represented)
	if err != nil {
		return ContinuationPlan{}, err
	}
	if !prefix {
		s.Reset()
		full.FallbackReason = "history_not_append_only"
		return full, nil
	}
	return ContinuationPlan{
		PreviousResponseID: s.responseID,
		Input:              input[len(s.represented):],
		Incremental:        true,
	}, nil
}

// Commit records the request input plus response output represented by a public
// stored response. Empty IDs or malformed values clear the chain.
func (s *ContinuationState) Commit(request ContinuationRequest, responseID string, output []json.RawMessage) error {
	if !request.Store || responseID == "" {
		s.Reset()
		return nil
	}
	stable, err := cloneJSON(request.Stable)
	if err != nil {
		s.Reset()
		return fmt.Errorf("store stable Responses fields: %w", err)
	}
	input, err := cloneJSONValues(request.Input)
	if err != nil {
		s.Reset()
		return err
	}
	result, err := cloneJSONValues(output)
	if err != nil {
		s.Reset()
		return err
	}
	s.responseID = responseID
	s.stable = stable
	s.represented = append(input, result...)
	return nil
}

func (s *ContinuationState) Reset() {
	s.responseID = ""
	s.stable = nil
	s.represented = nil
}

func cloneJSON(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		return nil, nil
	}
	if !json.Valid(value) {
		return nil, fmt.Errorf("invalid JSON value")
	}
	return bytes.Clone(value), nil
}

func cloneJSONValues(values []json.RawMessage) ([]json.RawMessage, error) {
	if values == nil {
		return nil, nil
	}
	result := make([]json.RawMessage, len(values))
	for i, value := range values {
		clone, err := cloneJSON(value)
		if err != nil {
			return nil, fmt.Errorf("Responses history item %d: %w", i, err)
		}
		result[i] = clone
	}
	return result, nil
}

func hasJSONPrefix(values, prefix []json.RawMessage) (bool, error) {
	if len(values) < len(prefix) {
		return false, nil
	}
	for i := range prefix {
		equal, err := equalJSON(values[i], prefix[i])
		if err != nil || !equal {
			return false, err
		}
	}
	return true, nil
}

func equalJSON(a, b json.RawMessage) (bool, error) {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b), nil
	}
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &right); err != nil {
		return false, err
	}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON), nil
}
