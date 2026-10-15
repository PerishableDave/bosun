// Bosun web UI entry point: sign-in, the app shell (sidebar, header,
// banner), routing, keyboard shortcuts, and the event stream.
//
// Routes (location.hash): #chat (new chat), #chat/<id>, #approvals, #tools,
// #memory, #accounts, #browser, #users (admins), #settings.
import { api, auth } from "./api.js";
import { pending, renderApprovalsPage, markResolved } from "./approvals.js";
import { createChat } from "./chat.js";
import { convs } from "./conversations.js";
import { createConvList } from "./convlist.js";
import { h, icon, iconButton } from "./dom.js";
import { connection, startEvents } from "./events.js";
import { renderTools, renderMemory, renderAccounts } from "./pages.js";
import { prefs } from "./prefs.js";
import { renderSettings } from "./settings.js";
import { showMenu, closeMenu } from "./ui.js";
import { renderUsers, acceptInviteForm } from "./users.js";
import { renderBrowser } from "./browser.js";

const root = document.getElementById("app");

// [id, label, icon, render(el, user), adminOnly]
const PAGES = [
  ["approvals", "Approvals", "shield", renderApprovalsPage],
  ["tools", "Tools", "tool", renderTools],
  ["memory", "Memory", "brain", renderMemory],
  ["accounts", "Accounts", "key", renderAccounts],
  ["browser", "Browser", "globe", renderBrowser],
  ["users", "Users", "users", renderUsers, true],
  ["settings", "Settings", "settings", renderSettings, false, true], // hidden from the nav (user menu)
];

let stopEvents = () => {};

// ---------- sign-in ----------

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
  h("h1", {}, h("span", { class: "logo", "aria-hidden": "true" }, "B"), "Bosun"),
  h("label", {}, "Username", user),
  h("label", {}, "Password", pass),
  h("button", { class: "btn primary block", type: "submit" }, "Sign in"),
  h("a", { href: "#", class: "small", onclick: (e) => { e.preventDefault(); showAcceptInvite(); } }, "Have an invite code?"),
  err);
  root.replaceChildren(h("main", { class: "login-wrap" }, form));
  user.focus();
}

function showAcceptInvite() {
  const form = acceptInviteForm((res) => {
    auth.token = res.token;
    showApp(res.user);
  }, () => showLogin());
  root.replaceChildren(h("main", { class: "login-wrap" }, form));
}

// ---------- shell ----------

