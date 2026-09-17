package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const selectedTokenRotationLimit = 64

// selectedTokenCompletionTimeout bounds how long a caller's disconnection can
// be outlived by an in-flight OAuth exchange/commit before its successor is
// retained and the caller sees context.DeadlineExceeded instead of hanging
// forever. It is a var (not a const) solely so tests that must observe this
// real deadline firing can shrink it instead of waiting out the production
// value on every run.
var selectedTokenCompletionTimeout = time.Minute

// A receipt retains the process-local publication state around a private durable
// exchange lineage. Cross-store recovery requires that exact lineage and its
// captured input proof; an arbitrary newer disk token is never a successor.
// writeMu protects the receipt's configuration state; freshMu protects only
// the immutable successor and is never held while waiting for writeMu or I/O.
// The provider refresh lock serializes exchange and commit callers.
type selectedTokenRotation struct {
	freshMu         *sync.RWMutex
	providerID      string
	originalID      string
	environment     []string
	before          *Config
	preimage        authenticationInputFile
	fresh           *oauth.Token
	next            *Config
	postimage       authenticationInputFile
	written         bool
	committed       bool
	exchangeStarted bool
	lineage         *selectedTokenLineage
}

func (r *selectedTokenRotation) token() *oauth.Token {
	r.freshMu.RLock()
	defer r.freshMu.RUnlock()
	return r.fresh
}

func (r *selectedTokenRotation) retain(token *oauth.Token) {
	r.freshMu.Lock()
	r.fresh = token
	r.freshMu.Unlock()
}

func (selectedTokenRotation) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth rotation receipt]"))
}

func (selectedTokenRotation) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth rotation receipts are private")
}

