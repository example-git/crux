package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/example-git/crux/internal/shell"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	errAuthenticationBasisUnavailable = errors.New("authentication configuration has no accepted input basis; reload configuration before changing authentication")
	errAuthenticationBasisChanged     = errors.New("saved configuration differs from the accepted runtime; reload configuration before changing authentication")
)

// authenticationLoadBasis records inputs at the reads/evaluations that actually
// produced an accepted configuration. Status must never manufacture this proof
// by observing the current disk. A builder is private to an unpublished load;
// published bases are immutable and typed changes make independent copies.
type authenticationLoadBasis struct {
	valid        bool
	noUnset      bool
	order        []string
	sources      map[string]authenticationBasisSource
	deliveryPath string
	delivery     authenticationBasisSource
	configured   []byte
}

type authenticationBasisSource struct {
	exists         bool
	raw, evaluated []byte
}

func (authenticationLoadBasis) MarshalJSON() ([]byte, error) {
	return nil, errors.New("accepted authentication inputs are private")
}

func (authenticationLoadBasis) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private accepted authentication inputs]"))
}

func (authenticationBasisSource) MarshalJSON() ([]byte, error) {
	return nil, errors.New("accepted authentication source is private")
}

func (authenticationBasisSource) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private accepted authentication source]"))
}

func newAuthenticationLoadBasis() *authenticationLoadBasis {
	return &authenticationLoadBasis{valid: true, noUnset: shell.NoUnset.Load(), sources: map[string]authenticationBasisSource{}}
}

func (b *authenticationLoadBasis) source(path string, raw, evaluated []byte, exists bool) {
	if b == nil || path == "" {
		return
	}
	path = filepath.Clean(path)
	b.order = append(b.order, path)
	value := authenticationBasisSource{exists: exists, raw: bytes.Clone(raw), evaluated: bytes.Clone(evaluated)}
	if previous, found := b.sources[path]; found && (previous.exists != value.exists || !bytes.Equal(previous.raw, value.raw) || !bytes.Equal(previous.evaluated, value.evaluated)) {
		// Two reads of one path cannot establish one accepted preimage when
		// they disagree. Retain a visible unavailable basis, never the last read.
		b.valid = false
	}
	b.sources[path] = value
}

func (b *authenticationLoadBasis) configuration(cfg *Config) {
	if b == nil {
		return
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		b.valid = false
		return
	}
	b.configured = data
}

func (b *authenticationLoadBasis) deliverySource(path string, data []byte, exists bool) {
	if b == nil {
		return
	}
	b.deliveryPath = filepath.Clean(path)
	b.delivery = authenticationBasisSource{exists: exists, raw: bytes.Clone(data)}
	if source, found := b.sources[b.deliveryPath]; !found || !authenticationDeliveryEqual(source.raw, data) {
		// The later preference read must not conceal a different value from
		// the main merge. Such a load has no single accepted file preimage.
		b.valid = false
	}
}

func (b *authenticationLoadBasis) clone() *authenticationLoadBasis {
	if b == nil {
		return nil
	}
	next := *b
	next.order = slices.Clone(b.order)
	next.sources = maps.Clone(b.sources)
	// Source slices are immutable. Every replacement allocates its own bytes.
	next.configured = bytes.Clone(b.configured)
	return &next
}

// authored applies exactly the writer's declared JSON paths to its retained
// accepted source. In particular it never adopts the writer's whole disk
// preimage, which may already contain unrelated edits from another process.
func (b *authenticationLoadBasis) authored(path string, fields map[string]any, removed []string) *authenticationLoadBasis {
	if b == nil {
		return nil
	}
	next := b.clone()
	path = filepath.Clean(path)
	source, found := next.sources[path]
	if !found || isShellConfig(path) {
		next.valid = false
		return next
	}
	data := bytes.Clone(source.raw)
	if len(data) == 0 {
		data = []byte("{}")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var err error
	for _, key := range keys {
		data, err = sjson.SetBytes(data, key, fields[key])
		if err != nil {
			next.valid = false
			return next
		}
	}
	for _, key := range removed {
		data, err = sjson.DeleteBytes(data, key)
		if err != nil {
			next.valid = false
			return next
		}
	}
	next.sources[path] = authenticationBasisSource{exists: true, raw: data, evaluated: bytes.Clone(data)}
	if path == next.deliveryPath {
		// Delivery preference is the one separately read input from this file.
		// Keep all other bytes out of this auxiliary observation's authority.
		next.delivery = next.sources[path]
	}
	return next
}

func (c *Config) advanceAuthenticationBasis(path string, fields map[string]any, removed []string) {
	c.authenticationBasis = c.authenticationBasis.authored(path, fields, removed)
}

// advanceAuthenticationBasisCredentials mirrors the literal field receipt from
// stageCredentials. It deliberately does not adopt edit.data wholesale.
func (c *Config) advanceAuthenticationBasisCredentials(path, providerID string, desired *ProviderConfig) {
	b := c.authenticationBasis.clone()
	if b == nil {
		return
	}
	path = filepath.Clean(path)
	source, found := b.sources[path]
	if !found || providerID == "" || isShellConfig(path) {
		b.valid = false
		c.authenticationBasis = b
		return
	}
	data := bytes.Clone(source.raw)
	if len(data) == 0 {
		data = []byte("{}")
	}
	if desired != nil && (desired.ID != providerID || desired.OAuthToken == nil || desired.Owner == nil) {
		b.valid = false
		c.authenticationBasis = b
		return
	}
	fields := authenticationCredentialValues(desired)
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		value, err := json.Marshal(fields[key])
		if err == nil {
			data, err = runtimeControlChangeField(data, []string{"providers", providerID, key}, value, desired == nil)
		}
		if err != nil {
			b.valid = false
			c.authenticationBasis = b
			return
		}
	}
	b.sources[path] = authenticationBasisSource{exists: true, raw: data, evaluated: bytes.Clone(data)}
	c.authenticationBasis = b
}

