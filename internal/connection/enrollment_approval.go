package connection

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// EnrollmentCandidate is copied into the approver. Its fields contain public
// identity metadata only. Changing the copy cannot change the retained name,
// certificate, endpoint, expiry, server identity or authorization-store path.
// An empty Endpoint and zero ExpiresAt identify a local manual authorization.
type EnrollmentCandidate struct {
	ClientName        string
	ClientFingerprint string
	ServerFingerprint string
	Endpoint          string
	ExpiresAt         time.Time
}

// EnrollmentApprover must return nil only after explicit approval and must honor
// context cancellation. A missing approver is never implicit permission.
type EnrollmentApprover func(context.Context, EnrollmentCandidate) error

var ErrEnrollmentApprovalDenied = errors.New("enrollment approval denied")

func (e *EnrollmentListener) approveReserved(ctx context.Context, candidate EnrollmentCandidate, ready func(time.Time) error) error {
	if err := approvalContextError(ctx, candidate.ExpiresAt); err != nil {
		return err
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if ready != nil {
		if err := ready(candidate.ExpiresAt); err != nil {
			return errors.New("enrollment approval response deadline is unavailable")
		}
	}
	if e.approve == nil {
		return errors.New("enrollment requires an explicit approver")
	}
	if err := e.approve(ctx, candidate); err != nil {
		if contextErr := approvalContextError(ctx, candidate.ExpiresAt); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, ErrEnrollmentApprovalDenied) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrEnrollmentApprovalDenied, err)
	}
	return approvalContextError(ctx, candidate.ExpiresAt)
}

func approvalContextError(ctx context.Context, expiry time.Time) error {
	if !expiry.IsZero() && !time.Now().Before(expiry) {
		return enrollmentExpiredError()
	}
	return ctx.Err()
}

// AuthorizeClientWithApproval captures one initialized local server and store
// path before displaying the candidate. Approval cannot be rebound by a later
// environment change or replacement server identity.
func AuthorizeClientWithApproval(ctx context.Context, name, clientCertificate string, approve EnrollmentApprover) error {
	if approve == nil {
		return errors.New("client authorization requires an explicit approver")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name, clientCertificate = strings.TrimSpace(name), strings.TrimSpace(clientCertificate)
	if name == "" || len(name) > 128 || strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errors.New("invalid client name")
	}
	clientCert, err := parseCertificate(clientCertificate, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return errors.New("invalid client pairing code")
	}
	path, err := filepath.Abs(storePath())
	if err != nil {
		return err
	}
	captured, err := loadStoreAt(ctx, path)
	if err != nil {
		return err
	}
	if captured.Server == nil {
		return errors.New("server identity is not initialized; run crux connections server-init")
	}
	identity := *captured.Server
	serverCert, _, err := parseIdentity(identity, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return err
	}
	candidate := EnrollmentCandidate{
		ClientName:        name,
		ClientFingerprint: certificateFingerprint(clientCert),
		ServerFingerprint: certificateFingerprint(serverCert),
	}
	if err := approve(ctx, candidate); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrEnrollmentApprovalDenied) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrEnrollmentApprovalDenied, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return authorizeClientAt(ctx, path, &identity, name, clientCertificate, func(persist func() error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return persist()
	})
}
