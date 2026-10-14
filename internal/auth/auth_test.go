package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/store"
)

var cheap = &Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}

func newService(t *testing.T, now func() time.Time) (*Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := New(st, Options{Argon2: cheap, Now: now, SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func TestHashVerify(t *testing.T) {
	h, err := HashPassword("correct horse", *cheap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Errorf("hash format: %s", h)
	}
	if ok, err := VerifyPassword("correct horse", h); !ok || err != nil {
		t.Errorf("verify correct: %v, %v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong horse", h); ok {
		t.Error("verify wrong password succeeded")
	}
	h2, _ := HashPassword("correct horse", *cheap)
	if h == h2 {
		t.Error("salt not random")
	}
	for _, bad := range []string{"", "$argon2i$v=19$m=1,t=1,p=1$a$b", "$argon2id$v=19$m=x$a$b", "plain"} {
		if _, err := VerifyPassword("x", bad); err == nil {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
}

func TestDefaultParamsHash(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	h, err := HashPassword("pw-123456", DefaultArgon2Params)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifyPassword("pw-123456", h); !ok {
		t.Error("default params round trip failed")
	}
}

func TestCreateUserValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t, nil)
	if _, err := s.CreateUser(ctx, "dave", "longenough", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "Dave", "longenough", RoleMember); !errors.Is(err, ErrUserExists) {
		t.Errorf("duplicate: %v", err)
	}
	for _, tc := range []struct {
		user, pw string
		role     Role
	}{
		{"1bad", "longenough", RoleMember},
		{"has space", "longenough", RoleMember},
		{"ok", "short", RoleMember},
		{"ok", "longenough", "root"},
	} {
		if _, err := s.CreateUser(ctx, tc.user, tc.pw, tc.role); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("CreateUser(%q,%q,%q) = %v, want ErrInvalidInput", tc.user, tc.pw, tc.role, err)
		}
	}
	if ok, _ := s.HasUsers(ctx); !ok {
		t.Error("HasUsers = false")
	}
}

func TestLoginAuthenticateLogout(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s, st := newService(t, func() time.Time { return now })
	u, err := s.CreateUser(ctx, "dave", "longenough", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "dave", "wrongpass"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("bad password: %v", err)
	}
	if _, err := s.Login(ctx, "nobody", "longenough"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown user: %v", err)
	}
	sess, err := s.Login(ctx, "dave", "longenough")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sess.Token, TokenPrefix) || sess.User.ID != u.ID {
		t.Fatalf("session = %+v", sess)
	}
	// The raw token is never stored.
	var n int
	st.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE token_hash = ?`, []byte(sess.Token)).Scan(&n)
	if n != 0 {
		t.Error("raw token stored")
	}
	got, err := s.Authenticate(ctx, sess.Token)
	if err != nil || got.ID != u.ID || !got.IsAdmin() {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "bsn_nope", "garbage", sess.Token + "x"} {
		if _, err := s.Authenticate(ctx, bad); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) = %v", bad, err)
		}
	}
	if err := s.Logout(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, sess.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("after logout: %v", err)
	}
}

func TestExpiryAndDisabled(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s, st := newService(t, func() time.Time { return now })
	u, _ := s.CreateUser(ctx, "dave", "longenough", RoleMember)
	sess, _ := s.Login(ctx, "dave", "longenough")
	now = now.Add(2 * time.Hour)
	if _, err := s.Authenticate(ctx, sess.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired token accepted: %v", err)
	}
	sess, _ = s.Login(ctx, "dave", "longenough")
	st.SetUserDisabled(ctx, u.ID, true)
	if _, err := s.Authenticate(ctx, sess.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("disabled user accepted: %v", err)
	}
	if _, err := s.Login(ctx, "dave", "longenough"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("disabled user login: %v", err)
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t, nil)
	u, _ := s.CreateUser(ctx, "dave", "longenough", RoleMember)
	sess, _ := s.Login(ctx, "dave", "longenough")
	if err := s.ChangePassword(ctx, u.ID, "evenlonger"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, sess.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("old session survived password change")
	}
	if _, err := s.Login(ctx, "dave", "evenlonger"); err != nil {
		t.Errorf("login with new password: %v", err)
	}
}
