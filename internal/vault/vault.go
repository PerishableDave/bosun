// Package vault is Bosun's credential vault: integration secrets encrypted at
// rest with AES-256-GCM under a daemon master key, stored per user (or
// shared) in the credentials table.
//
// Integrations never use the vault directly: they receive an
// integration.CredentialSource from SourceFor, which resolves the calling
// user from the context the dispatcher sets.
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/store"
)

// KeySize is the master key length (AES-256).
const KeySize = 32

// KeySource yields the 32-byte master key: FileKeySource (a mode-0600 file),
// KeychainKeySource (the OS keychain), or StaticKeySource. SelectKeySource
// chooses between the first two from config daemon.master_key.
type KeySource interface {
	Key(ctx context.Context) ([]byte, error)
}

// MasterKeyFile is the key file name inside the data dir.
const MasterKeyFile = "master.key"

// FileKeySource keeps the master key in a mode-0600 file, creating it with
// 32 random bytes on first use. The key is cached after the first read.
type FileKeySource struct {
	Path string

	mu  sync.Mutex
	key []byte
}

// NewFileKeySource returns a FileKeySource for $dataDir/master.key.
func NewFileKeySource(dataDir string) *FileKeySource {
	return &FileKeySource{Path: filepath.Join(dataDir, MasterKeyFile)}
}

// Key implements KeySource. It refuses a key file that is group- or
// world-accessible or has the wrong length.
func (f *FileKeySource) Key(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.key != nil {
		return f.key, nil
	}
	key, err := f.load()
	if errors.Is(err, os.ErrNotExist) {
		key, err = f.create()
		if errors.Is(err, os.ErrExist) { // lost a creation race with another process
			key, err = f.load()
		}
	}
	if err != nil {
		return nil, err
	}
	f.key = key
	return key, nil
}

func (f *FileKeySource) load() ([]byte, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, fmt.Errorf("vault: open master key: %w", err)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, fmt.Errorf("vault: stat master key: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("vault: master key %s has insecure permissions %v (want 0600)", f.Path, st.Mode().Perm())
	}
	key := make([]byte, KeySize+1)
	n, err := io.ReadFull(fh, key)
	if n != KeySize || !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("vault: master key %s must be exactly %d bytes", f.Path, KeySize)
	}
	return key[:KeySize], nil
}

func (f *FileKeySource) create() ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return nil, fmt.Errorf("vault: create key dir: %w", err)
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("vault: generate master key: %w", err)
	}
	fh, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vault: create master key: %w", err)
	}
	if _, err := fh.Write(key); err != nil {
		fh.Close()
		os.Remove(f.Path)
		return nil, fmt.Errorf("vault: write master key: %w", err)
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		os.Remove(f.Path)
		return nil, fmt.Errorf("vault: sync master key: %w", err)
	}
	if err := fh.Close(); err != nil {
		return nil, fmt.Errorf("vault: close master key: %w", err)
	}
	return key, nil
}

// StaticKeySource is a fixed key (tests, or a key supplied by another
// mechanism).
type StaticKeySource []byte

// Key implements KeySource.
func (s StaticKeySource) Key(context.Context) ([]byte, error) {
	if len(s) != KeySize {
		return nil, fmt.Errorf("vault: static key must be %d bytes", KeySize)
	}
	return s, nil
}

// Store is the subset of *store.Store the vault needs.
type Store interface {
	UpsertCredential(ctx context.Context, c store.Credential) (*store.Credential, error)
	GetCredential(ctx context.Context, userID *int64, integration, kind string) (*store.Credential, error)
	DeleteCredential(ctx context.Context, userID *int64, integration, kind string) error
	ListCredentials(ctx context.Context, userID *int64, integration string) ([]*store.Credential, error)
}

var _ Store = (*store.Store)(nil)

// ErrNotFound means no credential matched.
var ErrNotFound = errors.New("vault: credential not found")

// Vault encrypts and stores credentials.
type Vault struct {
	st   Store
	keys KeySource

	resMu     sync.RWMutex
	resolvers []prefixResolver
}

// KindResolver resolves a credential kind for any integration, instead of
// the integration's own vault row. internal/oauth registers one for the
// "oauth2:" prefix so every integration asking for "oauth2:google" gets the
// calling user's linked Google account (with a fresh access token).
//
// owner is the user the source resolved from ctx (nil for a ScopeShared
// integration's source). Resolve returns an error wrapping
// integration.ErrNoCredential when the owner has nothing for kind.
type KindResolver interface {
	Resolve(ctx context.Context, owner *int64, kind string) (integration.Credential, error)
}

