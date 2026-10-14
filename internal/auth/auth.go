// Package auth manages users, roles, passwords (argon2id), and bearer session
// tokens. Tokens are random and only their SHA-256 hash is stored.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/perishabledave/bosun/internal/store"
)

// Role is a user's role.
type Role string

// Roles.
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleMember }

// User is an authenticated principal. It never carries the password hash.
type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// IsAdmin reports whether the user has the admin role.
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// Errors.
var (
	// ErrInvalidCredentials is returned by Login for any bad username/password
	// combination (deliberately indistinguishable).
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	// ErrUnauthenticated is returned by Authenticate for missing, unknown, or
	// expired tokens and disabled users.
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	// ErrUserExists is returned by CreateUser when the username is taken.
	ErrUserExists = errors.New("auth: user already exists")
	// ErrInvalidInput is returned for bad usernames, passwords, or roles.
	ErrInvalidInput = errors.New("auth: invalid input")
)

// TokenPrefix marks Bosun session tokens so they are recognisable in logs and
// secret scanners.
const TokenPrefix = "bsn_"

// MinPasswordLen is the minimum accepted password length (in characters).
const MinPasswordLen = 8

var usernameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$`)

// Options configure a Service.
type Options struct {
	// SessionTTL is how long a session token is valid. Default 30 days.
	SessionTTL time.Duration
	// Argon2 overrides the hashing cost (tests use cheap parameters).
	Argon2 *Argon2Params
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Service implements user management and authentication on top of the store.
type Service struct {
	st        *store.Store
	ttl       time.Duration
	params    Argon2Params
	now       func() time.Time
	dummyHash string // used to equalise timing for unknown usernames
}

// New returns a Service.
func New(st *store.Store, opts Options) (*Service, error) {
	s := &Service{st: st, ttl: opts.SessionTTL, params: DefaultArgon2Params, now: opts.Now}
	if s.ttl <= 0 {
		s.ttl = 30 * 24 * time.Hour
	}
	if opts.Argon2 != nil {
		s.params = *opts.Argon2
	}
	if s.now == nil {
		s.now = time.Now
	}
	h, err := HashPassword("bosun-dummy-password", s.params)
	if err != nil {
		return nil, fmt.Errorf("auth: init: %w", err)
	}
	s.dummyHash = h
	return s, nil
}

func toUser(u *store.User) *User {
	return &User{ID: u.ID, Username: u.Username, Role: Role(u.Role), CreatedAt: u.CreatedAt}
}

// ValidateUsername checks username syntax.
func ValidateUsername(username string) error {
	if !usernameRe.MatchString(username) {
		return fmt.Errorf("%w: username must start with a letter and contain only letters, digits, '.', '_' or '-' (max 64)", ErrInvalidInput)
	}
	return nil
}

// ValidatePassword checks password strength requirements.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return fmt.Errorf("%w: password must be at least %d characters", ErrInvalidInput, MinPasswordLen)
	}
	if len(password) > 1024 {
		return fmt.Errorf("%w: password too long", ErrInvalidInput)
	}
	return nil
}

// CreateUser creates a user with the given role.
func (s *Service) CreateUser(ctx context.Context, username, password string, role Role) (*User, error) {
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	if !role.Valid() {
		return nil, fmt.Errorf("%w: unknown role %q", ErrInvalidInput, role)
	}
	hash, err := HashPassword(password, s.params)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	u, err := s.st.CreateUser(ctx, username, hash, string(role))
	if errors.Is(err, store.ErrConflict) {
		return nil, fmt.Errorf("create user %q: %w", username, ErrUserExists)
	}
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return toUser(u), nil
}

// Session is the result of a successful Login. Token is shown to the client
// exactly once.
type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      *User     `json:"user"`
}

// Login verifies a username/password and issues a new session token.
func (s *Service) Login(ctx context.Context, username, password string) (*Session, error) {
	return s.LoginWithLabel(ctx, username, password, "")
}

// LoginWithLabel is Login with a free-form client description stored on the
// session (e.g. "cli", "web").
func (s *Service) LoginWithLabel(ctx context.Context, username, password, label string) (*Session, error) {
	su, err := s.st.GetUserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		VerifyPassword(password, s.dummyHash) // equalise timing
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	ok, err := VerifyPassword(password, su.PasswordHash)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if !ok || su.Disabled {
		return nil, ErrInvalidCredentials
	}
	token, err := newToken()
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	expires := s.now().Add(s.ttl)
	if _, err := s.st.CreateSession(ctx, su.ID, HashToken(token), label, expires); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	return &Session{Token: token, ExpiresAt: expires.UTC(), User: toUser(su)}, nil
}

// Authenticate resolves a bearer token to its user.
func (s *Service) Authenticate(ctx context.Context, token string) (*User, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return nil, ErrUnauthenticated
	}
	sess, err := s.st.GetSessionByTokenHash(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	if !s.now().Before(sess.ExpiresAt) {
		_ = s.st.DeleteSession(ctx, sess.ID)
		return nil, ErrUnauthenticated
	}
	su, err := s.st.GetUser(ctx, sess.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	if su.Disabled {
		return nil, ErrUnauthenticated
	}
	_ = s.st.TouchSession(ctx, sess.ID) // best effort
	return toUser(su), nil
}

// Logout revokes the session for token. Unknown tokens are not an error.
func (s *Service) Logout(ctx context.Context, token string) error {
	sess, err := s.st.GetSessionByTokenHash(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	if err := s.st.DeleteSession(ctx, sess.ID); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// ChangePassword sets a new password and revokes all of the user's sessions.
func (s *Service) ChangePassword(ctx context.Context, userID int64, newPassword string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	hash, err := HashPassword(newPassword, s.params)
	if err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	if err := s.st.UpdateUserPassword(ctx, userID, hash); err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	if err := s.st.DeleteUserSessions(ctx, userID); err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	return nil
}

// HasUsers reports whether any user exists (used by setup).
func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	n, err := s.st.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return n > 0, nil
}

// HashToken returns the SHA-256 of a session token. Tokens carry 256 bits of
// entropy, so a fast hash is sufficient.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}
