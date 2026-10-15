// Per-browser preferences in localStorage: theme, sidebar state, and
// composer drafts. Every access tolerates blocked storage.

function get(key, fallback) {
  try { const v = localStorage.getItem(key); return v === null ? fallback : v; } catch { return fallback; }
}

function set(key, value) {
  try {
    if (value === null || value === "") localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch { /* storage blocked or full */ }
}

export const prefs = {
  // theme is "auto", "light" or "dark" (theme.js applies it on load).
  get theme() { return get("bosun.theme", "auto"); },
  set theme(t) {
    set("bosun.theme", t === "auto" ? null : t);
    if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
    else document.documentElement.removeAttribute("data-theme");
  },
  get sidebarCollapsed() { return get("bosun.sidebar", "") === "collapsed"; },
  set sidebarCollapsed(v) { set("bosun.sidebar", v ? "collapsed" : null); },
};

// Drafts are keyed by conversation id ("new" for a chat not yet created).
const DRAFT = "bosun.draft.";

export const drafts = {
  get(id) { return get(DRAFT + (id || "new"), ""); },
  set(id, text) { set(DRAFT + (id || "new"), text.trim() ? text : null); },
  clear(id) { set(DRAFT + (id || "new"), null); },
  // move carries the "new chat" draft over once the conversation exists.
  move(from, to) {
    const t = this.get(from);
    this.clear(from);
    if (t) this.set(to, t);
  },
};