function showApp(user) {
  // A fresh sign-in (possibly as someone else) starts from empty state.
  convs.listeners.clear();
  convs.items = [];
  convs.meta.clear();
  convs.filter = { q: "", archived: false };
  convs.nextCursor = "";
  const pages = PAGES.filter((p) => !p[4] || user.role === "admin");
  const isPhone = () => window.matchMedia("(max-width: 768px)").matches;

  // Header: title (double-click to rename), source chip, actions.
  const titleText = h("span", { class: "title-text" });
  const chip = h("span", { class: "chip", hidden: true });
  const title = h("div", { class: "title" }, titleText, chip);
  const kebab = iconButton("dots", "Conversation actions", () => header.menu && showMenu(kebab, header.menu(startHeaderRename)));
  kebab.setAttribute("aria-haspopup", "menu");
  const header = { onRename: null, menu: null };
  let headerEditing = false;

  function setHeader({ title: t, chip: c = "", onRename = null, menu = null }) {
    header.onRename = onRename;
    header.menu = menu;
    kebab.hidden = !menu;
    chip.hidden = !c;
    chip.className = "chip " + c;
    chip.textContent = c;
    if (headerEditing) return;
    titleText.textContent = t;
    titleText.classList.toggle("editable", !!onRename);
    titleText.title = onRename ? "Double-click to rename" : "";
    document.title = (pending.list.length ? `(${pending.list.length}) ` : "") + (t && t !== "New chat" ? t + " · " : "") + "Bosun";
  }

  function startHeaderRename() {
    if (!header.onRename || headerEditing) return;
    headerEditing = true;
    const save = header.onRename;
    const input = h("input", { value: titleText.textContent, "aria-label": "Conversation title", maxlength: "200" });
    let done = false;
    const finish = (ok) => {
      if (done) return;
      done = true;
      headerEditing = false;
      input.replaceWith(titleText);
      if (ok && input.value.trim() && input.value.trim() !== titleText.textContent) {
        titleText.textContent = input.value.trim();
        save(input.value);
      }
    };
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") { e.preventDefault(); finish(true); }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); finish(false); }
    });
    input.addEventListener("blur", () => finish(true));
    titleText.replaceWith(input);
    input.focus();
    input.select();
  }
  titleText.addEventListener("dblclick", startHeaderRename);

  // Sidebar
  const shell = h("div", { class: "shell" + (prefs.sidebarCollapsed ? " collapsed" : "") });
  const setCollapsed = (v) => {
    shell.classList.toggle("collapsed", v);
    prefs.sidebarCollapsed = v;
  };
  const openDrawer = (v) => {
    shell.classList.toggle("drawer-open", v);
    // The page behind an open drawer can't be scrolled, tapped or focused.
    const main = shell.querySelector(".main");
    if (main) main.inert = v;
    if (v) sidebar.querySelector(".side-new")?.focus();
  };
  const toggleSidebar = () => (isPhone() ? openDrawer(!shell.classList.contains("drawer-open")) : setCollapsed(!shell.classList.contains("collapsed")));
  // Leaving phone width (rotation, resize) closes the drawer so the page
  // isn't left inert behind a sidebar that is no longer a drawer.
  window.matchMedia("(max-width: 768px)").addEventListener("change", (e) => { if (!e.matches) openDrawer(false); });

  const chat = createChat({ setHeader: (x) => { if (route.current === "chat") setHeader(x); }, navigate: (hash) => { location.hash = hash; } });
  const list = createConvList({
    current: () => (route.current === "chat" ? chat.currentId() : ""),
    onNavigate: () => openDrawer(false),
    onDeleted: (id) => chat.deleted(id),
  });

  const badge = h("span", { class: "badge", hidden: true }, "0");
  const navLinks = pages.filter((p) => !p[5]).map(([id, label, ic]) =>
    h("a", { href: "#" + id, class: "nav-link", "data-page": id, onclick: () => openDrawer(false) }, icon(ic), label, id === "approvals" ? badge : null));
  const userBtn = h("button", { type: "button", class: "user-btn", "aria-haspopup": "menu", "aria-label": "Account menu for " + user.username },
    h("span", { class: "avatar", "aria-hidden": "true" }, user.username.slice(0, 1)),
    h("span", { class: "who" }, h("span", {}, user.username), h("span", { class: "muted small" }, user.role)));
  userBtn.addEventListener("click", () => showMenu(userBtn, [
    { label: "Settings", icon: "settings", run: () => { location.hash = "#settings"; openDrawer(false); } },
    { label: "Log out", icon: "logout", run: logout },
  ]));

  const newChat = () => {
    openDrawer(false);
    // Switch now (pushState fires no hashchange), so keys typed right after
    // the shortcut land in the new chat.
    if (location.hash !== "#chat") history.pushState(null, "", "#chat");
    route();
    chat.focus();
  };

  const sidebar = h("aside", { class: "sidebar", "aria-label": "Sidebar" },
    h("div", { class: "side-head" },
      h("a", { class: "brand", href: "#chat" }, h("span", { class: "logo", "aria-hidden": "true" }, "B"), "Bosun"),
      iconButton("sidebar", "Hide sidebar", () => setCollapsed(true), "collapse-side"),
      iconButton("close", "Close menu", () => openDrawer(false), "close-side")),
    h("button", { type: "button", class: "btn ghost block side-new", onclick: newChat, title: "New chat" }, icon("plus"), "New chat"),
    h("nav", { class: "side-nav", "aria-label": "Pages" }, navLinks),
    list.box,
    list.el,
    h("div", { class: "side-foot" }, userBtn));

  const banner = h("div", { class: "banner", role: "status", hidden: true });
  const content = h("div", { class: "content" });
  const page = h("div", { class: "page", hidden: true });
  content.append(chat.el, page);
  const topbar = h("header", { class: "topbar" },
    iconButton("menu", "Show sidebar", () => (isPhone() ? openDrawer(true) : setCollapsed(false)), "open-side"),
    iconButton("plus", "New chat", newChat, "open-side"),
    title,
    h("div", { class: "top-actions" }, kebab));
  shell.append(sidebar, h("div", { class: "scrim", onclick: () => openDrawer(false) }),
    h("div", { class: "main" }, topbar, content));
  root.replaceChildren(banner, shell);

  // Disconnected banner.
  let failed = false, bannerTimer = null;
  const drawBanner = () => {
    clearInterval(bannerTimer);
    if (connection.state === "open") failed = false;
    if (connection.state === "retrying") failed = true;
    banner.hidden = !failed;
    if (!failed) return;
    const paint = () => {
      const s = Math.max(0, Math.ceil((connection.retryAt - Date.now()) / 1000));
      banner.replaceChildren(h("span", {}, connection.state === "retrying" ? `Disconnected. Retrying in ${s}s…` : "Reconnecting…"),
        connection.state === "retrying" ? h("button", { type: "button", class: "btn small", onclick: () => connection.retryNow() }, "Retry now") : null);
    };
    paint();
    if (connection.state === "retrying") bannerTimer = setInterval(paint, 1000);
  };
  connection.listeners.clear();
  connection.listeners.add(drawBanner);

  // Routing
  let cleanup = null;
  async function route() {
    const hash = decodeURIComponent((location.hash || "#chat").slice(1));
    const [id, arg = ""] = hash.split(/\/(.*)/s);
    const entry = pages.find((p) => p[0] === id);
    cleanup?.();
    cleanup = null;
    closeMenu();
    route.current = entry ? entry[0] : "chat";
    navLinks.forEach((a) => a.classList.toggle("active", a.dataset.page === route.current));
    chat.el.hidden = !!entry;
    page.hidden = !entry;
    if (!entry) {
      page.replaceChildren();
      chat.open(id === "chat" ? arg : "");
      list.draw();
      if (!isPhone()) chat.focus();
      return;
    }
    list.draw();
    setHeader({ title: entry[1] });
    page.className = "page page-" + entry[0];
    page.replaceChildren();
    page.scrollTop = 0;
    cleanup = await entry[3](page, user);
  }
  route.current = "";
  window.onhashchange = route;

  // Keyboard shortcuts
  document.onkeydown = (e) => {
    const mod = e.metaKey || e.ctrlKey;
    if (mod && !e.shiftKey && !e.altKey && e.key.toLowerCase() === "k") {
      e.preventDefault();
      if (isPhone()) openDrawer(true);
      else if (shell.classList.contains("collapsed")) setCollapsed(false);
      list.search.focus();
      list.search.select();
    } else if (mod && !e.shiftKey && !e.altKey && e.key.toLowerCase() === "b") {
      e.preventDefault();
      toggleSidebar();
    } else if (mod && e.shiftKey && e.key.toLowerCase() === "o") {
      e.preventDefault();
      newChat();
    } else if (e.key === "Escape" && shell.classList.contains("drawer-open")) {
      openDrawer(false);
    }
  };

  // Leaving the page cancels a running turn (it closes the stream).
  window.onbeforeunload = (e) => {
    if (chat.hasRunningTurn()) { e.preventDefault(); e.returnValue = ""; }
  };

  // Approvals badge and title count.
  pending.listeners.clear();
  pending.listeners.add((l) => {
    badge.textContent = l.length;
    badge.hidden = l.length === 0;
    badge.setAttribute("aria-label", l.length + " pending");
    document.title = document.title.replace(/^\(\d+\) /, "");
    if (l.length) document.title = `(${l.length}) ` + document.title;
  });

  // Event stream: approvals, conversation updates, routines.
  stopEvents = startEvents((type, d) => {
    if (type === "approval_required" || type === "approval_resolved") {
      if (type === "approval_resolved") markResolved(d.approval_id, d.decision);
      pending.refresh();
    } else if (type === "conversation_updated") {
      convs.applyEvent(d);
    } else if (type === "conversation_deleted") {
      convs.forget(d.id);
      chat.deleted(d.id);
    } else if (type === "routine_finished") {
      convs.refreshSoon();
    }
    chat.onEvent(type, d);
  }, (first) => {
    pending.refresh();
    if (!first) convs.refreshSoon(0);
  });

  convs.load();
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
