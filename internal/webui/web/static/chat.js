// Chat page: conversation list, history, composer, and the streamed turn.
import { api, stream } from "./api.js";
import { approvalCard, markResolved } from "./approvals.js";
import { h, shortJSON, fmtTime } from "./dom.js";
import { render } from "./md.js";

const RESULT_PREVIEW = 600;

export function renderChat(el) {
  let convs = [];
  let current = "";        // open conversation id
  let busy = false;        // a turn is streaming
  let abort = null;

  const list = h("div", { class: "conv-list" });
  const log = h("div", { class: "log" });
  const input = h("textarea", { rows: "2", placeholder: "Message Bosun…  (Enter to send, Shift+Enter for a new line)" });
  const send = h("button", { class: "btn primary", onclick: () => submit() }, "Send");
  const sidebar = h("aside", { class: "sidebar" },
    h("button", { class: "btn primary block", onclick: () => newConversation() }, "New conversation"),
    list);
  const toggle = h("button", { class: "btn small side-toggle", onclick: (e) => { e.stopPropagation(); sidebar.classList.toggle("open"); } }, "Conversations");
  el.replaceChildren(h("div", { class: "chat" }, sidebar,
    h("section", { class: "main" }, toggle, log,
      h("div", { class: "composer" }, input, send))));

  el.querySelector(".main").addEventListener("click", () => sidebar.classList.remove("open")); // phone drawer
  log.append(h("p", { class: "muted pad" }, "Start a new conversation or pick one on the left."));
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); submit(); }
  });

  function setBusy(b) {
    busy = b;
    input.disabled = send.disabled = b;
    if (!b) input.focus();
  }

  function scroll() { log.scrollTop = log.scrollHeight; }

  function drawList() {
    list.replaceChildren(...convs.map((c) => h("button", {
      class: "conv" + (c.id === current ? " active" : ""),
      onclick: () => open(c.id),
    }, h("span", { class: "conv-title" }, c.title || "Untitled"), h("span", { class: "muted small" }, fmtTime(c.updated_at)))));
    if (!convs.length) list.append(h("p", { class: "muted small pad" }, "No conversations yet."));
  }

  async function loadList() {
    convs = (await api("GET", "/v1/conversations")).conversations || [];
    drawList();
  }

  async function newConversation() {
    if (busy) return;
    const c = await api("POST", "/v1/conversations", {});
    convs.unshift(c);
    await open(c.id);
  }

  async function open(id) {
    if (busy) return;
    current = id;
    sidebar.classList.remove("open");
    drawList();
    log.replaceChildren(h("p", { class: "muted pad" }, "Loading…"));
    let conv;
    try { conv = await api("GET", "/v1/conversations/" + encodeURIComponent(id)); } catch (e) {
      log.replaceChildren(h("p", { class: "error pad" }, e.message));
      return;
    }
    log.replaceChildren();
    renderHistory(conv.messages || []);
    if (!log.children.length) log.append(h("p", { class: "muted pad" }, "Say something to get started."));
    scroll();
    input.focus();
  }

  // --- rendering blocks (shared by history and live turns) ---

  const bubble = (role, text) => h("div", { class: "msg " + role, html: render(text) });

  function toolRow(id, name, args) {
    const result = h("div", { class: "tool-result muted" }, "running…");
    const row = h("details", { class: "tool", "data-tool-id": id },
      h("summary", {}, "⚙ " + name + "(" + shortJSON(args, 70) + ")"),
      h("pre", { class: "tool-args" }, JSON.stringify(args ?? {}, null, 2)),
      result);
    row.result = result;
    return row;
  }

  function setResult(row, text) {
    row.result.textContent = text.length > RESULT_PREVIEW ? text.slice(0, RESULT_PREVIEW) + "…" : text;
    row.result.classList.remove("muted");
    row.classList.toggle("failed", text.startsWith("error: "));
    if (text.startsWith("error: ")) row.open = true;
  }

  function renderHistory(messages) {
    const rows = {};
    for (const m of messages) {
      for (const b of m.content || []) {
        if (b.type === "text" && b.text && (m.role === "user" || m.role === "assistant")) {
          log.append(bubble(m.role, b.text));
        } else if (b.type === "tool_use") {
          rows[b.id] = toolRow(b.id, b.name, b.input);
          log.append(rows[b.id]);
        } else if (b.type === "tool_result" && rows[b.tool_use_id]) {
          setResult(rows[b.tool_use_id], typeof b.content === "string" ? b.content : JSON.stringify(b.content));
        }
      }
    }
  }

  // --- sending a message ---

  async function submit() {
    const text = input.value.trim();
    if (!text || busy) return;
    if (!current) await newConversation();
    const convID = current;
    input.value = "";
    log.querySelector(".pad")?.remove();
    log.append(bubble("user", text));
    setBusy(true);
    abort = new AbortController();

    let cur = null, curText = "";  // assistant bubble being streamed
    const rows = {};
    const finish = (msg) => {
      if (msg) log.append(h("div", { class: "msg error" }, msg));
      scroll();
    };
    const onEvent = (type, d) => {
      switch (type) {
        case "text":
          if (!cur) { cur = bubble("assistant", ""); curText = ""; log.append(cur); }
          curText += d.delta;
          cur.innerHTML = render(curText);
          break;
        case "tool_call":
          cur = null;
          rows[d.id] = toolRow(d.id, d.name, d.args);
          log.append(rows[d.id]);
          break;
        case "tool_result":
          if (rows[d.id]) setResult(rows[d.id], d.summary);
          break;
        case "approval_required":
          cur = null;
          log.append(approvalCard({ id: d.approval_id, tool: d.tool, args: d.args, risk: d.risk, scope: d.scope, has_preview: d.has_preview }));
          break;
        case "error":
          finish(d.message);
          break;
        case "approval_resolved":
          markResolved(d.approval_id, d.decision);
          break;
        // done needs no rendering.
      }
      scroll();
    };

    try {
      await stream("POST", "/v1/conversations/" + encodeURIComponent(convID) + "/messages", { content: text }, onEvent, abort.signal);
    } catch (e) {
      if (e.name !== "AbortError") finish(e.message);
    }
    setBusy(false);
    loadList().catch(() => {});
  }

  loadList().then(() => convs[0] && open(convs[0].id)).catch((e) => log.replaceChildren(h("p", { class: "error pad" }, e.message)));


}
