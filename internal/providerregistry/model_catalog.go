package providerregistry

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/example-git/crux/internal/providertransport"
)

// ValidateModelCatalogOperation describes the fixed checked-key catalog
// executor. It does not authorize inference or turn an arbitrary operation
// into a catalog probe. Keep activation and checked-input admission identical.
func ValidateModelCatalogOperation(operation *providertransport.Operation) error {
	if operation == nil || operation.Kind != "model-catalog" {
		return fmt.Errorf("model-catalog operation is unavailable")
	}
	if operation.Key.Protocol != "generic-json" || operation.Key.Transport != "http-json" || operation.Streaming != nil {
		return fmt.Errorf("model-catalog operation %q requires generic JSON over HTTP", operation.ID)
	}
	if operation.PromptTransform != nil || operation.RoleMap != nil || operation.ToolCodec != nil || operation.Continuation != nil || operation.Compaction != nil {
		return fmt.Errorf("model-catalog operation %q declares unsupported inference policies", operation.ID)
	}
	if operation.Timeouts != nil && operation.Timeouts.IdleSeconds != 0 {
		return fmt.Errorf("model-catalog operation %q cannot use a streaming idle timeout", operation.ID)
	}
	if operation.Retry.Authentication != "never" || operation.Retry.UnexpectedEOF || operation.Retry.MaxAttempts > 1 && operation.Retry.ReplayRequirement != "idempotent" {
		return fmt.Errorf("model-catalog operation %q requires non-refreshing, explicitly idempotent retries", operation.ID)
	}
	path, err := url.Parse(operation.Path)
	if operation.Method == "" || err != nil || path.IsAbs() || path.Host != "" || path.Fragment != "" || !strings.HasPrefix(operation.Path, "/") || strings.ContainsAny(operation.Path, "{}") || strings.HasPrefix(operation.Path, "//") {
		return fmt.Errorf("model-catalog operation %q requires a literal relative path", operation.ID)
	}
	return nil
}
