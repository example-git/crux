package pathtranslate

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const AliasPrefix = "pt_"

type Store struct {
	db *sql.DB
}

type Policy struct {
	Scope    Scope    `json:"scope"`
	Settings Settings `json:"settings"`
	Revision int64    `json:"revision"`
}

type Names struct {
	store        *Store
	secret       []byte
	databasePath string
}

type Name struct {
	Identity string
	Alias    string
	Path     string
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Save(ctx context.Context, policy Policy) (Policy, error) {
	key, err := policy.Scope.Key()
	if err != nil {
		return Policy{}, err
	}
	if err := policy.Settings.Validate(); err != nil {
		return Policy{}, err
	}
	if policy.Revision < 0 {
		return Policy{}, errors.New("path translation revision cannot be negative")
	}
	settings, err := json.Marshal(policy.Settings)
	if err != nil {
		return Policy{}, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Policy{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO path_translation_identity (id, secret) VALUES (1, ?) ON CONFLICT (id) DO NOTHING`, secret); err != nil {
		return Policy{}, err
	}
	if policy.Revision == 0 {
		err = s.db.QueryRowContext(ctx, `INSERT INTO path_translation_scopes (scope_key, settings)
			VALUES (?, ?) ON CONFLICT (scope_key) DO NOTHING RETURNING revision`, key, string(settings)).Scan(&policy.Revision)
	} else {
		err = s.db.QueryRowContext(ctx, `UPDATE path_translation_scopes SET settings = ?, revision = revision + 1
			WHERE scope_key = ? AND revision = ? RETURNING revision`, string(settings), key, policy.Revision).Scan(&policy.Revision)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, errors.New("path translation settings changed; reload before saving")
	}
	if err != nil {
		return Policy{}, fmt.Errorf("save path translation settings: %w", err)
	}
	return policy, nil
}

func (s *Store) Load(ctx context.Context, scope Scope) (Policy, bool, error) {
	key, err := scope.Key()
	if err != nil {
		return Policy{}, false, err
	}
	policy := Policy{Scope: scope}
	var settings string
	err = s.db.QueryRowContext(ctx, `SELECT settings, revision FROM path_translation_scopes WHERE scope_key = ?`, key).Scan(&settings, &policy.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, false, nil
	}
	if err != nil {
		return Policy{}, false, err
	}
	if err := json.Unmarshal([]byte(settings), &policy.Settings); err != nil {
		return Policy{}, false, fmt.Errorf("decode path translation settings: %w", err)
	}
	if err := policy.Settings.Validate(); err != nil {
		return Policy{}, false, err
	}
	return policy, true, nil
}

func (s *Store) Effective(ctx context.Context, sessionID string) (Policy, bool, error) {
	if sessionID == "" {
		return Policy{}, false, nil
	}
	return s.Load(ctx, Scope{Kind: "session", SessionID: sessionID})
}

func (s *Store) Names(ctx context.Context) (*Names, error) {
	var secret []byte
	if err := s.db.QueryRowContext(ctx, `SELECT secret FROM path_translation_identity WHERE id = 1`).Scan(&secret); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load path translation identity: %w", err)
	}
	if len(secret) != 32 {
		return nil, errors.New("invalid path translation identity key")
	}
	var databasePath string
	if err := s.db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&databasePath); err != nil {
		return nil, fmt.Errorf("locate path translation identity store: %w", err)
	}
	return &Names{store: s, secret: secret, databasePath: databasePath}, nil
}

func (n *Names) identity(path string) string {
	hash := hmac.New(sha256.New, n.secret)
	hash.Write([]byte("crux-path-translation-v1\x00"))
	hash.Write([]byte(path))
	return hex.EncodeToString(hash.Sum(nil))
}

func (n *Names) Register(ctx context.Context, path string) (Name, error) {
	id := n.identity(path)
	name := Name{Identity: id, Alias: AliasPrefix + id, Path: path}
	var storedPath string
	err := n.store.db.QueryRowContext(ctx, `SELECT alias, path FROM path_translation_names WHERE identity = ?`, id).Scan(&name.Alias, &storedPath)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = n.store.db.ExecContext(ctx, `INSERT INTO path_translation_names (identity, alias, path)
			VALUES (?, ?, ?) ON CONFLICT (identity) DO NOTHING`, name.Identity, name.Alias, name.Path)
		if err != nil {
			return Name{}, fmt.Errorf("register translated path: %w", err)
		}
		err = n.store.db.QueryRowContext(ctx, `SELECT alias, path FROM path_translation_names WHERE identity = ?`, id).Scan(&name.Alias, &storedPath)
	}
	if err != nil {
		return Name{}, err
	}
	if storedPath != path {
		return Name{}, errors.New("path translation identity collision")
	}
	return name, nil
}

func (n *Names) Resolve(ctx context.Context, alias string) (string, error) {
	var path string
	err := n.store.db.QueryRowContext(ctx, `SELECT path FROM path_translation_names WHERE alias = ?`, alias).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("unknown path translation alias")
	}
	if err != nil {
		return "", fmt.Errorf("resolve translated path: %w", err)
	}
	return path, nil
}

func IsAlias(value string) bool {
	if !strings.HasPrefix(value, AliasPrefix) || len(value) != len(AliasPrefix)+64 {
		return false
	}
	_, err := hex.DecodeString(value[len(AliasPrefix):])
	return err == nil
}
