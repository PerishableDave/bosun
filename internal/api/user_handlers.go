package api

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/auth"
)

// UserAdmin is the subset of *auth.Service behind /v1/users, /v1/invites and
// /v1/me/password.
type UserAdmin interface {
	ListUsers(ctx context.Context) ([]*auth.UserInfo, error)
	GetUserInfo(ctx context.Context, id int64) (*auth.UserInfo, error)
	CreateUser(ctx context.Context, username, password string, role auth.Role) (*auth.User, error)
	CreateInvitedUser(ctx context.Context, username string, role auth.Role, createdBy int64) (*auth.User, *auth.Invite, error)
	UpdateUser(ctx context.Context, id int64, role *auth.Role, disabled *bool) (*auth.UserInfo, error)
	DeleteUser(ctx context.Context, id int64) error
	ResetPassword(ctx context.Context, id, createdBy int64) (*auth.Invite, error)
	AcceptInvite(ctx context.Context, code, username, password, label string) (*auth.Session, error)
	ChangeOwnPassword(ctx context.Context, userID int64, current, newPassword, keepToken string) error
}

var _ UserAdmin = (*auth.Service)(nil)

// InviteAcceptsPerMinute bounds POST /v1/invites/{code}/accept per client
// address (the endpoint is unauthenticated).
const InviteAcceptsPerMinute = 10

// UserListResponse is the body of GET /v1/users.
type UserListResponse struct {
	Users []*auth.UserInfo `json:"users"`
}

// CreateUserRequest is the body of POST /v1/users. Without a password the
// response carries a one-time invite code instead.
type CreateUserRequest struct {
	Username string `json:"username"`
	Role     string `json:"role,omitempty"` // default "member"
	Password string `json:"password,omitempty"`
}

// UserInviteResponse is the body of POST /v1/users and
// POST /v1/users/{id}/reset-password. Invite is omitted when a password was
// set directly.
type UserInviteResponse struct {
	User   *auth.UserInfo `json:"user"`
	Invite *auth.Invite   `json:"invite,omitempty"`
}

// UpdateUserRequest is the body of PUT /v1/users/{id}.
type UpdateUserRequest struct {
	Role     *string `json:"role,omitempty"`
	Disabled *bool   `json:"disabled,omitempty"`
}

// AcceptInviteRequest is the body of POST /v1/invites/{code}/accept.
type AcceptInviteRequest struct {
	Username string `json:"username,omitempty"` // optional; must match the invite
	Password string `json:"password"`
	Label    string `json:"label,omitempty"`
}

// ChangePasswordRequest is the body of PUT /v1/me/password.
type ChangePasswordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

// requireAdmin writes 403 unless the caller is an admin, and 501 when user
// management is not wired.
func (s *Server) requireUserAdmin(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	u, _ := UserFromContext(r.Context())
	if !u.IsAdmin() {
		writeError(w, http.StatusForbidden, "forbidden", "admin only")
		return nil, false
	}
	if s.users == nil {
		notImplemented(w)
		return nil, false
	}
	return u, true
}

func (s *Server) userError(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "not_found", "user not found")
	case errors.Is(err, auth.ErrUserExists):
		writeError(w, http.StatusConflict, "conflict", "a user with that name already exists")
	case errors.Is(err, auth.ErrLastAdmin):
		writeError(w, http.StatusConflict, "last_admin", "the last enabled admin cannot be demoted, disabled or removed")
	case errors.Is(err, auth.ErrInvalidInvite):
		writeError(w, http.StatusNotFound, "invalid_invite", "invite code is invalid, expired or already used")
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusForbidden, "invalid_credentials", "current password is wrong")
	case errors.Is(err, auth.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "bad_request", strings.TrimPrefix(err.Error(), auth.ErrInvalidInput.Error()+": "))
	default:
		s.internalError(w, r, what, err)
	}
}

func pathUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return 0, false
	}
	return id, true
}