// refreshProviderOAuthTokenAtPath rotates a namespace-free owner's exact
// accepted token, then persists its successor without touching account storage.
// A returned token with an error is a retained successor, not acknowledgement
// that configuration or the remote runtime was published. Repeating the same
// admitted credential reuses only a proven in-memory or durable rotation receipt.
func (s *ConfigStore) refreshProviderOAuthTokenAtPath(ctx context.Context, path string, identityScope any, validateOrigin func(RuntimeSnapshot) error, owner providerregistry.RegistrationOwner, expected *oauth.Token, admitted RuntimeSnapshot) (*oauth.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.RemoteAuthority() != nil {
		return nil, ErrClientRuntimeManaged
	}
	if owner.ProviderID == "" || owner.AccountNamespace != "" || !owner.HasOAuth || expected == nil || expected.AccessToken == "" || expected.RefreshToken == "" {
		return nil, errors.New("refresh requires an exact namespace-free OAuth token and owner")
	}
	if err := validateRemoteOAuthToken(expected); err != nil {
		return nil, err
	}
	expected = cloneOAuthToken(expected)
	registerOAuthTokenSecrets(expected)
	definition, admittedOwner, err := admitted.clientProviderDefinitionRaw(owner.ProviderID)
	if err != nil || admittedOwner != owner {
		return nil, errors.New("selected provider definition is unavailable in the captured runtime")
	}
	registration, ok := admitted.Config().ProviderBehaviorRegistration(owner.ProviderID)
	if !ok || !owner.Matches(registration) || registration.OAuth == nil || registration.OAuth.Refresh == nil {
		return nil, errors.New("selected provider does not support OAuth refresh")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || isShellConfig(path) {
		return nil, errors.New("OAuth refresh requires its captured absolute JSON config path")
	}
	environment := admitted.Environment()
	validateRuntime := func(snapshot RuntimeSnapshot) error {
		if err := s.RuntimeRevocation(); err != nil {
			return err
		}
		if err := snapshot.RuntimeRevocation(); err != nil {
			return err
		}
		actual, actualOwner, err := snapshot.clientProviderDefinitionRaw(owner.ProviderID)
		provider, configured := snapshot.Config().Providers.Get(owner.ProviderID)
		if err != nil || actualOwner != owner || !configured || provider.Disable || !reflect.DeepEqual(actual, definition) || !snapshot.nativeIdentities.matches(environment) {
			return errors.New("selected provider definition or environment changed during OAuth refresh")
		}
		if validateOrigin != nil {
			return validateOrigin(snapshot)
		}
		return nil
	}
	definitionID, err := definition.Digest()
	if err != nil {
		return nil, err
	}
	digest, err := s.loadAuthenticationDigest(ctx)
	if err != nil {
		return nil, err
	}
	identity, _ := json.Marshal([]any{identityScope, owner, definitionID, OAuthTokenCredentialID(expected)})
	key := digest.bytesID(authenticationDigestOperation, identity)
	lockCtx, cancelLock := context.WithTimeout(ctx, refreshLockDeadline)
	defer cancelLock()
	release, err := lock.File(lockCtx, s.refreshLockPath(owner.ProviderID))
	if err != nil {
		return nil, authenticationInputError(err)
	}
	defer release()
	// The captured config path, not a store's global data directory, is the
	// shared identity for this journal. Different stores may point their
	// workspace scope at the same file while using different global roots.
	releaseLineage, err := lock.File(lockCtx, selectedTokenLineagePath(path, owner.ProviderID)+".lock")
	if err != nil {
		return nil, authenticationInputError(err)
	}
	defer releaseLineage()
	currentRuntime, err := s.captureRemoteCollectionRuntime(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateRuntime(currentRuntime); err != nil {
		return nil, err
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryLock, s.writeMu.Unlock); err != nil {
		return nil, err
	}
	receipt := s.selectedTokenRotations[key]
	if receipt != nil && receipt.exchangeStarted && receipt.token() == nil {
		s.writeMu.Unlock()
		return nil, errors.New("OAuth token exchange outcome is unknown; reauthenticate or recollect the owning client")
	}
	if receipt != nil && receipt.token() != nil && receipt.lineage != nil {
		_, journal, readErr := readSelectedTokenLineage(ctx, receipt.lineage.path, digest)
		if readErr != nil {
			s.writeMu.Unlock()
			return cloneOAuthToken(receipt.token()), fmt.Errorf("OAuth token rotated and retained; durable lineage cannot be read: %w", readErr)
		}
		if recorded, found := journal.Records[key]; found && recorded.Successor != nil {
			if !reflect.DeepEqual(recorded.Successor, receipt.token()) {
				s.writeMu.Unlock()
				return cloneOAuthToken(receipt.token()), errors.New("OAuth token rotated and retained; durable successor conflicts with the observed exchange")
			}
			restored, restoreErr := s.prepareSelectedTokenLineage(ctx, digest, currentRuntime, path, key, definitionID, owner, expected)
			if restoreErr != nil {
				s.writeMu.Unlock()
				return cloneOAuthToken(receipt.token()), fmt.Errorf("OAuth token rotated and retained; durable lineage cannot be adopted: %w", restoreErr)
			}
			receipt = restored
			s.selectedTokenRotations[key] = receipt
		}
	}
	if receipt == nil {
		for _, previous := range s.selectedTokenRotations {
			if previous.providerID == owner.ProviderID && previous.originalID == selectedTokenCredentialID(digest, expected) && previous.token() != nil {
				s.writeMu.Unlock()
				return nil, errors.New("this OAuth credential already has a retained rotation; reconcile its original provider definition before reuse")
			}
		}
		for id, previous := range s.selectedTokenRotations {
			if previous.committed && previous.next != currentRuntime.Config() {
				delete(s.selectedTokenRotations, id)
			}
		}
		if len(s.selectedTokenRotations) >= selectedTokenRotationLimit {
			s.writeMu.Unlock()
			return nil, errors.New("too many retained OAuth rotations; reconcile pending credentials before refreshing")
		}
		receipt, err = s.prepareSelectedTokenLineage(ctx, digest, currentRuntime, path, key, definitionID, owner, expected)
		if err != nil {
			s.writeMu.Unlock()
			return nil, err
		}
		if s.selectedTokenRotations == nil {
			s.selectedTokenRotations = make(map[string]*selectedTokenRotation)
		}
		s.selectedTokenRotations[key] = receipt
	}
	s.writeMu.Unlock()

	// Once exchange starts, caller disconnection cannot discard its successor.
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), selectedTokenCompletionTimeout)
	defer cancelFinish()
	finish, cancelRuntime := s.BindRuntimeContext(finish)
	defer cancelRuntime()
	if receipt.token() == nil {
		if err := ctx.Err(); err != nil {
			s.forgetUnexchangedSelectedToken(ctx, key, receipt)
			return nil, err
		}
		validateOwner := func() error {
			current := s.Config()
			actual, ok := current.ProviderOwner(owner.ProviderID)
			if current != receipt.before || !ok || actual != owner {
				return errors.New("selected OAuth credential or owner changed during exchange")
			}
			return nil
		}
		// Recheck the captured definition/environment and full file immediately
		// before handing the only selected refresh token to its registered flow.
		currentRuntime, err := s.captureRemoteCollectionRuntime(ctx)
		if err == nil {
			err = validateRuntime(currentRuntime)
		}
		if err == nil {
			err = validateOwner()
		}
		if err == nil {
			err = checkAuthenticationScopePreimage(ctx, receipt.preimage)
		}
		if err == nil {
			err = s.validateSelectedTokenSources(ctx, receipt.before)
		}
		if err != nil {
			s.forgetUnexchangedSelectedToken(ctx, key, receipt)
			return nil, authenticationInputError(err)
		}
		if err := receipt.lineage.persist(ctx, nil); err != nil {
			return nil, err
		}
		exchangeCtx := providertransport.ContextWithOwnerValidator(oauth.ContextWithEnvironment(finish, environment), validateOwner)
		var fresh *oauth.Token
		// Cleanup may fail to acquire writeMu after a deadline. Retaining an
		// unacknowledged exchange must never allow this token to be sent again.
		// The provider and captured-path locks serialize this flag's callers.
		receipt.exchangeStarted = true
		if s.exchangeToken != nil {
			fresh, err = s.exchangeToken(exchangeCtx, owner.ProviderID, expected.RefreshToken)
		} else {
			fresh, err = registration.OAuth.Refresh(exchangeCtx, expected.RefreshToken)
		}
		if err != nil || fresh == nil || fresh.AccessToken == "" {
			s.forgetUnexchangedSelectedToken(finish, key, receipt)
			if err == nil {
				err = errors.New("OAuth refresh returned no access token")
			}
			return nil, err
		}
		fresh = cloneOAuthToken(fresh)
		if fresh.RefreshToken == "" {
			fresh.RefreshToken = expected.RefreshToken
		}
		if fresh.Client == nil {
			fresh.Client = clonePointer(expected.Client)
		}
		registerOAuthTokenSecrets(fresh)
		receipt.retain(fresh)
	}
	if durable, recorded := receipt.lineage.journal.Records[receipt.lineage.key]; !recorded || !reflect.DeepEqual(durable.Successor, receipt.token()) {
		if err := receipt.lineage.persist(finish, receipt.token()); err != nil {
			return cloneOAuthToken(receipt.token()), fmt.Errorf("OAuth token rotated and retained in memory; durable lineage is not acknowledged: %w", err)
		}
	}
	if err := s.commitSelectedTokenRotation(finish, owner, registration, receipt, validateRuntime); err != nil {
		return cloneOAuthToken(receipt.token()), fmt.Errorf("OAuth token rotated and retained; provider configuration is not acknowledged: %w", err)
	}
	if durable := receipt.lineage.journal.Records[receipt.lineage.key]; !durable.Committed {
		receipt.lineage.record.Committed = true
		if err := receipt.lineage.persist(finish, receipt.token()); err != nil {
			return cloneOAuthToken(receipt.token()), fmt.Errorf("OAuth configuration saved; durable lineage completion is not acknowledged: %w", err)
		}
	}
	return cloneOAuthToken(receipt.token()), nil
}

