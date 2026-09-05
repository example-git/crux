package providertransport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

type OwnerValidator func() error

type OwnerValidationError struct {
	Err error
}

func (err *OwnerValidationError) Error() string {
	return err.Err.Error()
}

func (err *OwnerValidationError) Unwrap() error {
	return err.Err
}

func IsOwnerValidationError(err error) bool {
	var ownerError *OwnerValidationError
	return errors.As(err, &ownerError)
}

type ownerValidatorContextKey struct{}

func ContextWithOwnerValidator(ctx context.Context, validate OwnerValidator) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ownerValidatorContextKey{}, validate)
}

func OwnerValidatorFromContext(ctx context.Context) OwnerValidator {
	if ctx == nil {
		return nil
	}
	validate, _ := ctx.Value(ownerValidatorContextKey{}).(OwnerValidator)
	return validate
}

func ValidateContextOwner(ctx context.Context) error {
	if validate := OwnerValidatorFromContext(ctx); validate != nil {
		return validate()
	}
	return nil
}

func ClientWithContextOwnerValidator(ctx context.Context, client *http.Client) *http.Client {
	if validate := OwnerValidatorFromContext(ctx); validate != nil {
		return ClientWithOwnerValidator(client, validate)
	}
	if client == nil {
		return http.DefaultClient
	}
	return client
}

func OpenURLWithContextOwnerValidator(ctx context.Context, open func(string) error, rawURL string) error {
	if err := ValidateContextOwner(ctx); err != nil {
		return err
	}
	if open == nil {
		return fmt.Errorf("authorization URL opener is unavailable")
	}
	return open(rawURL)
}

type ownerValidatingTransport struct {
	base     http.RoundTripper
	validate OwnerValidator
}

func (transport ownerValidatingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.validate == nil {
		return nil, &OwnerValidationError{Err: fmt.Errorf("provider owner validator is unavailable")}
	}
	if err := transport.validate(); err != nil {
		return nil, &OwnerValidationError{Err: err}
	}
	base := transport.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(request)
}

func OwnerValidatingTransport(base http.RoundTripper, validate OwnerValidator) http.RoundTripper {
	return ownerValidatingTransport{base: base, validate: validate}
}

func ClientWithOwnerValidator(client *http.Client, validate OwnerValidator) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.Transport = OwnerValidatingTransport(client.Transport, validate)
	return &clone
}