// recordUser audits a user-management action by actor. Passwords and
// invite codes are never recorded.
func (s *Server) recordUser(ctx context.Context, actor int64, note string, target *auth.User, extra map[string]any) {
	if s.audit == nil {
		return
	}
	d := map[string]any{}
	if target != nil {
		d["target_user_id"] = target.ID
		d["username"] = target.Username
	}
	for k, v := range extra {
		d[k] = v
	}
	detail, _ := json.Marshal(d)
	e := audit.Entry{UserID: actor, Kind: audit.KindAuth, Notes: []string{note}, Detail: detail}
	if err := s.audit.Record(context.WithoutCancel(ctx), e); err != nil {
		s.log.ErrorContext(ctx, "audit user change", "err", err)
	}
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUserAdmin(w, r); !ok {
		return
	}
	us, err := s.users.ListUsers(r.Context())
	if err != nil {
		s.internalError(w, r, "list users", err)
		return
	}
	if us == nil {
		us = []*auth.UserInfo{}
	}
	writeJSON(w, http.StatusOK, UserListResponse{Users: us})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireUserAdmin(w, r)
	if !ok {
		return
	}
	var req CreateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	role := auth.Role(strings.ToLower(req.Role))
	if req.Role == "" {
		role = auth.RoleMember
	}
	if !role.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", "role must be admin or member")
		return
	}
	var (
		u   *auth.User
		inv *auth.Invite
		err error
	)
	if req.Password != "" {
		u, err = s.users.CreateUser(r.Context(), req.Username, req.Password, role)
	} else {
		u, inv, err = s.users.CreateInvitedUser(r.Context(), req.Username, role, admin.ID)
	}
	req.Password = ""
	if err != nil {
		s.userError(w, r, "create user", err)
		return
	}
	s.recordUser(r.Context(), admin.ID, "user created", u, map[string]any{"role": u.Role, "invite": inv != nil})
	info, err := s.users.GetUserInfo(r.Context(), u.ID)
	if err != nil {
		s.userError(w, r, "create user", err)
		return
	}
	if inv != nil {
		info.InviteExpiresAt = &inv.ExpiresAt
	}
	writeJSON(w, http.StatusCreated, UserInviteResponse{User: info, Invite: inv})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireUserAdmin(w, r)
	if !ok {
		return
	}
	id, ok := pathUserID(w, r)
	if !ok {
		return
	}
	var req UpdateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role == nil && req.Disabled == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "nothing to change: give role and/or disabled")
		return
	}
	var role *auth.Role
	if req.Role != nil {
		rr := auth.Role(strings.ToLower(*req.Role))
		if !rr.Valid() {
			writeError(w, http.StatusBadRequest, "bad_request", "role must be admin or member")
			return
		}
		role = &rr
	}
	before, err := s.users.GetUserInfo(r.Context(), id)
	if err != nil {
		s.userError(w, r, "update user", err)
		return
	}
	info, err := s.users.UpdateUser(r.Context(), id, role, req.Disabled)
	if err != nil {
		s.userError(w, r, "update user", err)
		return
	}
	extra := map[string]any{}
	if before.Role != info.Role {
		extra["role"] = info.Role
	}
	if before.Disabled != info.Disabled {
		extra["disabled"] = info.Disabled
	}
	if len(extra) > 0 {
		s.recordUser(r.Context(), admin.ID, "user updated", &info.User, extra)
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireUserAdmin(w, r)
	if !ok {
		return
	}
	id, ok := pathUserID(w, r)
	if !ok {
		return
	}
	target, err := s.users.GetUserInfo(r.Context(), id)
	if err != nil {
		s.userError(w, r, "delete user", err)
		return
	}
	if err := s.users.DeleteUser(r.Context(), id); err != nil {
		s.userError(w, r, "delete user", err)
		return
	}
	s.recordUser(r.Context(), admin.ID, "user deleted", &target.User, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireUserAdmin(w, r)
	if !ok {
		return
	}
	id, ok := pathUserID(w, r)
	if !ok {
		return
	}
	inv, err := s.users.ResetPassword(r.Context(), id, admin.ID)
	if err != nil {
		s.userError(w, r, "reset password", err)
		return
	}
	info, err := s.users.GetUserInfo(r.Context(), id)
	if err != nil {
		s.userError(w, r, "reset password", err)
		return
	}
	info.InviteExpiresAt = &inv.ExpiresAt
	s.recordUser(r.Context(), admin.ID, "password reset", &info.User, nil)
	writeJSON(w, http.StatusOK, UserInviteResponse{User: info, Invite: inv})
}

// rateLimitKey identifies a client address for rate limiting, using the
// proxy-aware client IP when the proxy middleware has resolved one.
func rateLimitKey(r *http.Request) int64 {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if c, ok := ClientFromContext(r.Context()); ok && c.IP != "" {
		host = c.IP
	}
	h := fnv.New64a()
	h.Write([]byte(host))
	return int64(h.Sum64() >> 1)
}

// handleAcceptInvite is unauthenticated: the one-time code identifies the
// user.
func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		notImplemented(w)
		return
	}
	if ok, wait := s.inviteAccepts.allow(rateLimitKey(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts; try again shortly")
		return
	}
	var req AcceptInviteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "password is required")
		return
	}
	if len(req.Label) > 100 {
		req.Label = req.Label[:100]
	}
	sess, err := s.users.AcceptInvite(r.Context(), r.PathValue("code"), req.Username, req.Password, req.Label)
	req.Password = ""
	if err != nil {
		s.userError(w, r, "accept invite", err)
		return
	}
	s.recordUser(r.Context(), sess.User.ID, "invite accepted", sess.User, nil)
	writeJSON(w, http.StatusOK, LoginResponse{Token: sess.Token, ExpiresAt: sess.ExpiresAt, User: sess.User})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	var req ChangePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Current == "" || req.New == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "current and new are required")
		return
	}
	err := s.users.ChangeOwnPassword(r.Context(), u.ID, req.Current, req.New, bearerToken(r))
	req.Current, req.New = "", ""
	if err != nil {
		s.userError(w, r, "change password", err)
		return
	}
	s.recordUser(r.Context(), u.ID, "password changed", u, nil)
	w.WriteHeader(http.StatusNoContent)
}
