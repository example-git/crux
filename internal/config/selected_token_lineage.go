package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	selectedTokenLineageVersion   = 1
	maxSelectedTokenLineageBytes  = 64 << 20
	maxSelectedTokenPreimageBytes = 8 << 20
)

// This private sidecar is scoped to the captured config path and provider.
// The provider refresh lock and captured-path journal lock serialize reads and
// writes across stores/processes. A started record without a successor is ambiguous:
// no caller may infer that its refresh token remains safe to exchange.
type selectedTokenLineageJournal struct {
	Version  int                                   `json:"version"`
	Sequence uint64                                `json:"sequence"`
	Records  map[string]selectedTokenLineageRecord `json:"records"`
}

type selectedTokenLineageRecord struct {
	Sequence    uint64                             `json:"sequence"`
	Committed   bool                               `json:"committed,omitempty"`
	Owner       providerregistry.RegistrationOwner `json:"owner"`
	Definition  string                             `json:"definition"`
	Original    string                             `json:"original"`
	Environment string                             `json:"environment"`
	Runtime     string                             `json:"runtime"`
	Order       []string                           `json:"order"`
	Inputs      []selectedTokenInputProof          `json:"inputs"`
	Before      selectedTokenInputProof            `json:"before"`
	BeforeData  []byte                             `json:"before_data"`
	Successor   *oauth.Token                       `json:"successor,omitempty"`
}

type selectedTokenInputProof struct {
	Path     string            `json:"path"`
	Exists   bool              `json:"exists"`
	Identity [sha256.Size]byte `json:"identity"`
	Size     int64             `json:"size"`
	Mode     uint32            `json:"mode"`
	Modified int64             `json:"modified"`
	Digest   string            `json:"digest"`
}

type selectedTokenLineage struct {
	path    string
	key     string
	file    authenticationInputFile
	journal selectedTokenLineageJournal
	record  selectedTokenLineageRecord
}

func (selectedTokenLineage) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth token lineage holder]"))
}

func (selectedTokenLineageRecord) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth token lineage]"))
}

func (selectedTokenLineageJournal) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth token lineage journal]"))
}

func selectedTokenProof(file authenticationInputFile) selectedTokenInputProof {
	return selectedTokenInputProof{Path: file.path, Exists: file.info.exists, Identity: file.info.identity, Size: file.info.size, Mode: uint32(file.info.mode), Modified: file.info.modified, Digest: selectedTokenBytesID(file.data)}
}

func (p selectedTokenInputProof) file(data []byte) authenticationInputFile {
	return authenticationInputFile{path: p.Path, data: bytes.Clone(data), info: authenticationInputFileInfo{exists: p.Exists, identity: p.Identity, size: p.Size, mode: os.FileMode(p.Mode), modified: p.Modified}}
}

func selectedTokenBytesID(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func selectedTokenLineagePath(path, providerID string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".oauth-"+selectedTokenBytesID([]byte(providerID))+".json")
}

