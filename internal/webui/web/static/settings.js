// Settings page: account, password change, theme, keyboard shortcuts.
import { api } from "./api.js";
import { h, modKey } from "./dom.js";
import { prefs } from "./prefs.js";
import { toast } from "./ui.js";

export const SHORTCUTS = [
  [modKey + " K", "Search conversations"],
  [modKey + " Shift O", "New chat"],
  [modKey + " B", "Show or hide the sidebar"],
  ["Enter", "Send the message"],
  ["Shift Enter", "New line in the message"],
  ["Esc", "Close a menu, the drawer, or cancel a rename"],
  ["↑ ↓", "Move between conversations in the list"],
];

export function renderSettings(el, user) {
  // ----- password -----
  const cur = h("input", { type: "password", autocomplete: "current-password", required: true });
  const pw = h("input", { type: "password", autocomplete: "new-password", required: true, minlength: "8" });
  const again = h("input", { type: "password", autocomplete: "new-password", required: true });
  const msg = h("p", { class: "error small", role: "alert" });
  const save = h("button", { type: "submit", class: "btn primary" }, "Change password");
  const form = h("form", { onsubmit: async (e) => {
    e.preventDefault();
    msg.textContent = "";
    if (pw.value !== again.value) { msg.textContent = "The new passwords do not match."; return; }
    save.disabled = true;
    try {
      await api("PUT", "/v1/me/password", { current: cur.value, new: pw.value });
      form.reset();
      toast("Password changed");
    } catch (ex) {
      msg.textContent = ex.status === 403 ? "The current password is wrong." : ex.message;
    } finally {
      save.disabled = false;
    }
  } },
  h("label", {}, "Current password", cur),
  h("label", {}, "New password (at least 8 characters)", pw),
  h("label", {}, "Repeat new password", again),
  msg,
  h("div", {}, save));

  // ----- theme -----
  const choices = [["auto", "Auto"], ["light", "Light"], ["dark", "Dark"]];
  const seg = h("div", { class: "seg", role: "radiogroup", "aria-label": "Theme" });
  const drawSeg = () => {
    seg.replaceChildren(...choices.map(([v, label]) => {
      const on = prefs.theme === v;
      return h("button", { type: "button", role: "radio", "aria-checked": String(on), tabindex: on ? "0" : "-1",
        onclick: () => { prefs.theme = v; drawSeg(); seg.querySelector('[aria-checked="true"]').focus(); } }, label);
    }));
  };
  seg.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
    e.preventDefault();
    const i = choices.findIndex(([v]) => v === prefs.theme);
    const n = (i + (e.key === "ArrowRight" ? 1 : choices.length - 1)) % choices.length;
    prefs.theme = choices[n][0];
    drawSeg();
    seg.querySelector('[aria-checked="true"]').focus();
  });
  drawSeg();

  el.replaceChildren(h("div", { class: "settings" },
    h("h2", {}, "Settings"),
    h("section", {},
      h("h3", {}, "Account"),
      h("p", { class: "muted", }, "Signed in as ", h("strong", {}, user.username), " ", h("span", { class: "tag" }, user.role))),
    h("section", {}, h("h3", {}, "Password"), form),
    h("section", {},
      h("h3", {}, "Appearance"),
      h("p", { class: "muted small" }, "Auto follows your device's light or dark setting."),
      seg),
    h("section", {},
      h("h3", {}, "Keyboard shortcuts"),
      h("table", { class: "keys" }, h("tbody", {}, SHORTCUTS.map(([k, d]) =>
        h("tr", {}, h("td", {}, k.split(" ").map((x) => [h("kbd", {}, x), " "])), h("td", {}, d))))))));
}
