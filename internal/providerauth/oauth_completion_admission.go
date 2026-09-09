package providerauth

import "context"

type oauthCompletionAdmissionKey struct{}
type oauthCompletionAdmission func(context.Context) (func(), error)

// WithOAuthCompletionAdmission lets the owning Workspace durably record an
// intent only after the service's normal capture/owner/generation preflight.
// The returned release belongs to the actual commit worker, not its HTTP caller.
// This private context hook conveys no credential or alternate authorization.
func WithOAuthCompletionAdmission(ctx context.Context, admit func(context.Context) (func(), error)) context.Context {
	return context.WithValue(ctx, oauthCompletionAdmissionKey{}, oauthCompletionAdmission(admit))
}

func admitOAuthCompletion(ctx context.Context) (func(), error) {
	if admit, ok := ctx.Value(oauthCompletionAdmissionKey{}).(oauthCompletionAdmission); ok && admit != nil {
		return admit(ctx)
	}
	return func() {}, nil
}
