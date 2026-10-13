// Tools and Memory pages.
import { api } from "./api.js";
import { h, clear, fmtTime } from "./dom.js";

function errorLine(e) { return h("p", { class: "error" }, e.message || String(e)); }

// ---------- Tools ----------

export async function renderTools(el) {
  el.replaceChildren(h("p", { class: "muted" }, "Loading…"));
  let tools, rules;
  try {
    [tools, rules] = await Promise.all([api("GET", "/v1/tools"), api("GET", "/v1/policy")]);
  } catch (e) { el.replaceChildren(errorLine(e)); return; }
  const explicit = new Set((rules.rules || []).map((r) => r.capability));
  const msg = h("p", { class: "error" });

  const rows = (tools.tools || []).map((t) => {
    const sel = h("select", { "aria-label": "Decision for " + t.capability },
      ["allow", "ask", "deny"].map((d) => h("option", { value: d, selected: d === t.decision }, d)));
    const reset = h("button", { class: "btn small", disabled: !explicit.has(t.capability) }, "reset");
    const note = h("span", { class: "muted small" }, explicit.has(t.capability) ? "custom rule" : "default");
    const done = (decision, isExplicit) => {
      sel.value = decision;
      reset.disabled = !isExplicit;
      note.textContent = isExplicit ? "custom rule" : "default";
      msg.textContent = "";
    };
    sel.addEventListener("change", async () => {
      try {
        await api("PUT", "/v1/policy/" + encodeURIComponent(t.capability), { decision: sel.value });
        done(sel.value, true);
      } catch (e) { msg.textContent = e.message; sel.value = t.decision; }
    });
    reset.addEventListener("click", async () => {
      try {
        await api("DELETE", "/v1/policy/" + encodeURIComponent(t.capability));
        const fresh = (await api("GET", "/v1/tools")).tools.find((x) => x.name === t.name);
        t.decision = fresh ? fresh.decision : t.decision;
        done(t.decision, false);
      } catch (e) { msg.textContent = e.message; }
    });
    return h("tr", {},
      h("td", { "data-label": "Tool" }, h("strong", {}, t.name), h("div", { class: "muted small" }, t.description)),
      h("td", { "data-label": "Risk" }, h("span", { class: "risk risk-" + t.risk }, t.risk)),
      h("td", { "data-label": "Decision" }, sel, " ", reset, " ", note));
  });

  const d = rules.defaults || {};
  el.replaceChildren(
    h("h2", {}, "Tools"),
    h("p", { class: "muted small" }, `Defaults: read ${d.read}, write ${d.write}, destructive ${d.destructive}. A custom rule overrides the default for that tool.`),
    msg,
    rows.length
      ? h("table", { class: "grid" }, h("thead", {}, h("tr", {}, h("th", {}, "Tool"), h("th", {}, "Risk"), h("th", {}, "Decision"))), h("tbody", {}, rows))
      : h("p", { class: "muted" }, "No tools are enabled. Enable integrations in bosun.yaml."));
}

// ---------- Memory ----------

const KINDS = ["note", "fact", "preference", "summary"];

export function renderMemory(el) {
  const search = h("input", { type: "search", placeholder: "Search memory…", "aria-label": "Search memory" });
  const list = h("div", { class: "mem-list" });
  const msg = h("p", { class: "error" });
  const kind = h("select", { "aria-label": "Kind" }, KINDS.map((k) => h("option", { value: k }, k)));
  const text = h("input", { type: "text", placeholder: "Add a memory…", maxlength: "4000", "aria-label": "New memory" });

  async function load() {
    const q = search.value.trim();
    try {
      const res = await api("GET", "/v1/memory?limit=200" + (q ? "&q=" + encodeURIComponent(q) : ""));
      draw(res.memories || []);
      msg.textContent = "";
    } catch (e) { msg.textContent = e.message; }
  }

  function draw(items) {
    clear(list);
    if (!items.length) list.append(h("p", { class: "muted" }, search.value.trim() ? "No matches." : "No memories yet."));
    for (const m of items) list.append(memoryItem(m));
  }

  function memoryItem(m) {
    const body = h("div", { class: "mem-text" }, m.content);
    const row = h("div", { class: "mem" }, h("span", { class: "tag" }, m.kind), body,
      h("div", { class: "mem-meta muted small" }, (m.source === "user" ? "added by you" : m.source) + " · " + fmtTime(m.updated_at)));
    const actions = h("div", { class: "mem-actions" });
    const view = () => {
      actions.replaceChildren(
        h("button", { class: "btn small", onclick: edit }, "Edit"),
        h("button", { class: "btn small danger", onclick: remove }, "Delete"));
    };
    async function remove() {
      if (!confirm("Delete this memory?")) return;
      try { await api("DELETE", "/v1/memory/" + m.id); row.remove(); } catch (e) { msg.textContent = e.message; }
    }
    function edit() {
      const ta = h("textarea", { rows: "3", maxlength: "4000" }, m.content);
      body.replaceChildren(ta);
      actions.replaceChildren(
        h("button", { class: "btn small primary", onclick: async () => {
          try {
            const u = await api("PUT", "/v1/memory/" + m.id, { content: ta.value });
            m.content = u.content; m.updated_at = u.updated_at;
            body.textContent = m.content;
            view();
          } catch (e) { msg.textContent = e.message; }
        } }, "Save"),
        h("button", { class: "btn small", onclick: () => { body.textContent = m.content; view(); } }, "Cancel"));
      ta.focus();
    }
    view();
    row.append(actions);
    return row;
  }

  const add = async (e) => {
    e.preventDefault();
    const content = text.value.trim();
    if (!content) return;
    try {
      await api("POST", "/v1/memory", { kind: kind.value, content });
      text.value = "";
      search.value = "";
      await load();
    } catch (err) { msg.textContent = err.message; }
  };

  let timer;
  search.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(load, 250); });
  el.replaceChildren(
    h("h2", {}, "Memory"),
    h("p", { class: "muted small" }, "Things Bosun remembers about you. Some are saved automatically after a chat; you can add, edit, or delete any of them."),
    h("form", { class: "mem-add", onsubmit: add }, kind, text, h("button", { class: "btn primary", type: "submit" }, "Add")),
    search, msg, list);
  load();
  return () => clearTimeout(timer);
}
