package providerdiagnostics

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
)

type Runtime interface {
	RuntimeSnapshot() config.RuntimeSnapshot
	ValidateActiveProviderOwner(providerregistry.RegistrationOwner) error
}

type Check string

const (
	CheckAccount    Check = "account"
	CheckUsage      Check = "usage"
	CheckConnection Check = "connection"
)

type Request struct {
	ProviderID string  `json:"provider_id"`
	AccountID  string  `json:"-"`
	Checks     []Check `json:"checks,omitempty"`
}

type Status string

const (
	StatusPassed     Status = "passed"
	StatusFailed     Status = "failed"
	StatusNotReached Status = "not-reached"
)

type AccountResult struct {
	Loaded bool   `json:"loaded"`
	Source string `json:"source,omitempty"`
}

type CheckResult struct {
	Check   Check  `json:"check"`
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
}

type OperationResult struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Status     Status `json:"status"`
	HTTPStatus int    `json:"http_status,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Message    string `json:"message,omitempty"`
}

type Report struct {
	Valid      bool              `json:"valid"`
	ProviderID string            `json:"provider_id"`
	OwnerType  string            `json:"owner_type"`
	Account    AccountResult     `json:"account"`
	Checks     []CheckResult     `json:"checks"`
	Operations []OperationResult `json:"operations,omitempty"`
}

type diagnosticRuntime struct {
	runtime      Runtime
	snapshot     config.RuntimeSnapshot
	provider     config.ProviderConfig
	owner        providerregistry.RegistrationOwner
	registration providerregistry.Registration
	registered   bool
}

type operationRecorder struct {
	mu     sync.Mutex
	values []providertransport.OperationDiagnostic
}

func (r *operationRecorder) record(value providertransport.OperationDiagnostic) {
	r.mu.Lock()
	r.values = append(r.values, value)
	r.mu.Unlock()
}

func Run(ctx context.Context, runtime Runtime, request Request) (Report, error) {
	if runtime == nil {
		return Report{}, errors.New("provider diagnostic runtime is required")
	}
	if request.ProviderID == "" {
		return Report{}, errors.New("provider ID is required")
	}
	resolved, err := captureRuntime(runtime, request.ProviderID)
	if err != nil {
		return Report{}, err
	}
	report := Report{ProviderID: request.ProviderID, OwnerType: string(resolved.provider.Owner.Type)}
	checks, err := resolveChecks(resolved, request.Checks)
	if err != nil {
		return Report{}, err
	}
	validate := providertransport.OwnerValidator(func() error {
		return runtime.ValidateActiveProviderOwner(resolved.owner)
	})
	if err := validate(); err != nil {
		return Report{}, fmt.Errorf("provider %s exact owner changed before diagnostics", request.ProviderID)
	}
	ctx = providertransport.ContextWithOwnerValidator(ctx, validate)

	if resolved.provider.Owner.Type == config.ProviderOwnerPreset {
		started := time.Now()
		err := resolved.provider.TestConnection(ctx, snapshotResolver{snapshot: resolved.snapshot}, validate)
		report.Checks = append(report.Checks, resultForError(CheckConnection, err, "preset API key and connection are valid", "preset API key or connection validation failed"))
		report.Operations = append(report.Operations, OperationResult{
			Path: "/connection", Kind: "connection", Status: statusForError(err), DurationMS: durationMilliseconds(time.Since(started)), Message: messageForError(err, "connection succeeded", "connection failed"),
		})
		report.Valid = err == nil
		return report, nil
	}

	entry, source, err := loadAccount(ctx, resolved, request.AccountID, validate)
	if err != nil || entry == nil || entry.AccessToken == "" {
		report.Account = AccountResult{}
		report.Checks = append(report.Checks, CheckResult{Check: CheckAccount, Status: StatusFailed, Message: "authenticated account could not be loaded"})
		for _, check := range checks {
			if check != CheckAccount {
				report.Checks = append(report.Checks, CheckResult{Check: check, Status: StatusNotReached, Message: "authenticated account was unavailable"})
			}
		}
		report.Operations = operationResults(resolved.registration, checks, nil)
		return report, nil
	}
	report.Account = AccountResult{Loaded: true, Source: source}
	recorder := &operationRecorder{}
	ctx = providertransport.ContextWithOperationDiagnostics(ctx, recorder.record)

	for _, check := range checks {
		switch check {
		case CheckAccount:
			err = runAccountCheck(ctx, resolved, entry.AccessToken)
			report.Checks = append(report.Checks, resultForError(check, err, "authenticated account loaded", "authenticated account identity failed"))
		case CheckUsage:
			_, err = oauthusage.FetchWithTokenForOwner(ctx, resolved.provider.ID, entry.AccessToken, resolved.registration.Quota, func() error { return validate() })
			report.Checks = append(report.Checks, resultForError(check, err, "usage operations completed", "usage operations failed"))
		}
	}
	if err := validate(); err != nil {
		report.Checks = append(report.Checks, CheckResult{Check: CheckAccount, Status: StatusFailed, Message: "exact provider owner changed during diagnostics"})
	}
	report.Operations = operationResults(resolved.registration, checks, recorder.values)
	report.Valid = checksPassed(report.Checks) && operationsPassed(report.Operations)
	return report, nil
}

func captureRuntime(runtime Runtime, providerID string) (diagnosticRuntime, error) {
	snapshot := runtime.RuntimeSnapshot()
	cfg := snapshot.Config()
	if cfg == nil || cfg.Providers == nil {
		return diagnosticRuntime{}, fmt.Errorf("provider %s is not configured", providerID)
	}
	provider, ok := cfg.Providers.Get(providerID)
	if !ok {
		return diagnosticRuntime{}, fmt.Errorf("provider %s is not configured", providerID)
	}
	provider, registration, registered, err := snapshot.ProviderForConstruction(providerID, provider)
	if err != nil {
		return diagnosticRuntime{}, err
	}
	owner, ok := snapshot.ProviderOwnerFor(providerID, provider)
	if !ok {
		return diagnosticRuntime{}, fmt.Errorf("provider %s exact owner is unavailable", providerID)
	}
	if provider.Owner.Type != config.ProviderOwnerPlugin && provider.Owner.Type != config.ProviderOwnerPreset {
		return diagnosticRuntime{}, fmt.Errorf("provider %s is not owned by a provider plugin or preset", providerID)
	}
	if provider.Owner.Type == config.ProviderOwnerPlugin && (!registered || registration.Manifest == nil) {
		return diagnosticRuntime{}, fmt.Errorf("provider %s exact plugin registration is unavailable", providerID)
	}
	return diagnosticRuntime{runtime: runtime, snapshot: snapshot, provider: provider, owner: owner, registration: registration, registered: registered}, nil
}

func resolveChecks(runtime diagnosticRuntime, requested []Check) ([]Check, error) {
	if len(requested) == 0 {
		if runtime.provider.Owner.Type == config.ProviderOwnerPreset {
			return []Check{CheckConnection}, nil
		}
		var checks []Check
		if runtime.registration.AccountNamespace != "" {
			checks = append(checks, CheckAccount)
		}
		if runtime.registration.Quota != nil {
			checks = append(checks, CheckUsage)
		}
		if len(checks) == 0 {
			return nil, fmt.Errorf("provider %s declares no live diagnostic checks", runtime.provider.ID)
		}
		return checks, nil
	}
	checks := make([]Check, 0, len(requested))
	for _, check := range requested {
		if slices.Contains(checks, check) {
			continue
		}
		switch check {
		case CheckConnection:
			if runtime.provider.Owner.Type != config.ProviderOwnerPreset {
				return nil, fmt.Errorf("connection check is available only for provider presets")
			}
		case CheckAccount:
			if runtime.provider.Owner.Type != config.ProviderOwnerPlugin || runtime.registration.AccountNamespace == "" {
				return nil, fmt.Errorf("account check is unavailable for provider %s", runtime.provider.ID)
			}
		case CheckUsage:
			if runtime.provider.Owner.Type != config.ProviderOwnerPlugin || runtime.registration.Quota == nil {
				return nil, fmt.Errorf("usage check is unavailable for provider %s", runtime.provider.ID)
			}
		default:
			return nil, fmt.Errorf("unknown provider diagnostic check %q", check)
		}
		checks = append(checks, check)
	}
	return checks, nil
}

func loadAccount(ctx context.Context, runtime diagnosticRuntime, accountID string, validate providertransport.OwnerValidator) (*accounts.Entry, string, error) {
	if entry, ok := runtime.snapshot.EphemeralAccount(runtime.owner); ok && entry != nil && (accountID == "" || entry.ID == accountID) {
		if entry.Expired() {
			return nil, "", errors.New("forwarded account credential is expired")
		}
		if err := validate(); err != nil {
			return nil, "", err
		}
		return entry, "forwarded", nil
	}
	var entry *accounts.Entry
	var err error
	if accountID == "" {
		entry, err = accounts.Active(ctx, runtime.registration.AccountNamespace)
	} else {
		var entries []accounts.Entry
		entries, err = accounts.List(ctx, runtime.registration.AccountNamespace)
		if err == nil {
			for index := range entries {
				if entries[index].ID == accountID {
					value := entries[index]
					entry = &value
					break
				}
			}
		}
	}
	if err != nil {
		return nil, "", err
	}
	if err := validate(); err != nil {
		return nil, "", err
	}
	if entry == nil && accountID == "" && runtime.provider.OAuthToken != nil && runtime.provider.OAuthToken.AccessToken != "" {
		return &accounts.Entry{AccessToken: runtime.provider.OAuthToken.AccessToken}, "configured", nil
	}
	if entry == nil {
		return nil, "", errors.New("authenticated account not found")
	}
	if entry.Expired() {
		if runtime.registration.OAuth == nil || runtime.registration.OAuth.Refresh == nil {
			return nil, "", errors.New("authenticated account is expired")
		}
		entry, err = accounts.EnsureFreshForOwner(ctx, runtime.registration.AccountNamespace, entry, runtime.registration.OAuth.Refresh, func() error { return validate() })
		if err != nil {
			return nil, "", err
		}
	}
	if err := validate(); err != nil {
		return nil, "", err
	}
	return entry, "stored", nil
}

func runAccountCheck(ctx context.Context, runtime diagnosticRuntime, token string) error {
	if runtime.registration.Identity == nil {
		return nil
	}
	if !delegates(runtime.registration.Manifest, "identity") {
		for index := range runtime.registration.Manifest.Capabilities.Operations {
			declaration := runtime.registration.Manifest.Capabilities.Operations[index]
			if declaration.Kind != "account" {
				continue
			}
			operation := runtime.registration.Operations[declaration.ID]
			_, _, _, err := providertransport.ExecuteAccountIdentity(ctx, operation, runtime.registration.Manifest.Capabilities.Credentials, token)
			return err
		}
	}
	id, _, _ := runtime.registration.Identity(ctx, token)
	if id == "" {
		return errors.New("account identity is unavailable")
	}
	return providertransport.ValidateContextOwner(ctx)
}

func operationResults(registration providerregistry.Registration, checks []Check, recorded []providertransport.OperationDiagnostic) []OperationResult {
	indexes := make(map[string]int, len(registration.Manifest.Capabilities.Operations))
	kinds := make(map[string]string, len(registration.Manifest.Capabilities.Operations))
	for index, operation := range registration.Manifest.Capabilities.Operations {
		indexes[operation.ID] = index
		kinds[operation.ID] = operation.Kind
	}
	results := make([]OperationResult, 0, len(recorded))
	seen := make(map[string]bool, len(recorded))
	for _, value := range recorded {
		index, ok := indexes[value.ID]
		if !ok {
			continue
		}
		status := StatusPassed
		message := "request completed"
		if value.Failed {
			status = StatusFailed
			message = "request failed"
		}
		results = append(results, OperationResult{Path: fmt.Sprintf("/capabilities/operations/%d", index), Kind: value.Kind, Status: status, HTTPStatus: value.StatusCode, DurationMS: durationMilliseconds(value.Duration), Message: message})
		seen[value.ID] = true
	}
	for _, id := range expectedOperations(registration, checks) {
		if seen[id] {
			continue
		}
		results = append(results, OperationResult{Path: fmt.Sprintf("/capabilities/operations/%d", indexes[id]), Kind: kinds[id], Status: StatusNotReached, Message: "request was not reached"})
	}
	return results
}

func expectedOperations(registration providerregistry.Registration, checks []Check) []string {
	var expected []string
	if slices.Contains(checks, CheckAccount) && registration.Identity != nil && !delegates(registration.Manifest, "identity") {
		for _, operation := range registration.Manifest.Capabilities.Operations {
			if operation.Kind == "account" {
				expected = append(expected, operation.ID)
				break
			}
		}
	}
	if slices.Contains(checks, CheckUsage) && registration.Usage != nil && registration.Usage.Source == "operation" && !delegates(registration.Manifest, "usage") {
		for _, setup := range registration.Usage.Setup {
			expected = append(expected, setup.Operation)
		}
		expected = append(expected, registration.Usage.Operation)
	}
	return expected
}

func delegates(value *manifest.Manifest, capability string) bool {
	return value != nil && value.Capabilities.Compatibility != nil && slices.Contains(value.Capabilities.Compatibility.Delegates, capability)
}

func resultForError(check Check, err error, success, failure string) CheckResult {
	return CheckResult{Check: check, Status: statusForError(err), Message: messageForError(err, success, failure)}
}

func statusForError(err error) Status {
	if err != nil {
		return StatusFailed
	}
	return StatusPassed
}

func messageForError(err error, success, failure string) string {
	if err != nil {
		return failure
	}
	return success
}

func durationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	milliseconds := value.Milliseconds()
	if milliseconds == 0 {
		return 1
	}
	return milliseconds
}

func checksPassed(results []CheckResult) bool {
	for _, result := range results {
		if result.Status != StatusPassed {
			return false
		}
	}
	return len(results) > 0
}

func operationsPassed(results []OperationResult) bool {
	for _, result := range results {
		if result.Status != StatusPassed {
			return false
		}
	}
	return true
}

type snapshotResolver struct {
	snapshot config.RuntimeSnapshot
}

func (r snapshotResolver) ResolveValue(value string) (string, error) {
	return r.snapshot.Resolve(value)
}
