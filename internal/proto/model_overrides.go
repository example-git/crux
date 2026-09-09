package proto

import "github.com/example-git/crux/internal/config"

// ModelOverridesRequest replaces the supplied model slots for this workspace
// only. Owners bind each selection to the initiating provider generation.
type ModelOverridesRequest struct {
	State config.AgentModelState `json:"state"`
}