type prefixResolver struct {
	prefix string
	r      KindResolver
}

// RegisterResolver routes every SourceFor lookup of a kind starting with
// prefix (e.g. "oauth2:") to r. Later registrations for the same prefix
// replace earlier ones. Register before integrations are initialised.
func (v *Vault) RegisterResolver(prefix string, r KindResolver) {
	v.resMu.Lock()
	defer v.resMu.Unlock()
	for i := range v.resolvers {
		if v.resolvers[i].prefix == prefix {
			v.resolvers[i].r = r
			return
		}
	}
	v.resolvers = append(v.resolvers, prefixResolver{prefix: prefix, r: r})
}

// resolverFor returns the registered resolver whose prefix kind has (the
// longest match), or nil.
func (v *Vault) resolverFor(kind string) KindResolver {
	v.resMu.RLock()
	defer v.resMu.RUnlock()
	var best prefixResolver
	for _, pr := range v.resolvers {
		if strings.HasPrefix(kind, pr.prefix) && len(pr.prefix) >= len(best.prefix) {
			best = pr
		}
	}
	return best.r
}

// New returns a Vault.
func New(st Store, keys KeySource) *Vault { return &Vault{st: st, keys: keys} }

// Info describes a stored credential without its secret.
type Info struct {
	UserID      *int64 // nil = shared
	Integration string
	Kind        string
	Expiry      time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// payload is the plaintext that gets encrypted.
type payload struct {
	Secret string            `json:"secret"`
	Fields map[string]string `json:"fields,omitempty"`
	Expiry time.Time         `json:"expiry,omitzero"`
}

// aad binds a ciphertext to its row identity, so a ciphertext copied into
// another user's (or integration's, or kind's) row fails to decrypt.
func aad(userID *int64, integrationName, kind string) []byte {
	owner := "shared"
	if userID != nil {
		owner = "user:" + strconv.FormatInt(*userID, 10)
	}
	b, _ := json.Marshal([]string{"bosun-credential-v1", owner, integrationName, kind})
	return b
}

func (v *Vault) aead(ctx context.Context) (cipher.AEAD, error) {
	key, err := v.keys.Key(ctx)
	if err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("vault: master key must be %d bytes", KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	return g, nil
}

func checkOwner(userID *int64) error {
	if userID != nil && *userID <= 0 {
		return fmt.Errorf("vault: invalid user id %d", *userID)
	}
	return nil
}

// Put encrypts and stores cred for (userID, integrationName, kind),
// replacing any existing one. userID nil stores a shared credential. If kind
// is empty, cred.Kind is used.
func (v *Vault) Put(ctx context.Context, userID *int64, integrationName, kind string, cred integration.Credential) error {
	if kind == "" {
		kind = cred.Kind
	}
	if integrationName == "" || kind == "" {
		return fmt.Errorf("vault: put: integration and kind are required")
	}
	if err := checkOwner(userID); err != nil {
		return err
	}
	g, err := v.aead(ctx)
	if err != nil {
		return fmt.Errorf("vault: put: %w", err)
	}
	pt, err := json.Marshal(payload{Secret: cred.Secret, Fields: cred.Fields, Expiry: cred.Expiry})
	if err != nil {
		return fmt.Errorf("vault: put: %w", err)
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("vault: put: nonce: %w", err)
	}
	ct := g.Seal(nil, nonce, pt, aad(userID, integrationName, kind))
	clear(pt)
	if _, err := v.st.UpsertCredential(ctx, store.Credential{
		UserID: userID, Integration: integrationName, Kind: kind, Nonce: nonce, Ciphertext: ct, ExpiresAt: cred.Expiry,
	}); err != nil {
		return fmt.Errorf("vault: put: %w", err)
	}
	return nil
}

// Get decrypts the credential for (userID, integrationName, kind). Returns
// ErrNotFound if absent.
func (v *Vault) Get(ctx context.Context, userID *int64, integrationName, kind string) (integration.Credential, error) {
	if err := checkOwner(userID); err != nil {
		return integration.Credential{}, err
	}
	row, err := v.st.GetCredential(ctx, userID, integrationName, kind)
	if errors.Is(err, store.ErrNotFound) {
		return integration.Credential{}, ErrNotFound
	}
	if err != nil {
		return integration.Credential{}, fmt.Errorf("vault: get: %w", err)
	}
	g, err := v.aead(ctx)
	if err != nil {
		return integration.Credential{}, fmt.Errorf("vault: get: %w", err)
	}
	if len(row.Nonce) != g.NonceSize() {
		return integration.Credential{}, fmt.Errorf("vault: get %s/%s: corrupt nonce", integrationName, kind)
	}
	pt, err := g.Open(nil, row.Nonce, row.Ciphertext, aad(userID, integrationName, kind))
	if err != nil {
		return integration.Credential{}, fmt.Errorf("vault: get %s/%s: decrypt failed (wrong key or tampered row)", integrationName, kind)
	}
	defer clear(pt)
	var p payload
	if err := json.Unmarshal(pt, &p); err != nil {
		return integration.Credential{}, fmt.Errorf("vault: get %s/%s: decode: %w", integrationName, kind, err)
	}
	return integration.Credential{Kind: kind, Secret: p.Secret, Fields: p.Fields, Expiry: p.Expiry}, nil
}

// Delete removes the credential. Returns ErrNotFound if absent.
func (v *Vault) Delete(ctx context.Context, userID *int64, integrationName, kind string) error {
	err := v.st.DeleteCredential(ctx, userID, integrationName, kind)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("vault: delete: %w", err)
	}
	return nil
}

// List returns metadata (never secrets) for the credentials owned by userID
// (nil = shared), optionally limited to one integration ("" = all).
func (v *Vault) List(ctx context.Context, userID *int64, integrationName string) ([]Info, error) {
	rows, err := v.st.ListCredentials(ctx, userID, integrationName)
	if err != nil {
		return nil, fmt.Errorf("vault: list: %w", err)
	}
	out := make([]Info, 0, len(rows))
	for _, r := range rows {
		out = append(out, Info{UserID: r.UserID, Integration: r.Integration, Kind: r.Kind, Expiry: r.ExpiresAt,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// SourceFor returns the CredentialSource handed to an integration's Init.
//
//   - ScopePerUser: Get reads the calling user from ctx
//     (integration.UserIDFromContext, set only by the dispatcher) and returns
//     that user's credential; with no user in ctx it returns ErrNoCredential.
//     The integration never chooses the user, so it cannot read another
//     user's secrets.
//   - ScopeShared: Get returns the shared (user_id NULL) credential
//     regardless of the caller.
//
// Any other scope yields a source that always returns ErrNoCredential.
//
// Kinds matching a resolver registered with RegisterResolver (e.g.
// "oauth2:google") are answered by that resolver for the same owner, not
// from the integration's own row.
func (v *Vault) SourceFor(integrationName string, scope integration.Scope) integration.CredentialSource {
	return &source{v: v, integ: integrationName, scope: scope}
}

type source struct {
	v     *Vault
	integ string
	scope integration.Scope
}

var _ integration.CredentialKindLister = (*source)(nil)

// owner returns the vault owner for ctx: the calling user for per-user
// integrations (an error wrapping ErrNoCredential without one), nil for
// shared ones.
func (s *source) owner(ctx context.Context, kind string) (*int64, error) {
	switch s.scope {
	case integration.ScopePerUser:
		uid, ok := integration.UserIDFromContext(ctx)
		if !ok || uid <= 0 {
			return nil, fmt.Errorf("%s/%s: no calling user: %w", s.integ, kind, integration.ErrNoCredential)
		}
		return &uid, nil
	case integration.ScopeShared:
		return nil, nil
	default:
		return nil, fmt.Errorf("%s: invalid scope %q: %w", s.integ, s.scope, integration.ErrNoCredential)
	}
}

// Kinds implements integration.CredentialKindLister: the kinds stored in the
// integration's own rows for the owner (resolver kinds are not listed).
func (s *source) Kinds(ctx context.Context) ([]string, error) {
	owner, err := s.owner(ctx, "*")
	if err != nil {
		return nil, err
	}
	infos, err := s.v.List(ctx, owner, s.integ)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, in := range infos {
		out = append(out, in.Kind)
	}
	sort.Strings(out)
	return out, nil
}

func (s *source) Get(ctx context.Context, kind string) (integration.Credential, error) {
	owner, err := s.owner(ctx, kind)
	if err != nil {
		return integration.Credential{}, err
	}
	if r := s.v.resolverFor(kind); r != nil {
		return r.Resolve(ctx, owner, kind)
	}
	c, err := s.v.Get(ctx, owner, s.integ, kind)
	if errors.Is(err, ErrNotFound) {
		return integration.Credential{}, fmt.Errorf("%s/%s: %w", s.integ, kind, integration.ErrNoCredential)
	}
	return c, err
}
