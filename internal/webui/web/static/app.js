// Bosun web UI entry point: login, page shell, tab navigation.
import { api, auth } from "./api.js";
import { pending, startEvents, renderApprovalsPage } from "./approvals.js";
import { renderChat } from "./chat.js";
import { h } from "./dom.js";
import { renderTools, renderMemory } from "./pages.js";

const root = document.getElementById("app");
const PAGES = [
  ["chat", "Chat"],
  ["tools", "Tools", renderTools],
  ["memory", "Memory", renderMemory],
  ["approvals", "Approvals", renderApprovalsPage],
];

let stopEvents = () => {};

function showLogin(note) {
  stopEvents();
  auth.token = "";
  const user = h("input", { type: "text", name: "username", autocomplete: "username", required: true, autofocus: true });
  const pass = h("input", { type: "password", name: "password", autocomplete: "current-password", required: true });
  const err = h("p", { class: "error", role: "alert" }, note || "");
  const form = h("form", { class: "login", onsubmit: async (e) => {
    e.preventDefault();
    err.textContent = "";
    try {
      const res = await api("POST", "/v1/login", { username: user.value.trim(), password: pass.value, label: "web" });
      auth.token = res.token;
      showApp(res.user);
    } catch (ex) {
      err.textContent = ex.code === "invalid_credentials" ? "Wrong username or password." : ex.message;
      pass.value = "";
    }
  } },
  h("h1", {}, "Bosun"),
  h("label", {}, "Username", user),
  h("label", {}, "Password", pass),
  h("button", { class: "btn primary block", type: "submit" }, "Sign in"),
  err);
  root.replaceChildren(h("main", { class: "login-wrap" }, form));
}

function showApp(user) {
  const badge = h("span", { class: "badge", hidden: true }, "0");
  const tabs = PAGES.map(([id, label]) => {
    const a = h("a", { href: "#" + id, class: "tab", "data-page": id }, label);
    if (id === "approvals") a.append(" ", badge);
    return a;
  });
  // The chat page is built once and only hidden when another tab is open, so
  // a running turn (and its pending approval) survives tab switches: leaving
  // the page must not drop the SSE connection, which would cancel the turn.
  const chatPage = h("main", { class: "page page-chat" });
  const page = h("main", { class: "page", hidden: true });
  root.replaceChildren(
    h("header", { class: "top" },
      h("strong", { class: "brand" }, "Bosun"),
      h("nav", {}, tabs),
      h("span", { class: "who" }, user.username, " ", h("span", { class: "tag" }, user.role)),
      h("button", { class: "btn small", onclick: logout }, "Log out")),
    chatPage, page);
  renderChat(chatPage);

  let cleanup = null;
  const route = async () => {
    const id = (location.hash || "#chat").slice(1);
    const entry = PAGES.find((p) => p[0] === id) || PAGES[0];
    tabs.forEach((t) => t.classList.toggle("active", t.dataset.page === entry[0]));
    cleanup?.();
    cleanup = null;
    chatPage.hidden = entry[0] !== "chat";
    page.hidden = entry[0] === "chat";
    if (entry[0] !== "chat") {
      page.className = "page page-" + entry[0];
      cleanup = await entry[2](page);
    }
  };
  window.onhashchange = route;

  pending.listeners.clear();
  pending.listeners.add((list) => {
    badge.textContent = list.length;
    badge.hidden = list.length === 0;
    document.title = (list.length ? `(${list.length}) ` : "") + "Bosun";
  });
  stopEvents = startEvents(() => {});
  route();
}

async function logout() {
  try { await api("POST", "/v1/logout"); } catch { /* token may already be invalid */ }
  showLogin();
}

auth.onExpired = () => showLogin("Your session expired. Please sign in again.");

(async () => {
  if (!auth.token) return showLogin();
  try {
    showApp(await api("GET", "/v1/me"));
  } catch {
    showLogin();
  }
})();
