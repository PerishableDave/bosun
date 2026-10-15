// Shared UI pieces: toasts, a screen-reader announcer, a confirm dialog
// (native <dialog>), and popup menus.
import { h, icon } from "./dom.js";

// ---------- toasts ----------

let toastBox = null;

// toast shows a short message. opts: {error: bool, action: {label, run}, ms}.
export function toast(message, opts = {}) {
  if (!toastBox || !toastBox.isConnected) {
    toastBox = h("div", { class: "toasts", role: "status", "aria-live": "polite" });
    document.body.append(toastBox);
  }
  const t = h("div", { class: "toast" + (opts.error ? " error" : "") }, h("span", {}, message));
  if (opts.action) {
    t.append(h("button", { type: "button", onclick: () => { t.remove(); opts.action.run(); } }, opts.action.label));
  }
  toastBox.append(t);
  setTimeout(() => t.remove(), opts.ms || (opts.error ? 6000 : 3000));
}

// errorToast shows an API or other error.
export function errorToast(e, prefix) {
  toast((prefix ? prefix + ": " : "") + (e && e.message ? e.message : String(e)), { error: true });
}

// ---------- screen-reader announcements ----------

let live = null;

// announce speaks text through a polite live region.
export function announce(text) {
  if (!live || !live.isConnected) {
    live = h("div", { class: "sr-only", "aria-live": "polite", "aria-atomic": "true" });
    document.body.append(live);
  }
  live.textContent = "";
  // A separate task so assistive tech notices the change.
  setTimeout(() => { live.textContent = text; }, 50);
}

// ---------- confirm dialog ----------

// confirmDialog resolves true if the user confirms. opts: {title, body,
// confirm: label, danger: bool}.
export function confirmDialog({ title, body, confirm = "OK", danger = false }) {
  return new Promise((resolve) => {
    const ok = h("button", { type: "submit", class: "btn " + (danger ? "danger solid" : "primary"), value: "ok" }, confirm);
    const cancel = h("button", { type: "submit", class: "btn", value: "cancel", formnovalidate: true }, "Cancel");
    const dlg = h("dialog", { "aria-labelledby": "dlg-title" },
      h("form", { method: "dialog" },
        h("h3", { id: "dlg-title" }, title),
        body ? h("p", {}, body) : null,
        h("div", { class: "dialog-actions" }, cancel, ok)));
    dlg.addEventListener("close", () => {
      resolve(dlg.returnValue === "ok");
      dlg.remove();
    });
    document.body.append(dlg);
    dlg.showModal();
    (danger ? cancel : ok).focus();
  });
}

// ---------- popup menu ----------

let openMenu = null;

export function closeMenu() {
  if (!openMenu) return;
  const { el, anchor, onKey, onDown } = openMenu;
  openMenu = null;
  el.remove();
  document.removeEventListener("keydown", onKey, true);
  document.removeEventListener("pointerdown", onDown, true);
  anchor.setAttribute("aria-expanded", "false");
}

// showMenu opens a menu under anchor. items: [{label, icon, run, danger}] or
// "-" for a separator.
export function showMenu(anchor, items) {
  const wasOpen = openMenu && openMenu.anchor === anchor;
  closeMenu();
  if (wasOpen) return; // the anchor toggles its menu
  const buttons = [];
  const el = h("div", { class: "menu", role: "menu" }, items.map((it) => {
    if (it === "-") return h("hr");
    const b = h("button", { type: "button", role: "menuitem", class: it.danger ? "danger" : "", onclick: () => { closeMenu(); it.run(); } },
      it.icon ? icon(it.icon) : null, it.label);
    buttons.push(b);
    return b;
  }));
  document.body.append(el);
  const r = anchor.getBoundingClientRect();
  const w = el.offsetWidth, hgt = el.offsetHeight;
  let left = Math.min(r.right - w, window.innerWidth - w - 8);
  if (left < 8) left = Math.max(8, r.left);
  let top = r.bottom + 4;
  if (top + hgt > window.innerHeight - 8) top = Math.max(8, r.top - hgt - 4);
  el.style.left = left + "px";
  el.style.top = top + "px";
  anchor.setAttribute("aria-expanded", "true");
  const onKey = (e) => {
    const i = buttons.indexOf(document.activeElement);
    if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closeMenu(); anchor.focus(); }
    else if (e.key === "ArrowDown") { e.preventDefault(); buttons[(i + 1) % buttons.length].focus(); }
    else if (e.key === "ArrowUp") { e.preventDefault(); buttons[(i - 1 + buttons.length) % buttons.length].focus(); }
    else if (e.key === "Tab") closeMenu();
  };
  const onDown = (e) => { if (!el.contains(e.target) && !anchor.contains(e.target)) closeMenu(); };
  document.addEventListener("keydown", onKey, true);
  document.addEventListener("pointerdown", onDown, true);
  openMenu = { el, anchor, onKey, onDown };
  buttons[0]?.focus();
}
