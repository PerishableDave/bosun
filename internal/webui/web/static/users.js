// Users page (admins only) and the invite-acceptance form on the login screen.
import { api } from "./api.js";
import { h, fmtTime } from "./dom.js";

// inviteBox shows a freshly issued one-time code and how to use it.
function inviteBox(username, inv) {
  const cmd = "bosun accept-invite " + inv.code + (isLoopback() ? "" : " --server " + location.origin);
  return h("div", { class: "approval invite" },
    h("strong", {}, "Invite code for " + username),
    h("code", { class: "appr-args" }, inv.code),
    h("div", { class: "muted small" },
      "Single use, expires " + fmtTime(inv.expires_at) + ". Give it to " + username +
      " privately; they choose “Have an invite code?” on the sign-in page, or run:"),
    h("code", { class: "appr-args" }, cmd),
    h("div", { class: "muted small" }, "The code is shown only now."));
}

function isLoopback() {
  const host = location.hostname;
  return host === "localhost" || host === "[::1]" || host === "::1" || /^127\./.test(host);
}

export async function renderUsers(el, me) {
  const msg = h("p", { class: "error" });
  const shown = h("div");
  const tbody = h("tbody");

  async function load() {
    let res;
    try { res = await api("GET", "/v1/users"); } catch (e) { msg.textContent = e.message; return; }
    tbody.replaceChildren(...(res.users || []).map(row));
  }

  function status(u) {
    if (u.disabled) return "disabled";
    if (u.invite_expires_at) return "invited";
    return "active";
  }

  function row(u) {
    const act = async (fn) => {
      msg.textContent = "";
      try { await fn(); await load(); } catch (e) { msg.textContent = e.message; }
    };
    const toggle = h("button", { class: "btn small", onclick: () => act(() =>
      api("PUT", "/v1/users/" + u.id, { disabled: !u.disabled })) }, u.disabled ? "Enable" : "Disable");
    const reset = h("button", { class: "btn small", disabled: u.disabled, onclick: () => act(async () => {
      if (!confirm("Reset " + u.username + "’s password? They are signed out and need a new invite code.")) return;
      const r = await api("POST", "/v1/users/" + u.id + "/reset-password");
      shown.replaceChildren(inviteBox(u.username, r.invite));
    }) }, u.invite_expires_at ? "New code" : "Reset password");
    const remove = h("button", { class: "btn small danger", onclick: () => act(async () => {
      if (!confirm("Delete " + u.username + " and all their conversations, memories, credentials and routines? This cannot be undone.")) return;
      await api("DELETE", "/v1/users/" + u.id);
    }) }, "Remove");
    const self = u.id === me.id;
    return h("tr", {},
      h("td", { "data-label": "User" }, h("strong", {}, u.username), self ? h("span", { class: "muted small" }, " (you)") : null),
      h("td", { "data-label": "Role" }, h("span", { class: "tag" }, u.role)),
      h("td", { "data-label": "Status" }, status(u)),
      h("td", { "data-label": "Last login" }, h("span", { class: "muted small" }, u.last_login_at ? fmtTime(u.last_login_at) : "never")),
      h("td", { "data-label": "" }, toggle, " ", reset, " ", remove));
  }

  const name = h("input", { type: "text", placeholder: "username", autocomplete: "off", required: true, "aria-label": "New username" });
  const role = h("select", { "aria-label": "Role" }, h("option", { value: "member" }, "member"), h("option", { value: "admin" }, "admin"));
  const add = async (e) => {
    e.preventDefault();
    msg.textContent = "";
    try {
      const r = await api("POST", "/v1/users", { username: name.value.trim(), role: role.value });
      shown.replaceChildren(inviteBox(r.user.username, r.invite));
      name.value = "";
      await load();
    } catch (err) { msg.textContent = err.message; }
  };

  el.replaceChildren(
    h("h2", {}, "Users"),
    h("p", { class: "muted small" }, "Add household members. Each gets a one-time invite code (valid 24 hours) and chooses their own password."),
    h("form", { class: "mem-add", onsubmit: add }, name, role, h("button", { class: "btn primary", type: "submit" }, "Add user")),
    shown, msg,
    h("table", { class: "grid" },
      h("thead", {}, h("tr", {}, h("th", {}, "User"), h("th", {}, "Role"), h("th", {}, "Status"), h("th", {}, "Last login"), h("th", {}, ""))),
      tbody));
  await load();
}

// acceptInviteForm is shown instead of the sign-in form. onDone(res) gets
// the login response; onCancel returns to sign-in.
export function acceptInviteForm(onDone, onCancel) {
  const code = h("input", { type: "text", name: "code", autocomplete: "off", required: true, autofocus: true, spellcheck: "false" });
  const pass = h("input", { type: "password", name: "new-password", autocomplete: "new-password", required: true, minlength: "8" });
  const again = h("input", { type: "password", name: "new-password-2", autocomplete: "new-password", required: true });
  const err = h("p", { class: "error", role: "alert" });
  return h("form", { class: "login", onsubmit: async (e) => {
    e.preventDefault();
    err.textContent = "";
    if (pass.value !== again.value) { err.textContent = "The passwords do not match."; return; }
    try {
      const c = code.value.trim().toLowerCase();
      const res = await api("POST", "/v1/invites/" + encodeURIComponent(c) + "/accept", { password: pass.value, label: "web" });
      onDone(res);
    } catch (ex) {
      err.textContent = ex.message;
    }
  } },
  h("h1", {}, "Accept invite"),
  h("p", { class: "muted small" }, "Enter the code an admin gave you and choose your password."),
  h("label", {}, "Invite code", code),
  h("label", {}, "New password (at least 8 characters)", pass),
  h("label", {}, "Repeat password", again),
  h("button", { class: "btn primary block", type: "submit" }, "Set password and sign in"),
  h("a", { href: "#", onclick: (e) => { e.preventDefault(); onCancel(); } }, "Back to sign in"),
  err);
}
