// Modified by the Crux project for in-repository integration.
package foundation

import (
	"context"
)

// Provider represents a provider of language models.
type Provider interface {
	Name() string
	LanguageModel(ctx context.Context, modelID string) (LanguageModel, error)
}
