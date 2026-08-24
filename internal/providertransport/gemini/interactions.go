package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// InteractionRequest separates stable request properties from the new input so
// previous_interaction_id can be applied without replaying stored history.
type InteractionRequest struct {
	Stable json.RawMessage
	Input  json.RawMessage
	Store  bool
}

type InteractionPlan struct {
	PreviousInteractionID string
	Input                 json.RawMessage
	Continued             bool
	FallbackReason        string
}

type InteractionState struct {
	interactionID string
	stable        json.RawMessage
}

func (s *InteractionState) Plan(request InteractionRequest) (InteractionPlan, error) {
	input, err := validClone(request.Input)
	if err != nil {
		return InteractionPlan{}, fmt.Errorf("Gemini interaction input: %w", err)
	}
	plan := InteractionPlan{Input: input}
	if !request.Store {
		plan.FallbackReason = "storage_disabled"
		return plan, nil
	}
	if s.interactionID == "" {
		plan.FallbackReason = "no_previous_interaction"
		return plan, nil
	}
	equal, err := semanticallyEqual(request.Stable, s.stable)
	if err != nil {
		return InteractionPlan{}, err
	}
	if !equal {
		s.Reset()
		plan.FallbackReason = "request_properties_changed"
		return plan, nil
	}
	plan.PreviousInteractionID = s.interactionID
	plan.Continued = true
	return plan, nil
}

func (s *InteractionState) Commit(request InteractionRequest, interactionID string) error {
	if !request.Store || interactionID == "" {
		s.Reset()
		return nil
	}
	stable, err := validClone(request.Stable)
	if err != nil {
		s.Reset()
		return fmt.Errorf("Gemini interaction stable fields: %w", err)
	}
	s.interactionID = interactionID
	s.stable = stable
	return nil
}

func (s *InteractionState) Reset() {
	s.interactionID = ""
	s.stable = nil
}

// ApplyPreviousInteraction adds the documented continuation field to a JSON
// interaction request without interpreting provider-specific steps.
func ApplyPreviousInteraction(request json.RawMessage, interactionID string) (json.RawMessage, error) {
	if !json.Valid(request) {
		return nil, fmt.Errorf("Gemini interaction request is not valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(request, &object); err != nil || object == nil {
		return nil, fmt.Errorf("Gemini interaction request must be a JSON object")
	}
	if interactionID == "" {
		delete(object, "previous_interaction_id")
	} else {
		encoded, _ := json.Marshal(interactionID)
		object["previous_interaction_id"] = encoded
	}
	result, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode Gemini interaction request: %w", err)
	}
	return result, nil
}

func validClone(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		return nil, nil
	}
	if !json.Valid(value) {
		return nil, fmt.Errorf("invalid JSON value")
	}
	return bytes.Clone(value), nil
}

func semanticallyEqual(a, b json.RawMessage) (bool, error) {
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