func (s *ConfigStore) validateSelectedTokenSources(ctx context.Context, cfg *Config) error {
	if cfg.authenticationBasis == nil {
		return nil
	}
	return s.verifyAuthenticationCOW(ctx, cfg, slices.Sorted(maps.Keys(cfg.authenticationBasis.sources)))
}

func (s *ConfigStore) forgetUnexchangedSelectedToken(ctx context.Context, key string, receipt *selectedTokenRotation) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryLock, s.writeMu.Unlock); err != nil {
		// An unexchanged reservation can be retried with the same credential.
		// Cleanup must not turn cancellation into an unbounded global lock wait.
		return
	}
	defer s.writeMu.Unlock()
	if s.selectedTokenRotations[key] == receipt && receipt.token() == nil {
		delete(s.selectedTokenRotations, key)
	}
}

func selectedTokenDiskCredential(file authenticationInputFile, id string, provider ProviderConfig, expected *oauth.Token) error {
	if !file.info.exists || !authenticationLayerObject(file.data) {
		return errors.New("selected OAuth config credential is unavailable on disk")
	}
	if selectedTokenAliasedField(gjson.ParseBytes(file.data), "providers") {
		return errors.New("selected OAuth configuration has ambiguous provider fields")
	}
	stored := gjson.GetBytes(file.data, "providers."+id)
	if selectedTokenAliasedField(stored, "api_key", "oauth", "owner", "plugin", "preset") {
		return errors.New("selected OAuth configuration has ambiguous credential fields")
	}
	var token oauth.Token
	if !stored.Get("oauth").Exists() || json.Unmarshal([]byte(stored.Get("oauth").Raw), &token) != nil || !reflect.DeepEqual(&token, expected) || stored.Get("api_key").String() != expected.AccessToken {
		return errors.New("selected OAuth config credential changed on disk; recollect the owning client runtime")
	}
	for name, value := range map[string]any{"owner": provider.Owner, "plugin": provider.Plugin, "preset": provider.Preset} {
		if raw := stored.Get(name); raw.Exists() {
			encoded, err := json.Marshal(value)
			if err != nil || !reflect.DeepEqual(raw.Value(), gjson.ParseBytes(encoded).Value()) {
				return errors.New("selected OAuth provider owner changed on disk")
			}
		}
	}
	return nil
}

