package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/store"
)

// Household user management: invites, role/disable changes, removal, and
// self-service password changes.

// More errors.
var (
	// ErrUserNotFound is returned for an unknown user ID.
	ErrUserNotFound = errors.New("auth: user not found")
	// ErrLastAdmin is returned when a change would leave no enabled admin.
	ErrLastAdmin = errors.New("auth: the last enabled admin cannot be demoted, disabled or removed")
	// ErrInvalidInvite is returned for an unknown, expired or used invite
	// code (deliberately indistinguishable).
	ErrInvalidInvite = errors.New("auth: invite code is invalid, expired or already used")
)

// InviteTTL is how long an invite code stays valid.
const InviteTTL = 24 * time.Hour

// InvitePrefix marks invite codes.
const InvitePrefix = "bsi_"

// UserInfo is a user as listed for admins.
type UserInfo struct {
	User
	Disabled    bool       `json:"disabled"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	// InviteExpiresAt is set while the user has an unredeemed invite.
	InviteExpiresAt *time.Time `json:"invite_expires_at,omitempty"`
}

// Invite is a freshly issued invite. Code is shown exactly once; only its
// hash is stored.
type Invite struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

func toUserInfo(u *store.User) *UserInfo {
	ui := &UserInfo{User: *toUser(u), Disabled: u.Disabled}
	if !u.LastLoginAt.IsZero() {
		t := u.LastLoginAt
		ui.LastLoginAt = &t
	}
	return ui
}

// NormalizeInviteCode trims surrounding space and lower-cases a code as
// typed by a person.
func NormalizeInviteCode(code string) string { return strings.ToLower(strings.TrimSpace(code)) }

func newInviteCode() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate invite code: %w", err)
	}
	return InvitePrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrUserNotFound, err)
	case errors.Is(err, store.ErrLastAdmin):
		return fmt.Errorf("%w: %w", ErrLastAdmin, err)
	}
	return err
}

// GetUserInfo returns one user.
func (s *Service) GetUserInfo(ctx context.Context, id int64) (*UserInfo, error) {
	u, err := s.st.GetUser(ctx, id)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return toUserInfo(u), nil
}

// ListUsers returns every user, ordered by ID.
func (s *Service) ListUsers(ctx context.Context) ([]*UserInfo, error) {
	us, err := s.st.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := s.st.PendingInvites(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*UserInfo, 0, len(us))
	for _, u := range us {
		ui := toUserInfo(u)
		if e, ok := pending[u.ID]; ok {
			ui.InviteExpiresAt = &e
		}
		out = append(out, ui)
	}
	return out, nil
}

// issueInvite stores a new invite for userID, replacing any earlier one.
func (s *Service) issueInvite(ctx context.Context, userID, createdBy int64) (*Invite, error) {
	code, err := newInviteCode()
	if err != nil {
		return nil, err
	}
	inv, err := s.st.ReplaceInvite(ctx, userID, createdBy, HashToken(code), s.now().Add(InviteTTL))
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &Invite{Code: code, ExpiresAt: inv.ExpiresAt}, nil
}

// unusablePasswordHash hashes a random password nobody knows, for accounts
// waiting on an invite (password_hash is NOT NULL, and a real hash keeps
// Login's timing and code path uniform).
func (s *Service) unusablePasswordHash() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return HashPassword(base32.StdEncoding.EncodeToString(b[:]), s.params)
}

// CreateInvitedUser creates a user without a known password and returns a
// one-time invite code with which they choose their own.
func (s *Service) CreateInvitedUser(ctx context.Context, username string, role Role, createdBy int64) (*User, *Invite, error) {
	if err := ValidateUsername(username); err != nil {
		return nil, nil, err
	}
	if !role.Valid() {
		return nil, nil, fmt.Errorf("%w: unknown role %q", ErrInvalidInput, role)
	}
	hash, err := s.unusablePasswordHash()
	if err != nil {
		return nil, nil, fmt.Errorf("create user: %w", err)
	}
	su, err := s.st.CreateUser(ctx, username, hash, string(role))
	if errors.Is(err, store.ErrConflict) {
		return nil, nil, fmt.Errorf("create user %q: %w", username, ErrUserExists)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("create user: %w", err)
	}
	inv, err := s.issueInvite(ctx, su.ID, createdBy)
	if err != nil {
		return nil, nil, fmt.Errorf("create user: %w", err)
	}
	return toUser(su), inv, nil
}

// UpdateUser changes a user's role and/or disabled flag (nil leaves a field
// unchanged). Disabling revokes the user's sessions and invites.
func (s *Service) UpdateUser(ctx context.Context, id int64, role *Role, disabled *bool) (*UserInfo, error) {
	var r *string
	if role != nil {
		if !role.Valid() {
			return nil, fmt.Errorf("%w: unknown role %q", ErrInvalidInput, *role)
		}
		rs := string(*role)
		r = &rs
	}
	u, err := s.st.UpdateUserAccess(ctx, id, r, disabled)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return toUserInfo(u), nil
}

// DeleteUser removes a user and everything they own (sessions, credentials,
// policy rules, conversations, memories, routines, channel links,
// approvals, invites). The audit log is kept.
func (s *Service) DeleteUser(ctx context.Context, id int64) error {
	return mapStoreErr(s.st.DeleteUser(ctx, id))
}

// ResetPassword replaces the user's password with an unknown one, revokes
// their sessions and returns a new invite code with which they choose a new
// password.
func (s *Service) ResetPassword(ctx context.Context, id, createdBy int64) (*Invite, error) {
	u, err := s.st.GetUser(ctx, id)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if u.Disabled {
		return nil, fmt.Errorf("%w: user is disabled; enable them first", ErrInvalidInput)
	}
	hash, err := s.unusablePasswordHash()
	if err != nil {
		return nil, fmt.Errorf("reset password: %w", err)
	}
	if err := s.st.UpdateUserPassword(ctx, id, hash); err != nil {
		return nil, mapStoreErr(err)
	}
	if err := s.st.DeleteUserSessions(ctx, id); err != nil {
		return nil, fmt.Errorf("reset password: %w", err)
	}
	return s.issueInvite(ctx, id, createdBy)
}

// AcceptInvite redeems an invite code: it sets the user's password, revokes
// any other sessions and invites, and logs them in. username, if not empty,
// must match the invited user.
func (s *Service) AcceptInvite(ctx context.Context, code, username, password, label string) (*Session, error) {
	code = NormalizeInviteCode(code)
	if !strings.HasPrefix(code, InvitePrefix) {
		return nil, ErrInvalidInvite
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	// Look the code up before hashing so that guesses cost no argon2 work.
	inv, err := s.st.GetLiveInvite(ctx, HashToken(code))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidInvite
	}
	if err != nil {
		return nil, fmt.Errorf("accept invite: %w", err)
	}
	su, err := s.st.GetUser(ctx, inv.UserID)
	if err != nil {
		return nil, fmt.Errorf("accept invite: %w", err)
	}
	if su.Disabled {
		return nil, ErrInvalidInvite
	}
	if username != "" && !strings.EqualFold(username, su.Username) {
		return nil, fmt.Errorf("%w: this invite is for a different username", ErrInvalidInput)
	}
	hash, err := HashPassword(password, s.params)
	if err != nil {
		return nil, fmt.Errorf("accept invite: %w", err)
	}
	uid, err := s.st.RedeemInvite(ctx, HashToken(code), hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidInvite // used concurrently or expired meanwhile
	}
	if err != nil {
		return nil, fmt.Errorf("accept invite: %w", err)
	}
	return s.newSession(ctx, uid, label)
}

// newSession issues a session token for userID.
func (s *Service) newSession(ctx context.Context, userID int64, label string) (*Session, error) {
	su, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	token, err := newToken()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	expires := s.now().Add(s.ttl)
	if _, err := s.st.CreateSession(ctx, su.ID, HashToken(token), label, expires); err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	_ = s.st.TouchUserLogin(ctx, su.ID) // best effort
	return &Session{Token: token, ExpiresAt: expires.UTC(), User: toUser(su)}, nil
}

// ChangeOwnPassword verifies the current password, sets a new one and
// revokes every session of the user except the one for keepToken.
func (s *Service) ChangeOwnPassword(ctx context.Context, userID int64, current, newPassword, keepToken string) error {
	su, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return mapStoreErr(err)
	}
	ok, err := VerifyPassword(current, su.PasswordHash)
	if err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}
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
	if err := s.st.DeleteUserSessionsExcept(ctx, userID, HashToken(keepToken)); err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	return nil
}
