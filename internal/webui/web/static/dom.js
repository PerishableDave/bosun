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

export function fmtTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const today = new Date();
  const opts = d.toDateString() === today.toDateString()
    ? { hour: "2-digit", minute: "2-digit" }
    : { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" };
  return d.toLocaleString([], opts);
}

export function shortJSON(v, max = 80) {
  let s;
  try { s = JSON.stringify(v ?? {}); } catch { s = String(v); }
  return s.length > max ? s.slice(0, max - 1) + "…" : s;
}
