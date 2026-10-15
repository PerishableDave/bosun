// Conversation list state and the conversation API (list, patch, delete,
// read, export), shared by the sidebar, the header and the chat view.
//
// Mutations are optimistic: the change is applied locally and listeners are
// notified at once; if the server refuses, the old state is restored and
// the error is rethrown for the caller to report.
import { api, download, saveBlob, unsupported } from "./api.js";

const PAGE = 50;

// Older daemons return only {id, title, created_at, updated_at}; fill the
// contract's fields with neutral defaults so the UI need not check.
function normalize(c) {
  return {
    title: "",
    title_source: "auto",
    pinned: false,
    archived: false,
    source: "chat",
    unread: false,
    message_count: 0,
    last_message_preview: "",
    ...c,
  };
}

// activityTime is the time a conversation is grouped and sorted by.
// Like the server: last message time, else creation time (renames and pins
// do not reorder).
export const activityTime = (c) => new Date(c.last_message_at || c.created_at || c.updated_at || 0);

export const convs = {
  items: [],          // the loaded page(s) for the current filter
  meta: new Map(),    // id -> conversation, including ones outside the list
  filter: { q: "", archived: false },
  nextCursor: "",
  loading: false,
  error: null,
  listeners: new Set(),

  changed() { this.listeners.forEach((f) => f(this)); },

  get(id) { return this.meta.get(id) || null; },

  // remember merges fields into the cached conversation (and list item).
  remember(c) {
    const cur = this.meta.get(c.id);
    // An untitled conversation keeps the title shown locally (the first
    // message) until the server names it.
    if (cur && cur.title && !c.title) c = { ...c, title: cur.title };
    const next = cur ? Object.assign(cur, c) : normalize(c);
    this.meta.set(c.id, next);
    return next;
  },

  query(before) {
    const p = new URLSearchParams({ archived: this.filter.archived ? "true" : "false", limit: String(PAGE) });
    if (this.filter.q) p.set("q", this.filter.q);
    if (before) p.set("before", before);
    return "/v1/conversations?" + p;
  },

  // load fetches the first page for the current filter.
  async load() {
    const seq = (this.seq = (this.seq || 0) + 1);
    this.loading = true;
    this.changed();
    try {
      const res = await api("GET", this.query());
      if (seq !== this.seq) return; // a newer load superseded this one
      const raw = res.conversations || res.items || [];
      // An older daemon has no archive/search fields and ignores the
      // filters; apply them here.
      this.legacy = raw.length > 0 && !("archived" in raw[0]);
      const list = raw.map((c) => this.remember(c));
      this.items = this.legacy ? this.clientFilter(list) : list;
      this.nextCursor = res.next_cursor || "";
      this.error = null;
    } catch (e) {
      if (seq !== this.seq) return;
      this.error = e;
    } finally {
      if (seq === this.seq) {
        this.loading = false;
        this.changed();
      }
    }
  },

  clientFilter(list) {
    const q = this.filter.q.toLowerCase();
    return list.filter((c) => !!c.archived === this.filter.archived &&
      (!q || (c.title || "").toLowerCase().includes(q)));
  },

  async loadMore() {
    if (!this.nextCursor || this.loading) return;
    this.loading = true;
    this.changed();
    try {
      const res = await api("GET", this.query(this.nextCursor));
      const seen = new Set(this.items.map((c) => c.id));
      for (const c of res.conversations || res.items || []) {
        if (!seen.has(c.id)) this.items.push(this.remember(c));
      }
      this.nextCursor = res.next_cursor || "";
    } finally {
      this.loading = false;
      this.changed();
    }
  },

  // refreshSoon reloads the first page after a short pause, coalescing
  // bursts of events. Items already loaded beyond the first page are kept.
  refreshSoon(ms = 400) {
    clearTimeout(this.refreshTimer);
    this.refreshTimer = setTimeout(async () => {
      const extra = this.items.slice(PAGE);
      const cursor = this.nextCursor;
      await this.load();
      if (extra.length) {
        const seen = new Set(this.items.map((c) => c.id));
        this.items.push(...extra.filter((c) => !seen.has(c.id)));
        this.nextCursor = cursor;
        this.changed();
      }
    }, ms);
  },

  setFilter(f) {
    Object.assign(this.filter, f);
    this.items = [];
    this.nextCursor = "";
    return this.load();
  },

  async create(title) {
    const c = this.remember(await api("POST", "/v1/conversations", title ? { title } : {}));
    if (!this.filter.archived && !this.filter.q) this.items.unshift(c);
    this.changed();
    return c;
  },

  // touch records local activity (a message sent) so the list re-sorts.
  touch(id, fields) {
    const c = this.meta.get(id);
    if (!c) return;
    Object.assign(c, { last_message_at: new Date().toISOString(), updated_at: new Date().toISOString() }, fields);
    if (!this.items.includes(c) && !this.filter.archived && !this.filter.q) this.items.unshift(c);
    this.changed();
  },

  // patch applies {title?, pinned?, archived?} optimistically.
  async patch(id, fields) {
    const c = this.meta.get(id);
    if (!c) throw new Error("unknown conversation");
    const before = { title: c.title, title_source: c.title_source, pinned: c.pinned, archived: c.archived };
    const idx = this.items.indexOf(c);
    Object.assign(c, fields, "title" in fields ? { title_source: "user" } : {});
    if ("archived" in fields && fields.archived !== this.filter.archived && idx >= 0) this.items.splice(idx, 1);
    this.changed();
    try {
      const res = await api("PATCH", "/v1/conversations/" + encodeURIComponent(id), fields);
      if (res && res.id) this.remember(res);
      this.changed();
    } catch (e) {
      Object.assign(c, before);
      if (idx >= 0 && !this.items.includes(c)) this.items.splice(Math.min(idx, this.items.length), 0, c);
      this.changed();
      throw e;
    }
  },

  // remove deletes a conversation (and its messages) optimistically.
  async remove(id) {
    const c = this.meta.get(id);
    const idx = c ? this.items.indexOf(c) : -1;
    if (idx >= 0) this.items.splice(idx, 1);
    this.changed();
    try {
      await api("DELETE", "/v1/conversations/" + encodeURIComponent(id));
      this.meta.delete(id);
    } catch (e) {
      if (idx >= 0) this.items.splice(Math.min(idx, this.items.length), 0, c);
      this.changed();
      throw e;
    }
  },

  // forget drops a conversation deleted elsewhere (conversation_deleted).
  forget(id) {
    const c = this.meta.get(id);
    this.meta.delete(id);
    const i = c ? this.items.indexOf(c) : -1;
    if (i >= 0) this.items.splice(i, 1);
    this.changed();
  },

  // markRead clears the unread dot; an older daemon has no read state, so
  // its errors are ignored.
  async markRead(id) {
    const c = this.meta.get(id);
    if (!c || !c.unread) return;
    c.unread = false;
    this.changed();
    try { await api("POST", "/v1/conversations/" + encodeURIComponent(id) + "/read"); } catch { /* best effort */ }
  },

  // applyEvent handles conversation_updated from the event stream.
  applyEvent(d) {
    if (!d || !d.id) return;
    const c = this.meta.get(d.id);
    if (c) {
      const fields = {};
      for (const k of ["title", "title_source", "updated_at", "unread", "pinned", "archived", "last_message_at", "last_message_preview"]) {
        if (k in d) fields[k] = d[k];
      }
      Object.assign(c, fields);
      this.changed();
    }
    // Pin/archive changes and new conversations move items between groups
    // or filters; a refetch of the first page settles them.
    this.refreshSoon();
  },

  // exportMarkdown downloads the conversation as Markdown. Without the
  // export endpoint it builds the file from the stored history instead.
  async exportMarkdown(id) {
    const c = this.meta.get(id);
    const name = slug(c?.title || "conversation") + ".md";
    try {
      await download("/v1/conversations/" + encodeURIComponent(id) + "/export?format=markdown", name);
      return;
    } catch (e) {
      if (!unsupported(e)) throw e;
    }
    const conv = await api("GET", "/v1/conversations/" + encodeURIComponent(id));
    saveBlob(new Blob([toMarkdown(conv)], { type: "text/markdown" }), name);
  },
};


function slug(s) {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 60) || "conversation";
}

// toMarkdown renders a conversation's stored history for export.
export function toMarkdown(conv) {
  const out = ["# " + (conv.title || "Conversation"), ""];
  for (const m of conv.messages || []) {
    for (const b of m.content || []) {
      if (b.type === "text" && b.text) {
        out.push("**" + (m.role === "user" ? "You" : "Bosun") + "** · " + new Date(m.created_at).toLocaleString(), "", b.text, "");
      } else if (b.type === "tool_use") {
        out.push("> tool call `" + b.name + "` " + JSON.stringify(b.input ?? {}), "");
      }
    }
  }
  return out.join("\n");
}