func (b *authenticationLoadBasis) startupCorrections(plan notificationMigrationPlan, modelFields map[string]any, globalPath string) *authenticationLoadBasis {
	if b == nil {
		return nil
	}
	for path := range plan.overrides {
		fields := map[string]any{}
		if plan.setNotifications && path == plan.dataConfig {
			fields["options.notifications"] = plan.value
		}
		var removed []string
		if plan.cleanPaths[path] {
			removed = []string{"options.disable_notifications", "options.notification_style"}
		}
		b = b.authored(path, fields, removed)
	}
	if len(modelFields) > 0 {
		b = b.authored(globalPath, modelFields, nil)
	}
	return b
}

// validateConfigBasis is the pure pre-change admission seam for the fixed auth
// transaction. Exact input identity is stronger than trying to invert catalog
// expansion, defaults or resolved headers. credentialProviderID permits only
// that provider's literal api_key/oauth repair; ownership and all siblings are
// still compared. The caller separately validates the initiating exact owner.
func (c AuthenticationCapture) validateConfigBasis(layers authenticationLayers, credentialProviderID string) error {
	if c.runtime.IsClientOwned() {
		return ErrClientRuntimeManaged
	}
	cfg := c.runtime.Config()
	if cfg == nil || cfg.authenticationBasis == nil || !cfg.authenticationBasis.valid {
		return errAuthenticationBasisUnavailable
	}
	b := cfg.authenticationBasis
	if !c.inputs.valid || !slices.Equal(b.order, c.inputs.order) || !slices.Equal(b.order, layers.order) {
		return errAuthenticationBasisChanged
	}
	for path, accepted := range b.sources {
		current, found := c.inputs.file(path)
		if !found || current.info.exists != accepted.exists {
			return errAuthenticationBasisChanged
		}
		evaluated, evaluatedFound := layers.values[path]
		if !evaluatedFound {
			return errAuthenticationBasisChanged
		}
		if isShellConfig(path) {
			if !bytes.Equal(accepted.raw, current.data) {
				return errAuthenticationBasisChanged
			}
		} else if !authenticationBasisJSONEqual(accepted.raw, current.data, credentialProviderID) {
			return errAuthenticationBasisChanged
		}
		if !authenticationBasisJSONEqual(accepted.evaluated, evaluated, credentialProviderID) {
			return errAuthenticationBasisChanged
		}
	}
	if b.deliveryPath != "" {
		file, found := c.inputs.file(b.deliveryPath)
		if !found || !authenticationDeliveryEqual(b.delivery.raw, file.data) {
			return errAuthenticationBasisChanged
		}
	}
	return nil
}

func authenticationDeliveryEqual(left, right []byte) bool {
	l, r := gjson.GetBytes(left, "options.tui.delivery_mode"), gjson.GetBytes(right, "options.tui.delivery_mode")
	return l.Exists() == r.Exists() && (!l.Exists() || l.Type == r.Type && l.Raw == r.Raw)
}

func authenticationBasisJSONEqual(left, right []byte, providerID string) bool {
	clean := func(data []byte) ([]byte, bool) {
		if len(data) == 0 {
			data = []byte("{}")
		}
		if !authenticationLayerObject(data) {
			return nil, false
		}
		if providerID == "" {
			return data, true
		}
		var value map[string]any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		if providers, ok := value["providers"].(map[string]any); ok {
			if provider, ok := providers[providerID].(map[string]any); ok {
				delete(provider, "api_key")
				delete(provider, "oauth")
				if len(provider) == 0 {
					delete(providers, providerID)
				}
			}
			if len(providers) == 0 {
				delete(value, "providers")
			}
		}
		data, err := json.Marshal(value)
		return data, err == nil
	}
	l, lok := clean(left)
	r, rok := clean(right)
	return lok && rok && RuntimeControlJSONEqual(l, r)
}