// Read size and permissions before allocating or decoding secret data. Keep
// the same no-follow, stable-inode checks as captured configuration inputs.
func readSelectedTokenLineage(ctx context.Context, path string) (authenticationInputFile, selectedTokenLineageJournal, error) {
	before := authenticationInputFile{path: path}
	journal := selectedTokenLineageJournal{Version: selectedTokenLineageVersion, Records: map[string]selectedTokenLineageRecord{}}
	if err := ctx.Err(); err != nil {
		return before, journal, err
	}
	file, err := openAuthenticationInput(path)
	if errors.Is(err, os.ErrNotExist) {
		return before, journal, ctx.Err()
	}
	if err != nil {
		return before, journal, errors.New("OAuth token lineage cannot be opened")
	}
	defer file.Close()
	before.info, err = observeAuthenticationInput(file)
	if err != nil || before.info.size > maxSelectedTokenLineageBytes || fsext.ValidatePrivateFile(file) != nil {
		return before, journal, errors.New("OAuth token lineage has invalid size or permissions")
	}
	before.data, err = io.ReadAll(io.LimitReader(authenticationInputReader{ctx: ctx, reader: file}, maxSelectedTokenLineageBytes+1))
	if err != nil {
		return before, journal, authenticationInputError(err)
	}
	if len(before.data) > maxSelectedTokenLineageBytes {
		return before, journal, errors.New("OAuth token lineage cannot be read")
	}
	if err := verifyAuthenticationInput(ctx, path, file, before.info); err != nil {
		return before, journal, authenticationInputError(err)
	}
	if !authenticationLayerObject(before.data) || !selectedTokenLineageUnambiguous(gjson.ParseBytes(before.data)) {
		return before, journal, errors.New("OAuth token lineage is malformed")
	}
	decoder := json.NewDecoder(bytes.NewReader(before.data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&journal) != nil || decoder.Decode(new(any)) != io.EOF || journal.Version != selectedTokenLineageVersion || journal.Records == nil || len(journal.Records) > selectedTokenRotationLimit {
		return before, journal, errors.New("OAuth token lineage is malformed or unsupported")
	}
	for key, record := range journal.Records {
		registerOAuthTokenSecrets(record.Successor)
		if len(key) != 64 || record.Sequence == 0 || record.Sequence > journal.Sequence || record.Committed && record.Successor == nil || record.Original == "" || record.Definition == "" || len(record.BeforeData) > maxSelectedTokenPreimageBytes || len(record.Inputs) > 256 || selectedTokenBytesID(record.BeforeData) != record.Before.Digest || !filepath.IsAbs(record.Before.Path) || filepath.Clean(record.Before.Path) != record.Before.Path {
			return before, journal, errors.New("OAuth token lineage record is invalid")
		}
		if record.Successor != nil {
			if err := validateRemoteOAuthToken(record.Successor); err != nil {
				return before, journal, errors.New("OAuth token lineage successor is invalid")
			}
		}
	}
	return before, journal, nil
}

// encoding/json accepts case-insensitive struct field aliases. Journals are
// authored with exact names, so conflicting aliases have no valid meaning.
func selectedTokenLineageUnambiguous(value gjson.Result) bool {
	valid := true
	seen := map[string]bool{}
	if value.IsObject() || value.IsArray() {
		value.ForEach(func(key, child gjson.Result) bool {
			if value.IsObject() {
				name := strings.ToLower(key.Str)
				if seen[name] {
					valid = false
					return false
				}
				seen[name] = true
			}
			valid = selectedTokenLineageUnambiguous(child)
			return valid
		})
	}
	return valid
}

// canonicalTokenDocument changes only the selected credential when computing
// equivalence across our exact write. All other JSON values remain bound;
// UseNumber preserves large integers and decimal spelling.
func canonicalTokenDocument(data []byte, id string, original *oauth.Token) []byte {
	if len(data) == 0 {
		return nil
	}
	if !json.Valid(data) {
		return bytes.Clone(data)
	}
	changed, err := sjson.SetBytes(data, "providers."+id+".api_key", original.AccessToken)
	if err == nil {
		changed, err = sjson.SetBytes(changed, "providers."+id+".oauth", original)
	}
	if err != nil {
		return bytes.Clone(data)
	}
	decoder := json.NewDecoder(bytes.NewReader(changed))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return bytes.Clone(data)
	}
	result, err := json.Marshal(value)
	if err != nil {
		return bytes.Clone(data)
	}
	return result
}

func selectedTokenRuntimeID(snapshot RuntimeSnapshot, original *oauth.Token, record selectedTokenLineageRecord) (string, error) {
	cfg := snapshot.Config()
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", errors.New("OAuth token runtime cannot be captured")
	}
	aliases := map[string]bool{record.Before.Path: true}
	for _, input := range record.Inputs {
		if input.Exists && input.Identity == record.Before.Identity {
			aliases[input.Path] = true
		}
	}
	sourceID := func(path string, source authenticationBasisSource) any {
		raw, evaluated := source.raw, source.evaluated
		if aliases[path] {
			raw = canonicalTokenDocument(raw, record.Owner.ProviderID, original)
			evaluated = canonicalTokenDocument(evaluated, record.Owner.ProviderID, original)
		}
		return []any{source.exists, selectedTokenBytesID(raw), selectedTokenBytesID(evaluated)}
	}
	var basis any
	if b := cfg.authenticationBasis; b != nil {
		sources := map[string]any{}
		for path, source := range b.sources {
			sources[path] = sourceID(path, source)
		}
		// The loader's configured blob predates defaults/migrations and is
		// historical, not the published runtime. The complete current config
		// is bound below. Delivery's separate read owns only delivery_mode,
		// matching validateConfigBasis/authenticationDeliveryEqual.
		delivery := gjson.GetBytes(b.delivery.raw, "options.tui.delivery_mode")
		basis = []any{b.valid, b.noUnset, b.order, sources, b.deliveryPath, delivery.Exists(), delivery.Type, delivery.Raw}
	}
	encoded, err := json.Marshal([]any{canonicalTokenDocument(data, record.Owner.ProviderID, original), basis, cfg.authenticationCandidates})
	if err != nil {
		return "", errors.New("OAuth token runtime basis cannot be captured")
	}
	return selectedTokenBytesID(encoded), nil
}

func selectedTokenEnvironmentID(environment []string) string {
	values := slices.Clone(environment)
	slices.Sort(values)
	encoded, _ := json.Marshal(values)
	return selectedTokenBytesID(encoded)
}

