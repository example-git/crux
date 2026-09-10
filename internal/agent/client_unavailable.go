package agent

import (
	"context"

	fantasy "github.com/example-git/crux/foundation"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
)

// An explicit client logout/disable is publishable, but it cannot dispatch
// inference. All supported model entry points return the accepted reason.
type unavailableClientProvider struct {
	id  string
	err error
}

func (p unavailableClientProvider) Name() string              { return p.id }
func (p unavailableClientProvider) continuationOwner() string { return "unavailable:" + p.id }
func (p unavailableClientProvider) LanguageModel(_ context.Context, id string) (fantasy.LanguageModel, error) {
	return unavailableClientModel{provider: p.id, model: id, err: p.err}, nil
}

type unavailableClientModel struct {
	provider, model string
	err             error
}

func (m unavailableClientModel) Provider() string { return m.provider }
func (m unavailableClientModel) Model() string    { return m.model }
func (m unavailableClientModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, m.err
}

func (m unavailableClientModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, m.err
}

func (m unavailableClientModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, m.err
}

func (m unavailableClientModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, m.err
}

func (m unavailableClientModel) Compact(context.Context, fantasy.Call) (*codexresponses.CompactionResult, error) {
	return nil, m.err
}
