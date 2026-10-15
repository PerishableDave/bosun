// DOM helpers shared by every page.

// h builds a DOM element: h("div", {class: "x", onclick: fn}, child, "text", ...).
// Strings become text nodes (never HTML); null/false children are skipped.
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "html") el.innerHTML = v; // only ever fed md.render() output
    else if (v === true) el.setAttribute(k, "");
    else el.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

// Icons: 24x24 stroke paths (drawn for this UI), rendered as inline SVG.
const ICONS = {
  plus: "M12 5v14M5 12h14",
  search: "M11 18a7 7 0 1 1 0-14 7 7 0 0 1 0 14zM21 21l-4.3-4.3",
  sidebar: "M4 5h16v14H4zM9 5v14",
  menu: "M4 7h16M4 12h16M4 17h16",
  close: "M6 6l12 12M18 6L6 18",
  pencil: "M4 20h4L19 9l-4-4L4 16zM13.5 6.5l4 4",
  pin: "M9 4h6l-1 5 4 4H6l4-4zM12 13v7",
  unpin: "M9 4h6l-1 5 4 4H6l4-4zM12 13v7M4 4l16 16",
  archive: "M4 5h16v4H4zM5 9v10h14V9M10 13h4",
  unarchive: "M4 5h16v4H4zM5 9v10h14V9M12 18v-6M9.5 14.5 12 12l2.5 2.5",
  trash: "M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3",
  dots: "M5 12h.01M12 12h.01M19 12h.01",
  copy: "M9 9h11v11H9zM5 15H4V4h11v1",
  check: "M5 12l5 5L20 7",
  send: "M12 19V5M5 12l7-7 7 7",
  stop: "M7 7h10v10H7z",
  down: "M12 5v14M5 12l7 7 7-7",
  retry: "M4 12a8 8 0 1 0 2.3-5.7M4 4v5h5",
  chevron: "M9 6l6 6-6 6",
  shield: "M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z",
  tool: "M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.5 2.5-2.4-.6-.6-2.4z",
  chat: "M4 5h16v11H8l-4 4z",
  brain: "M12 4a3 3 0 0 0-3 3 3 3 0 0 0-3 3 3 3 0 0 0 1 5 3 3 0 0 0 5 2 3 3 0 0 0 5-2 3 3 0 0 0 1-5 3 3 0 0 0-3-3 3 3 0 0 0-3-3zM12 4v16",
  key: "M15 9a3 3 0 1 1-6 0 3 3 0 0 1 6 0zM12 12v8M12 16h3M12 19h2",
  globe: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM3 12h18M12 3c3 3 3 15 0 18M12 3c-3 3-3 15 0 18",
  users: "M9 11a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7zM2.5 20c.5-3.5 3-5.5 6.5-5.5s6 2 6.5 5.5M16 4.5a3.5 3.5 0 0 1 0 6.5M18 14.5c2 .7 3.2 2.6 3.5 5.5",
  settings: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-2.9 1.2V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-2.9-1.2l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.7 1.7 0 0 0 3 14H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.2-2.9l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.7 1.7 0 0 0 10 3.1V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 2.9 1.2l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1A1.7 1.7 0 0 0 21 10h.1a2 2 0 1 1 0 4H21a1.7 1.7 0 0 0-1.6 1z",
  logout: "M15 4h4v16h-4M10 8l-4 4 4 4M6 12h11",
  download: "M12 4v11M7 10l5 5 5-5M5 20h14",
  clock: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7v5l3 2",
};

export function icon(name, cls) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("class", "icon" + (cls ? " " + cls : ""));
  svg.setAttribute("aria-hidden", "true");
  if (name === "dots") svg.setAttribute("stroke-width", "3"); // dots need a heavier stroke
  const p = document.createElementNS(ns, "path");
  p.setAttribute("d", ICONS[name] || "");
  svg.append(p);
  return svg;
}

// iconButton is an accessible icon-only button.
export function iconButton(name, label, onclick, cls) {
  return h("button", { type: "button", class: "icon-btn" + (cls ? " " + cls : ""), "aria-label": label, title: label, onclick }, icon(name));
}

export function fmtTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const today = new Date();
  const opts = d.toDateString() === today.toDateString()
    ? { hour: "2-digit", minute: "2-digit" }
    : { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" };
  return d.toLocaleString([], opts);
}

export function fmtFull(iso) {
  const d = new Date(iso);
  return isNaN(d) ? "" : d.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
}

export function shortJSON(v, max = 80) {
  let s;
  try { s = JSON.stringify(v ?? {}); } catch { s = String(v); }
  return s.length > max ? s.slice(0, max - 1) + "…" : s;
}

// copyText copies to the clipboard. navigator.clipboard needs a secure
// context (https or localhost); on plain http over the LAN fall back to a
// hidden textarea and execCommand.
export async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch { /* fall through */ }
  const ta = h("textarea", { class: "sr-only", readonly: true });
  ta.value = text;
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { /* unsupported */ }
  ta.remove();
  return ok;
}

export const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
export const modKey = isMac ? "⌘" : "Ctrl";

// reducedMotion reports the user's motion preference.
export const reducedMotion = () => window.matchMedia("(prefers-reduced-motion: reduce)").matches;