func selectedTokenSuccessorData(record selectedTokenLineageRecord) ([]byte, error) {
	if record.Successor == nil {
		return nil, errors.New("OAuth token exchange has no recorded successor; reauthenticate or recollect the owning client")
	}
	data, err := sjson.SetBytes(bytes.Clone(record.BeforeData), "providers."+record.Owner.ProviderID+".api_key", record.Successor.AccessToken)
	if err == nil {
		data, err = sjson.SetBytes(data, "providers."+record.Owner.ProviderID+".oauth", record.Successor)
	}
	if err != nil {
		return nil, errors.New("OAuth token successor cannot be projected")
	}
	return data, nil
}

// Called with writeMu and the cross-process provider refresh lock held.
func (s *ConfigStore) prepareSelectedTokenLineage(ctx context.Context, current RuntimeSnapshot, path, key, definitionID string, owner providerregistry.RegistrationOwner, expected *oauth.Token) (*selectedTokenRotation, error) {
	file, journal, err := readSelectedTokenLineage(ctx, selectedTokenLineagePath(path, owner.ProviderID))
	if err != nil {
		return nil, err
	}
	provider, _ := current.Config().Providers.Get(owner.ProviderID)
	if s.Config() != current.Config() || provider.resolvedAPIKey != nil {
		return nil, errors.New("selected OAuth credential changed; recollect the owning client runtime")
	}
	inputs, err := s.captureAuthenticationInputsLocked(ctx, current)
	if err != nil {
		return nil, err
	}
	before, found := inputs.file(path)
	if !found || len(before.data) > maxSelectedTokenPreimageBytes || len(inputs.files) > 256 {
		return nil, errors.New("OAuth token captured inputs exceed the lineage limit")
	}
	record, exists := journal.Records[key]
	if !exists {
		for _, previous := range journal.Records {
			if previous.Original == OAuthTokenCredentialID(expected) {
				return nil, errors.New("this OAuth credential has a durable exchange under another provider definition; reconcile its original selection")
			}
		}
		if err := pruneSelectedTokenLineages(&journal, expected); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(provider.OAuthToken, expected) || provider.APIKey != expected.AccessToken {
			return nil, errors.New("selected OAuth credential changed; recollect the owning client runtime")
		}
		if err := selectedTokenDiskCredential(before, owner.ProviderID, provider, expected); err != nil {
			return nil, err
		}
		if journal.Sequence == ^uint64(0) {
			return nil, errors.New("OAuth token lineage sequence is exhausted")
		}
		record = selectedTokenLineageRecord{Sequence: journal.Sequence + 1, Owner: owner, Definition: definitionID, Original: OAuthTokenCredentialID(expected), Environment: selectedTokenEnvironmentID(current.Environment()), Order: slices.Clone(inputs.order), Before: selectedTokenProof(before), BeforeData: bytes.Clone(before.data)}
		for _, input := range inputs.files {
			record.Inputs = append(record.Inputs, selectedTokenProof(input))
		}
		record.Runtime, err = selectedTokenRuntimeID(current, expected, record)
		if err != nil {
			return nil, err
		}
		// Reserve enough journal capacity for the largest admitted token before
		// any exchange can consume the selected credential.
		journal.Records[key] = record
		encoded, err := json.Marshal(journal)
		delete(journal.Records, key)
		if err != nil || len(encoded)+2*maxRemoteOAuthTokenBytes > maxSelectedTokenLineageBytes {
			return nil, errors.New("OAuth token lineage storage is full")
		}
	} else {
		if record.Owner != owner || record.Definition != definitionID || record.Original != OAuthTokenCredentialID(expected) || record.Environment != selectedTokenEnvironmentID(current.Environment()) || record.Before.Path != path {
			return nil, errors.New("OAuth token lineage does not match the captured owner and environment")
		}
		if record.Successor == nil {
			return nil, errors.New("OAuth token exchange outcome is unknown; reauthenticate or recollect the owning client")
		}
		if !reflect.DeepEqual(provider.OAuthToken, expected) && !reflect.DeepEqual(provider.OAuthToken, record.Successor) {
			return nil, errors.New("OAuth token lineage does not match the current client credential")
		}
		if provider.APIKey != provider.OAuthToken.AccessToken {
			return nil, errors.New("OAuth token lineage credential is inconsistent")
		}
		runtimeID, err := selectedTokenRuntimeID(current, expected, record)
		if err != nil || runtimeID != record.Runtime {
			return nil, errors.New("OAuth token lineage captured configuration changed")
		}
		if err := verifySelectedTokenLineageInputs(inputs, record); err != nil {
			return nil, err
		}
	}
	lineage := &selectedTokenLineage{path: file.path, key: key, file: file, journal: journal, record: record}
	receipt := &selectedTokenRotation{freshMu: new(sync.RWMutex), providerID: owner.ProviderID, originalID: record.Original, environment: current.Environment(), before: current.Config(), preimage: record.Before.file(record.BeforeData), lineage: lineage}
	if record.Successor != nil {
		receipt.retain(cloneOAuthToken(record.Successor))
		planned, err := selectedTokenSuccessorData(record)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(before.data, planned) {
			receipt.written = true
			receipt.postimage = before
			registration, _ := current.Config().ProviderBehaviorRegistration(owner.ProviderID)
			next := current.Config().cloneForWrite()
			applyOAuthTokenToProvider(&provider, cloneOAuthToken(record.Successor), registration)
			next.Providers.Set(owner.ProviderID, provider)
			fields := map[string]any{"providers." + owner.ProviderID + ".api_key": record.Successor.AccessToken, "providers." + owner.ProviderID + ".oauth": record.Successor}
			if _, err := s.prepareAuthenticationCOW(ctx, next, path, fields, nil); err != nil {
				return nil, err
			}
			receipt.next = next
		} else if !reflect.DeepEqual(provider.OAuthToken, expected) {
			return nil, errors.New("OAuth token lineage successor is not present in its configuration")
		}
	}
	return receipt, nil
}

