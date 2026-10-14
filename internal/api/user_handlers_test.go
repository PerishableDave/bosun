package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/store"
)

type usersEnv struct {
	*env
	st    *store.Store
	audit *memAudit
	admin string // admin token

	mu  sync.Mutex
	now time.Time
}

func (u *usersEnv) clock() time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.now
}

func (u *usersEnv) advance(d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.now = u.now.Add(d)
}

func setupUsers(t *testing.T) *usersEnv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ue := &usersEnv{st: st, audit: &memAudit{}, now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	st.SetClock(ue.clock)
	a, err := auth.New(st, auth.Options{Now: ue.clock, Argon2: &auth.Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.CreateUser(ctx, "dave", "longenough", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	s, err := New(Options{Auth: a, Bus: bus, Version: "test", Users: a, Audit: ue.audit})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	ue.env = &env{srv: srv, auth: a, bus: bus, user: u}
	ue.admin = ue.login(t)
	return ue
}

func decode[T any](t *testing.T, e *env, method, path, token, body string, want int) T {
	t.Helper()
	resp := e.do(t, method, path, token, body)
	var v T
	if resp.StatusCode != want {
		var eb ErrorBody
		json.NewDecoder(resp.Body).Decode(&eb)
		t.Fatalf("%s %s = %d (%s: %s), want %d", method, path, resp.StatusCode, eb.Error.Code, eb.Error.Message, want)
	}
	if want != 204 {
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return v
}

func (u *usersEnv) invite(t *testing.T, username, role string) UserInviteResponse {
	t.Helper()
	r := decode[UserInviteResponse](t, u.env, "POST", "/v1/users", u.admin, fmt.Sprintf(`{"username":%q,"role":%q}`, username, role), 201)
	if r.Invite == nil || !strings.HasPrefix(r.Invite.Code, auth.InvitePrefix) || r.User.Username != username {
		t.Fatalf("create user response %+v", r)
	}
	return r
}

func (u *usersEnv) accept(t *testing.T, code, password string) LoginResponse {
	t.Helper()
	return decode[LoginResponse](t, u.env, "POST", "/v1/invites/"+code+"/accept", "", fmt.Sprintf(`{"password":%q}`, password), 200)
}

func TestInviteFlow(t *testing.T) {
	u := setupUsers(t)
	r := u.invite(t, "alice", "member")
	if !r.Invite.ExpiresAt.Equal(u.clock().Add(auth.InviteTTL)) || r.User.InviteExpiresAt == nil {
		t.Errorf("invite expiry = %v, user %+v", r.Invite.ExpiresAt, r.User)
	}
	code := r.Invite.Code

	// Invited user cannot log in yet.
	if resp := u.do(t, "POST", "/v1/login", "", `{"username":"alice","password":"whatever-pw"}`); resp.StatusCode != 401 {
		t.Errorf("login before accept = %d", resp.StatusCode)
	}
	// Short password: rejected without consuming the code.
	if resp := u.do(t, "POST", "/v1/invites/"+code+"/accept", "", `{"password":"short"}`); resp.StatusCode != 400 {
		t.Errorf("short password = %d", resp.StatusCode)
	}
	// Wrong username for the invite.
	if resp := u.do(t, "POST", "/v1/invites/"+code+"/accept", "", `{"username":"bob","password":"alicepass1"}`); resp.StatusCode != 400 {
		t.Errorf("wrong username = %d", resp.StatusCode)
	}
	// Codes are case-insensitive and trimmed.
	lr := decode[LoginResponse](t, u.env, "POST", "/v1/invites/"+strings.ToUpper(code)+"/accept", "",
		`{"username":"Alice","password":"alicepass1","label":"cli"}`, 200)
	if lr.Token == "" || lr.User.Username != "alice" || lr.User.Role != auth.RoleMember {
		t.Fatalf("accept = %+v", lr)
	}
	me := decode[auth.User](t, u.env, "GET", "/v1/me", lr.Token, "", 200)
	if me.Username != "alice" {
		t.Errorf("me = %+v", me)
	}
	// Single use.
	if resp := u.do(t, "POST", "/v1/invites/"+code+"/accept", "", `{"password":"otherpass1"}`); resp.StatusCode != 404 || errCode(t, resp) != "invalid_invite" {
		t.Errorf("second accept = %d", resp.StatusCode)
	}
	// Password login now works.
	if resp := u.do(t, "POST", "/v1/login", "", `{"username":"alice","password":"alicepass1"}`); resp.StatusCode != 200 {
		t.Errorf("login after accept = %d", resp.StatusCode)
	}

	// Members cannot manage users.
	for _, c := range [][2]string{{"GET", "/v1/users"}, {"POST", "/v1/users"}, {"PUT", "/v1/users/1"}, {"DELETE", "/v1/users/1"}, {"POST", "/v1/users/1/reset-password"}} {
		if resp := u.do(t, c[0], c[1], lr.Token, `{}`); resp.StatusCode != 403 {
			t.Errorf("member %s %s = %d", c[0], c[1], resp.StatusCode)
		}
	}

	list := decode[UserListResponse](t, u.env, "GET", "/v1/users", u.admin, "", 200)
	if len(list.Users) != 2 || list.Users[1].Username != "alice" || list.Users[1].InviteExpiresAt != nil || list.Users[1].LastLoginAt == nil || list.Users[0].Disabled {
		t.Errorf("list = %+v %+v", list.Users[0], list.Users[1])
	}

	// Duplicate and invalid input.
	if resp := u.do(t, "POST", "/v1/users", u.admin, `{"username":"ALICE"}`); resp.StatusCode != 409 {
		t.Errorf("duplicate = %d", resp.StatusCode)
	}
	if resp := u.do(t, "POST", "/v1/users", u.admin, `{"username":"carol","role":"root"}`); resp.StatusCode != 400 {
		t.Errorf("bad role = %d", resp.StatusCode)
	}
	if resp := u.do(t, "POST", "/v1/users", u.admin, `{"username":"9lives"}`); resp.StatusCode != 400 {
		t.Errorf("bad username = %d", resp.StatusCode)
	}
	// With a password: no invite.
	cr := decode[UserInviteResponse](t, u.env, "POST", "/v1/users", u.admin, `{"username":"carol","password":"carolpass1"}`, 201)
	if cr.Invite != nil || cr.User.Role != auth.RoleMember {
		t.Errorf("create with password = %+v", cr)
	}

	// Audit: auth entries by the acting user; never a password or code.
	u.audit.mu.Lock()
	defer u.audit.mu.Unlock()
	var notes []string
	for _, e := range u.audit.entries {
		if e.Kind != "auth" {
			t.Errorf("audit kind %q", e.Kind)
		}
		b, _ := json.Marshal(e)
		for _, secret := range []string{code, strings.ToUpper(code), "alicepass1", "carolpass1"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("audit entry leaks a secret: %s", b)
			}
		}
		notes = append(notes, fmt.Sprintf("%d:%s", e.UserID, e.Notes[0]))
	}
	want := fmt.Sprintf("1:user created|%d:invite accepted|1:user created", lr.User.ID)
	if got := strings.Join(notes, "|"); got != want {
		t.Errorf("audit = %q, want %q", got, want)
	}
}

func TestInviteExpiry(t *testing.T) {
	u := setupUsers(t)
	r := u.invite(t, "alice", "member")
	u.advance(auth.InviteTTL + time.Second)
	if resp := u.do(t, "POST", "/v1/invites/"+r.Invite.Code+"/accept", "", `{"password":"alicepass1"}`); resp.StatusCode != 404 {
		t.Errorf("expired accept = %d", resp.StatusCode)
	}
	list := decode[UserListResponse](t, u.env, "GET", "/v1/users", u.admin, "", 200)
	if list.Users[1].InviteExpiresAt != nil {
		t.Errorf("expired invite still pending: %+v", list.Users[1])
	}
	// The admin issues a fresh code.
	rr := decode[UserInviteResponse](t, u.env, "POST", fmt.Sprintf("/v1/users/%d/reset-password", r.User.ID), u.admin, "", 200)
	if rr.Invite == nil || rr.Invite.Code == r.Invite.Code {
		t.Fatalf("reset = %+v", rr)
	}
	u.accept(t, rr.Invite.Code, "alicepass1")
	if resp := u.do(t, "POST", "/v1/invites/bogus/accept", "", `{"password":"alicepass1"}`); resp.StatusCode != 404 {
		t.Errorf("bogus code = %d", resp.StatusCode)
	}
}

func TestResetPasswordRevokesAndReplaces(t *testing.T) {
	u := setupUsers(t)
	r := u.invite(t, "alice", "member")
	old := r.Invite.Code
	tok := u.accept(t, old, "alicepass1").Token
	rr := decode[UserInviteResponse](t, u.env, "POST", fmt.Sprintf("/v1/users/%d/reset-password", r.User.ID), u.admin, "", 200)
	if resp := u.do(t, "GET", "/v1/me", tok, ""); resp.StatusCode != 401 {
		t.Errorf("session after reset = %d", resp.StatusCode)
	}
	if resp := u.do(t, "POST", "/v1/login", "", `{"username":"alice","password":"alicepass1"}`); resp.StatusCode != 401 {
		t.Errorf("old password after reset = %d", resp.StatusCode)
	}
	// A second reset invalidates the first code.
	rr2 := decode[UserInviteResponse](t, u.env, "POST", fmt.Sprintf("/v1/users/%d/reset-password", r.User.ID), u.admin, "", 200)
	if resp := u.do(t, "POST", "/v1/invites/"+rr.Invite.Code+"/accept", "", `{"password":"alicepass2"}`); resp.StatusCode != 404 {
		t.Errorf("superseded code = %d", resp.StatusCode)
	}
	u.accept(t, rr2.Invite.Code, "alicepass2")
	if resp := u.do(t, "POST", "/v1/users/999/reset-password", u.admin, ""); resp.StatusCode != 404 {
		t.Errorf("reset unknown = %d", resp.StatusCode)
	}
}

func TestUpdateUserAndLastAdmin(t *testing.T) {
	u := setupUsers(t)
	adminID := u.user.ID
	path := fmt.Sprintf("/v1/users/%d", adminID)
	for _, body := range []string{`{"role":"member"}`, `{"disabled":true}`} {
		if resp := u.do(t, "PUT", path, u.admin, body); resp.StatusCode != 409 || errCode(t, resp) != "last_admin" {
			t.Errorf("last admin %s = %d", body, resp.StatusCode)
		}
	}
	if resp := u.do(t, "DELETE", path, u.admin, ""); resp.StatusCode != 409 {
		t.Errorf("delete last admin = %d", resp.StatusCode)
	}
	if resp := u.do(t, "PUT", path, u.admin, `{}`); resp.StatusCode != 400 {
		t.Errorf("empty update = %d", resp.StatusCode)
	}

	r := u.invite(t, "alice", "member")
	alice := u.accept(t, r.Invite.Code, "alicepass1").Token
	apath := fmt.Sprintf("/v1/users/%d", r.User.ID)

	// Disable: sessions revoked, login refused; enable restores login.
	info := decode[auth.UserInfo](t, u.env, "PUT", apath, u.admin, `{"disabled":true}`, 200)
	if !info.Disabled {
		t.Errorf("disable = %+v", info)
	}
	if resp := u.do(t, "GET", "/v1/me", alice, ""); resp.StatusCode != 401 {
		t.Errorf("disabled session = %d", resp.StatusCode)
	}
	if resp := u.do(t, "POST", "/v1/login", "", `{"username":"alice","password":"alicepass1"}`); resp.StatusCode != 401 {
		t.Errorf("disabled login = %d", resp.StatusCode)
	}
	decode[auth.UserInfo](t, u.env, "PUT", apath, u.admin, `{"disabled":false}`, 200)
	if resp := u.do(t, "POST", "/v1/login", "", `{"username":"alice","password":"alicepass1"}`); resp.StatusCode != 200 {
		t.Errorf("re-enabled login = %d", resp.StatusCode)
	}

	// Promote alice; now dave can step down, and alice becomes the last admin.
	decode[auth.UserInfo](t, u.env, "PUT", apath, u.admin, `{"role":"ADMIN"}`, 200)
	if got := decode[auth.UserInfo](t, u.env, "PUT", path, u.admin, `{"role":"member"}`, 200); got.Role != auth.RoleMember {
		t.Errorf("demote = %+v", got)
	}
	// dave is a member now (his token is still valid but no longer admin).
	if resp := u.do(t, "GET", "/v1/users", u.admin, ""); resp.StatusCode != 403 {
		t.Errorf("demoted admin list = %d", resp.StatusCode)
	}
	aliceTok := decode[LoginResponse](t, u.env, "POST", "/v1/login", "", `{"username":"alice","password":"alicepass1"}`, 200).Token
	if resp := u.do(t, "PUT", apath, aliceTok, `{"disabled":true}`); resp.StatusCode != 409 {
		t.Errorf("disable last admin (self) = %d", resp.StatusCode)
	}
	if resp := u.do(t, "PUT", "/v1/users/999", aliceTok, `{"disabled":true}`); resp.StatusCode != 404 {
		t.Errorf("update unknown = %d", resp.StatusCode)
	}
	if resp := u.do(t, "PUT", apath, aliceTok, `{"role":"root"}`); resp.StatusCode != 400 {
		t.Errorf("bad role = %d", resp.StatusCode)
	}
}

func TestChangeOwnPassword(t *testing.T) {
	u := setupUsers(t)
	other := u.login(t) // a second session for dave
	if resp := u.do(t, "PUT", "/v1/me/password", u.admin, `{"current":"wrong-password","new":"newpassword1"}`); resp.StatusCode != 403 {
		t.Errorf("wrong current = %d", resp.StatusCode)
	}
	if resp := u.do(t, "PUT", "/v1/me/password", u.admin, `{"current":"longenough","new":"short"}`); resp.StatusCode != 400 {
		t.Errorf("short new = %d", resp.StatusCode)
	}
	if resp := u.do(t, "PUT", "/v1/me/password", u.admin, `{"current":"longenough","new":"newpassword1"}`); resp.StatusCode != 204 {
		t.Fatalf("change = %d", resp.StatusCode)
	}
	if resp := u.do(t, "GET", "/v1/me", u.admin, ""); resp.StatusCode != 200 {
		t.Errorf("current session after change = %d", resp.StatusCode)
	}
	if resp := u.do(t, "GET", "/v1/me", other, ""); resp.StatusCode != 401 {
		t.Errorf("other session after change = %d", resp.StatusCode)
	}
	if resp := u.do(t, "POST", "/v1/login", "", `{"username":"dave","password":"newpassword1"}`); resp.StatusCode != 200 {
		t.Errorf("login with new password = %d", resp.StatusCode)
	}
	if resp := u.do(t, "PUT", "/v1/me/password", "", `{"current":"a","new":"b"}`); resp.StatusCode != 401 {
		t.Errorf("unauthenticated = %d", resp.StatusCode)
	}
}

func TestInviteAcceptRateLimited(t *testing.T) {
	u := setupUsers(t)
	var last int
	for i := 0; i <= InviteAcceptsPerMinute; i++ {
		last = u.do(t, "POST", "/v1/invites/bsi_nope/accept", "", `{"password":"whatever1"}`).StatusCode
	}
	if last != 429 {
		t.Errorf("after %d attempts status = %d, want 429", InviteAcceptsPerMinute+1, last)
	}
}

// userTables returns every table with a user_id column.
func userTables(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND p.name = 'user_id' ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

func countFor(t *testing.T, st *store.Store, table string, id int64) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM "`+table+`" WHERE user_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeleteUserCascades(t *testing.T) {
	u := setupUsers(t)
	r := u.invite(t, "alice", "member")
	id := r.User.ID
	u.accept(t, r.Invite.Code, "alicepass1") // session; audit entry by alice
	db := u.st.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO credentials(user_id, integration, kind, nonce, ciphertext, created_at, updated_at) VALUES (?, 'email', 'password', x'00', x'00', 0, 0)`, id)
	exec(`INSERT INTO policy_rules(user_id, capability, decision, created_at, updated_at) VALUES (?, 'email.send', 'allow', 0, 0)`, id)
	exec(`INSERT INTO conversations(id, user_id, created_at, updated_at) VALUES ('conv_alice', ?, 0, 0)`, id)
	exec(`INSERT INTO messages(conversation_id, role, content, created_at) VALUES ('conv_alice', 'user', '[]', 0)`)
	exec(`INSERT INTO memories(user_id, kind, content, created_at, updated_at) VALUES (?, 'note', 'zebrafish', 0, 0)`, id)
	exec(`INSERT INTO routines(user_id, name, trigger_kind, trigger_spec, prompt, created_at, updated_at) VALUES (?, 'r', 'cron', '0 * * * *', 'p', 0, 0)`, id)
	exec(`INSERT INTO routine_runs(routine_id, user_id, started_at, status, conversation_id) VALUES ((SELECT id FROM routines WHERE user_id = ?), ?, 0, 'done', 'conv_alice')`, id, id)
	exec(`INSERT INTO approvals(id, user_id, conversation_id, tool, capability, risk, status, created_at, expires_at) VALUES ('appr_alice', ?, 'conv_alice', 't', 'email.send', 'write', 'pending', 0, 0)`, id)
	exec(`INSERT INTO channel_links(channel, external_id, user_id, conversation_id, created_at) VALUES ('telegram', '42', ?, 'conv_alice', 0)`, id)
	exec(`INSERT INTO invites(user_id, code_hash, created_at, expires_at) VALUES (?, x'01', 0, 9999999999999)`, id)
	exec(`INSERT INTO audit_log(ts, user_id, kind) VALUES (0, ?, 'auth')`, id)
	// The admin's own data, partly referencing alice, must survive.
	exec(`INSERT INTO conversations(id, user_id, created_at, updated_at) VALUES ('conv_dave', ?, 0, 0)`, u.user.ID)
	exec(`INSERT INTO approvals(id, user_id, tool, capability, risk, status, created_at, expires_at, resolved_by) VALUES ('appr_dave', ?, 't', 'x.y', 'read', 'approved', 0, 0, ?)`, u.user.ID, id)
	exec(`INSERT INTO hooks(name, point, kind, command, created_by, created_at, updated_at) VALUES ('h', 'pre_tool_call', 'script', '[]', ?, 0, 0)`, id)

	// Kept by design: entries outlive users.
	keep := map[string]bool{"audit_log": true}
	tables := userTables(t, u.st)
	if len(tables) < 10 {
		t.Fatalf("user tables = %v", tables)
	}
	for _, tbl := range tables {
		if n := countFor(t, u.st, tbl, id); n == 0 {
			t.Errorf("seed missing for table %s: add a row so the cascade is tested", tbl)
		}
	}

	if resp := u.do(t, "DELETE", fmt.Sprintf("/v1/users/%d", id), u.admin, ""); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	for _, tbl := range tables {
		n := countFor(t, u.st, tbl, id)
		if keep[tbl] {
			if n == 0 {
				t.Errorf("%s rows for the deleted user were removed", tbl)
			}
			continue
		}
		if n != 0 {
			t.Errorf("%s: %d rows remain for deleted user", tbl, n)
		}
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id = 'conv_alice'`).Scan(&n)
	if n != 0 {
		t.Errorf("messages remain: %d", n)
	}
	db.QueryRow(`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'zebrafish'`).Scan(&n)
	if n != 0 {
		t.Errorf("memories_fts rows remain: %d", n)
	}
	db.QueryRow(`SELECT count(*) FROM approvals WHERE id = 'appr_dave' AND resolved_by IS NULL`).Scan(&n)
	if n != 1 {
		t.Error("admin's approval should survive with resolved_by cleared")
	}
	db.QueryRow(`SELECT count(*) FROM conversations WHERE id = 'conv_dave'`).Scan(&n)
	if n != 1 {
		t.Error("admin's conversation was deleted")
	}
	if resp := u.do(t, "DELETE", fmt.Sprintf("/v1/users/%d", id), u.admin, ""); resp.StatusCode != 404 {
		t.Errorf("second delete = %d", resp.StatusCode)
	}

	// The deleted user's ID is not reused.
	r2 := u.invite(t, "alice", "member")
	if r2.User.ID <= id {
		t.Errorf("new user ID %d reuses or precedes deleted ID %d", r2.User.ID, id)
	}

	u.audit.mu.Lock()
	defer u.audit.mu.Unlock()
	found := false
	for _, e := range u.audit.entries {
		if e.Notes[0] == "user deleted" && e.UserID == u.user.ID && strings.Contains(string(e.Detail), `"username":"alice"`) {
			found = true
		}
	}
	if !found {
		t.Error("no audit entry for the deletion")
	}
}

func TestUsersNotWired(t *testing.T) {
	e := setup(t) // no Users option
	tok := e.login(t)
	if resp := e.do(t, "GET", "/v1/users", tok, ""); resp.StatusCode != 501 {
		t.Errorf("list = %d", resp.StatusCode)
	}
	if resp := e.do(t, "POST", "/v1/invites/x/accept", "", `{"password":"x"}`); resp.StatusCode != 501 {
		t.Errorf("accept = %d", resp.StatusCode)
	}
}