func selectedTokenAliasedField(object gjson.Result, fields ...string) bool {
	ambiguous := false
	object.ForEach(func(key, _ gjson.Result) bool {
		for _, field := range fields {
			if key.Str != field && strings.EqualFold(key.Str, field) {
				ambiguous = true
				return false
			}
		}
		return true
	})
	return ambiguous
}

func (s *ConfigStore) commitSelectedTokenRotation(ctx context.Context, owner providerregistry.RegistrationOwner, registration providerregistry.Registration, receipt *selectedTokenRotation, validateRuntime func(RuntimeSnapshot) error) error {
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryLock, s.writeMu.Unlock); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	if err := lockAuthenticationMutex(ctx, s.configMu.TryLock, s.configMu.Unlock); err != nil {
		return err
	}
	current := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	if err := validateRuntime(current); err != nil {
		return err
	}
	if !current.nativeIdentities.matches(receipt.environment) {
		return errors.New("captured OAuth refresh environment changed")
	}
	if receipt.committed {
		if current.Config() != receipt.next {
			return errors.New("saved OAuth successor is no longer the current client configuration")
		}
		if err := s.validateSelectedTokenSources(ctx, current.Config()); err != nil {
			return err
		}
		return checkAuthenticationScopePreimage(ctx, receipt.postimage)
	}
	if current.Config() != receipt.before {
		return errors.New("client configuration changed after the admitted OAuth credential")
	}
	if err := lockAuthenticationMutex(ctx, s.mu.TryLock, s.mu.Unlock); err != nil {
		return err
	}
	defer s.mu.Unlock()
	release, err := lock.File(ctx, receipt.preimage.path+".lock")
	if err != nil {
		return authenticationInputError(err)
	}
	defer release()
	if !receipt.written {
		if err := checkAuthenticationScopePreimage(ctx, receipt.preimage); err != nil {
			return err
		}
		if err := s.validateSelectedTokenSources(ctx, current.Config()); err != nil {
			return err
		}
		provider, _ := current.Config().Providers.Get(owner.ProviderID)
		fresh := receipt.token()
		applyOAuthTokenToProvider(&provider, cloneOAuthToken(fresh), registration)
		next := current.Config().cloneForWrite()
		next.Providers.Set(owner.ProviderID, provider)
		fields := map[string]any{"providers." + owner.ProviderID + ".api_key": fresh.AccessToken, "providers." + owner.ProviderID + ".oauth": fresh}
		data := bytes.Clone(receipt.preimage.data)
		for _, key := range slices.Sorted(maps.Keys(fields)) {
			data, err = sjson.SetBytes(data, key, fields[key])
			if err != nil {
				return errors.New("cannot stage exact OAuth credential fields")
			}
		}
		if _, err := s.prepareAuthenticationCOW(ctx, next, receipt.preimage.path, fields, nil); err != nil {
			return err
		}
		stage, err := stageAuthenticationScopeWrite(ctx, receipt.preimage, authenticationCredentialEdit{path: receipt.preimage.path, data: data})
		if err != nil {
			return err
		}
		defer stage.Close()
		receipt.next = next
		receipt.postimage, receipt.written, err = stage.Commit(ctx)
		if err != nil {
			return err
		}
		if err := s.validateSelectedTokenSources(ctx, next); err != nil {
			return err
		}
	} else {
		if receipt.postimage.path == "" {
			return errors.New("OAuth token was written but its postimage is unverified; reconcile configuration before reuse")
		}
		if err := checkAuthenticationScopePreimage(ctx, receipt.postimage); err != nil {
			return err
		}
		if err := s.validateSelectedTokenSources(ctx, receipt.next); err != nil {
			return err
		}
	}
	s.captureStalenessSnapshot(append(slices.Clone(s.loadedPaths), receipt.preimage.path))
	s.setConfig(receipt.next)
	receipt.committed = true
	return nil
}