// Only verified committed histories can be evicted. The direct predecessor of
// the currently selected token and every unresolved exchange remain retained.
// An evicted caller cannot exchange from a different current config/token:
// ordinary new-lineage admission still requires its exact selected preimage.
func pruneSelectedTokenLineages(journal *selectedTokenLineageJournal, expected *oauth.Token) error {
	if len(journal.Records) < selectedTokenRotationLimit {
		return nil
	}
	original := OAuthTokenCredentialID(expected)
	oldest := ""
	for key, record := range journal.Records {
		if !record.Committed || record.Original == original || OAuthTokenCredentialID(record.Successor) == original {
			continue
		}
		if oldest == "" || record.Sequence < journal.Records[oldest].Sequence {
			oldest = key
		}
	}
	if oldest == "" {
		return errors.New("too many unresolved OAuth token lineages; reconcile pending credentials before refreshing")
	}
	delete(journal.Records, oldest)
	return nil
}

func verifySelectedTokenLineageInputs(inputs authenticationConfigInputs, record selectedTokenLineageRecord) error {
	if !slices.Equal(inputs.order, record.Order) || len(inputs.files) != len(record.Inputs) {
		return errors.New("OAuth token lineage input topology changed")
	}
	planned, err := selectedTokenSuccessorData(record)
	if err != nil {
		return err
	}
	for index, input := range inputs.files {
		proof := record.Inputs[index]
		if selectedTokenProof(input) == proof {
			continue
		}
		// Only the target and its already captured inode aliases may carry the
		// exact recorded successor write. Unrelated inputs require exact proof.
		if input.path == proof.Path && proof.Exists && proof.Identity == record.Before.Identity && input.info.exists && input.info.mode.Perm() == 0o600 && bytes.Equal(input.data, planned) {
			continue
		}
		return errors.New("OAuth token lineage captured inputs changed")
	}
	return nil
}

// Store a started record before exchange, then its exact decoded successor
// before the config write. A canceled caller cannot discard that successor.
func (l *selectedTokenLineage) persist(ctx context.Context, successor *oauth.Token) error {
	if l == nil {
		return errors.New("OAuth token lineage is unavailable")
	}
	if successor != nil {
		if err := validateRemoteOAuthToken(successor); err != nil {
			return err
		}
		l.record.Successor = cloneOAuthToken(successor)
	}
	next := selectedTokenLineageJournal{Version: selectedTokenLineageVersion, Sequence: max(l.journal.Sequence, l.record.Sequence), Records: maps.Clone(l.journal.Records)}
	next.Records[l.key] = l.record
	data, err := json.Marshal(next)
	if err != nil || len(data) > maxSelectedTokenLineageBytes {
		return errors.New("OAuth token lineage cannot be encoded within its limit")
	}
	stage, err := stageAuthenticationScopeWrite(ctx, l.file, authenticationCredentialEdit{path: l.path, data: data})
	if err != nil {
		return fmt.Errorf("OAuth token lineage cannot be staged: %w", authenticationInputError(err))
	}
	defer stage.Close()
	after, written, err := stage.Commit(ctx)
	if written {
		if after.path != "" {
			l.file = after
		} else {
			// A rename followed by a failed sync/read is still a write. A
			// retry may stage the identical known bytes from that observed
			// preimage, but never claim that the failed write was durable.
			observed, readErr := readAuthenticationInput(ctx, l.path)
			if readErr == nil && observed.info.exists && observed.info.mode.Perm() == 0o600 && bytes.Equal(observed.data, data) {
				l.file = observed
			}
		}
	}
	if err != nil {
		return fmt.Errorf("OAuth token lineage write is not acknowledged: %w", authenticationInputError(err))
	}
	l.file, l.journal = after, next
	return nil
}
