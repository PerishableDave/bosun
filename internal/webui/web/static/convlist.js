// The sidebar's conversation list: search, archived filter, date groups,
// per-item actions (hover icons on desktop, a kebab menu on touch), inline
// rename and "Load more".
import { convs, activityTime } from "./conversations.js";
import * as act from "./convactions.js";
import { h, icon, iconButton, modKey, fmtFull } from "./dom.js";
import { showMenu } from "./ui.js";

// groupOf names the date group for a conversation.
function groupOf(c, now) {
  if (c.pinned && !convs.filter.archived) return "Pinned";
  const d = activityTime(c);
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const day = 86400000;
  if (d >= startOfToday) return "Today";
  if (d >= startOfToday - day) return "Yesterday";
  if (d >= startOfToday - 7 * day) return "Previous 7 days";
  return "Older";
}
const ORDER = ["Pinned", "Today", "Yesterday", "Previous 7 days", "Older"];

// createConvList returns {search, filter, el}: the search box, the archived
// toggle and the scrolling list. current() is the open conversation id;
// onNavigate is called after a click (closes the phone drawer).
export function createConvList({ current, onNavigate, onDeleted }) {
  const search = h("input", { type: "search", placeholder: "Search", "aria-label": "Search conversations", autocomplete: "off", spellcheck: "false" });
  const filter = h("button", { type: "button", class: "icon-btn filter-btn", "aria-pressed": "false", "aria-label": "Show archived conversations", title: "Show archived conversations" }, icon("archive"));
  const box = h("div", { class: "side-search" },
    h("div", { class: "search-box" }, icon("search"), search, h("kbd", {}, modKey + "K")),
    filter);
  const el = h("div", { class: "conv-scroll", role: "navigation", "aria-label": "Conversations" });
  let renaming = null; // id being renamed inline

  let timer;
  search.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(() => convs.setFilter({ q: search.value.trim() }), 250);
  });
  search.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && search.value) { e.stopPropagation(); search.value = ""; convs.setFilter({ q: "" }); }
    if (e.key === "ArrowDown") { e.preventDefault(); el.querySelector(".conv-link")?.focus(); }
  });
  filter.addEventListener("click", () => {
    const on = !convs.filter.archived;
    filter.setAttribute("aria-pressed", String(on));
    convs.setFilter({ archived: on });
  });

  function item(c) {
    const id = c.id;
    const active = id === current();
    const row = h("div", { class: "conv-item" + (active ? " active" : "") + (c.unread && !active ? " unread" : ""), "data-id": id });
    if (renaming === id) {
      const input = h("input", { class: "conv-rename", value: c.title || "", "aria-label": "Conversation title", maxlength: "200" });
      let done = false;
      const finish = (save) => {
        if (done) return;
        done = true;
        renaming = null;
        if (save) act.rename(id, input.value);
        draw();
      };
      input.addEventListener("keydown", (e) => {
        if (e.key === "Enter") { e.preventDefault(); finish(true); }
        if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); finish(false); }
      });
      input.addEventListener("blur", () => finish(true));
      row.append(input);
      queueMicrotask(() => { input.focus(); input.select(); });
      return row;
    }
    const startRename = () => { renaming = id; draw(); };
    const link = h("a", {
      class: "conv-link", href: "#chat/" + encodeURIComponent(id),
      title: (c.title || "New chat") + (c.last_message_preview ? "\n" + c.last_message_preview : "") + "\n" + fmtFull(activityTime(c)),
      "aria-current": active ? "page" : null,
      onclick: () => onNavigate?.(),
    },
    c.unread && !active ? h("span", { class: "unread-dot", "aria-label": "unread" }) : null,
    h("span", { class: "conv-title" }, act.displayTitle(c)),
    c.source === "routine" || c.source === "telegram" ? h("span", { class: "chip " + c.source }, c.source) : null);
    link.addEventListener("dblclick", (e) => { e.preventDefault(); startRename(); });
    const kebab = iconButton("dots", "Conversation actions", (e) => {
      e.stopPropagation();
      showMenu(kebab, act.menuItems(id, { onRename: startRename, onDeleted: () => onDeleted?.(id) }));
    }, "sm conv-kebab");
    kebab.setAttribute("aria-haspopup", "menu");
    row.append(link,
      h("div", { class: "conv-actions" },
        iconButton("pencil", "Rename", startRename, "sm"),
        iconButton(c.pinned ? "unpin" : "pin", c.pinned ? "Unpin" : "Pin", () => act.togglePin(id), "sm"),
        iconButton(c.archived ? "unarchive" : "archive", c.archived ? "Unarchive" : "Archive", () => act.toggleArchive(id), "sm"),
        iconButton("trash", "Delete", () => act.remove(id, () => onDeleted?.(id)), "sm")),
      kebab);
    return row;
  }

  function draw() {
    const now = new Date();
    const groups = new Map();
    const items = [...convs.items].sort((a, b) => activityTime(b) - activityTime(a));
    for (const c of items) {
      const g = groupOf(c, now);
      if (!groups.has(g)) groups.set(g, []);
      groups.get(g).push(c);
    }
    const parts = [];
    if (convs.filter.archived) {
      parts.push(h("div", { class: "list-head" }, h("span", {}, "Archived"),
        h("button", { type: "button", class: "btn ghost small", onclick: () => filter.click() }, "Back")));
    }
    for (const g of ORDER) {
      if (!groups.has(g)) continue;
      parts.push(h("section", { class: "conv-group", "aria-label": g },
        h("div", { class: "conv-group-title" }, g),
        groups.get(g).map(item)));
    }
    if (!items.length && !convs.loading) {
      parts.push(h("p", { class: "conv-empty" },
        convs.error ? "Could not load conversations: " + convs.error.message
          : convs.filter.q ? "No conversations match “" + convs.filter.q + "”."
            : convs.filter.archived ? "No archived conversations." : "No conversations yet."));
    }
    if (convs.nextCursor) {
      parts.push(h("button", { type: "button", class: "btn small load-more", disabled: convs.loading, onclick: () => convs.loadMore() },
        convs.loading ? "Loading…" : "Load more"));
    }
    // Keep focus on the same item across redraws (keyboard users).
    const focusedId = document.activeElement?.closest?.(".conv-item")?.dataset.id;
    const focusedAction = document.activeElement?.getAttribute?.("aria-label");
    el.replaceChildren(...parts);
    if (focusedId && !renaming) {
      const row = el.querySelector(`.conv-item[data-id="${CSS.escape(focusedId)}"]`);
      const target = row && ([...row.querySelectorAll("button")].find((b) => b.getAttribute("aria-label") === focusedAction) || row.querySelector(".conv-link"));
      target?.focus();
    }
  }

  // Arrow keys move between conversations.
  el.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const links = [...el.querySelectorAll(".conv-link")];
    const i = links.indexOf(document.activeElement);
    if (i < 0) return;
    e.preventDefault();
    if (e.key === "ArrowDown") links[i + 1]?.focus();
    else if (i === 0) search.focus();
    else links[i - 1]?.focus();
  });

  convs.listeners.add(draw);
  return { search, el, box, draw, rename: (id) => { renaming = id; draw(); } };
}
