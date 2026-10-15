// Chat view: history, the streamed turn, tool rows, approvals, composer.
//
// Each open conversation has a View (its own log element). A view whose
// turn is still running is kept when the user switches away, so the stream
// (and its pending approval) survives; finished views are dropped and
// reloaded from the server when reopened.
import { api, stream, unsupported } from "./api.js";
import { approvalCard, markResolved, pending } from "./approvals.js";
import { convs } from "./conversations.js";
import * as act from "./convactions.js";
import { h, icon, iconButton, fmtTime, fmtFull, copyText, modKey, reducedMotion } from "./dom.js";
import { render } from "./md.js";
import { drafts } from "./prefs.js";
import { announce, errorToast, toast } from "./ui.js";

const RESULT_PREVIEW = 4000; // characters of a tool result shown
const CHUNK = 40;            // messages rendered per history step

const EXAMPLES = [
  ["Plan my week", "Summarize what's on my calendar for the next 7 days and point out any conflicts."],
  ["Catch up on email", "What important emails did I get today? Give me a short summary of each."],
  ["Look something up", "Search the web for a quick weeknight dinner recipe with chicken and rice."],
];

const nowISO = () => new Date().toISOString();
const sec = (ms) => (ms < 1000 ? ms + "ms" : (ms / 1000).toFixed(ms < 10000 ? 1 : 0) + "s");

// ---------- message blocks (shared by history and live turns) ----------

function timeEl(iso) {
  return h("time", { datetime: iso, title: fmtFull(iso) }, fmtTime(iso));
}

function userMsg(text, iso) {
  return h("div", { class: "msg user" },
    h("div", { class: "bubble" }, text),
    h("div", { class: "msg-meta" }, timeEl(iso)));
}

// assistantMsg returns {el, append(delta), finish()}; while streaming the
// markdown is re-rendered at most once per animation frame.
function assistantMsg(text, iso, streaming, onChange) {
  let raw = text;
  const md = h("div", { class: "md", html: render(raw) });
  const copy = iconButton("copy", "Copy message", async () => {
    if (await copyText(raw)) {
      copy.replaceChildren(icon("check"));
      setTimeout(() => copy.replaceChildren(icon("copy")), 1500);
    } else toast("Could not copy", { error: true });
  }, "sm");
  const el = h("div", { class: "msg assistant" + (streaming ? " streaming" : "") }, md,
    h("div", { class: "msg-meta" }, copy, timeEl(iso)));
  let queued = false;
  const paint = () => { queued = false; md.innerHTML = render(raw); onChange?.(); };
  return {
    el,
    get text() { return raw; },
    append(delta) {
      raw += delta;
      if (!queued) { queued = true; requestAnimationFrame(paint); }
    },
    finish() {
      el.classList.remove("streaming");
      paint();
      if (!raw.trim()) el.remove(); // nothing was said (tool-only step)
    },
  };
}

// toolGroup returns the group consecutive tool rows fold into: the last
// element of container if it is one, else a new group.
function toolGroup(container) {
  const last = container.lastElementChild;
  if (last && last.group) return last.group;
  const label = h("span", {});
  const head = h("button", { type: "button", class: "tool-group-head", "aria-expanded": "false" }, icon("chevron", "chev"), icon("tool"), label);
  const rows = h("div", { class: "tool-rows" });
  const el = h("div", { class: "tool-group" }, head, rows);
  head.hidden = true;
  const g = {
    el, rows, count: 0, failed: 0, running: 0,
    update() {
      el.classList.toggle("multi", g.count > 1);
      el.classList.toggle("failed", g.failed > 0);
      head.hidden = g.count < 2;
      label.textContent = g.count + " tool calls" + (g.running ? " · running" : "") + (g.failed ? " · " + g.failed + " failed" : "");
    },
  };
  head.addEventListener("click", () => {
    const open = el.classList.toggle("open");
    head.setAttribute("aria-expanded", String(open));
  });
  el.group = g;
  container.append(el);
  return g;
}

// toolRow adds a folded row for one call; returns {setResult(text, ms, isError)}.
function toolRow(group, name, args) {
  const dur = h("span", { class: "tool-dur" });
  const spin = h("span", { class: "spin", "aria-label": "running" });
  const result = h("pre", { class: "tool-result" }, "running…");
  const el = h("details", { class: "tool running" },
    h("summary", {}, icon("chevron", "chev"), spin, h("span", { class: "tool-name" }, name), dur),
    h("div", { class: "tool-body" },
      h("div", { class: "tool-label" }, "Arguments"),
      h("pre", { class: "tool-args" }, JSON.stringify(args ?? {}, null, 2)),
      h("div", { class: "tool-label" }, "Result"),
      result));
  group.rows.append(el);
  group.count++;
  group.running++;
  group.update();
  let done = false;
  return {
    setResult(text, ms, isError) {
      if (done) return;
      done = true;
      spin.remove();
      el.classList.remove("running");
      group.running--;
      text = String(text ?? "");
      result.textContent = text.length > RESULT_PREVIEW ? text.slice(0, RESULT_PREVIEW) + "…" : text;
      if (ms != null && ms >= 0) dur.textContent = "· " + sec(ms);
      if (isError || text.startsWith("error: ")) {
        el.classList.add("failed");
        group.failed++;
      }
      group.update();
    },
  };
}

function resultText(content) {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) return content.map((c) => (c && c.text) || JSON.stringify(c)).join("\n");
  return JSON.stringify(content);
}

// renderMessages appends stored messages to container. results maps a
// tool_use id to {text, at} from the tool_result blocks.
function renderMessages(container, messages, results) {
  for (const m of messages) {
    for (const b of m.content || []) {
      if (b.type === "text" && b.text && m.role === "user") {
        container.append(userMsg(b.text, m.created_at));
      } else if (b.type === "text" && b.text && m.role === "assistant") {
        container.append(assistantMsg(b.text, m.created_at).el);
      } else if (b.type === "tool_use") {
        const row = toolRow(toolGroup(container), b.name, b.input);
        const r = results.get(b.id);
        if (r) row.setResult(r.text, new Date(r.at) - new Date(m.created_at), r.error);
        else row.setResult("(no result recorded)");
      }
    }
  }
}

// ---------- the chat view ----------

// createChat builds the chat page. shell: {setHeader(h), navigate(hash)}.
export function createChat(shell) {
  const views = new Map(); // conversation id -> View kept while its turn runs
  let view = null;

  const scroller = h("div", { class: "log-scroll" });
  const jump = h("button", { type: "button", class: "btn small jump", hidden: true, onclick: () => toBottom(true) }, icon("down"), "Jump to latest");
  const input = h("textarea", { rows: "1", placeholder: "Message Bosun", "aria-label": "Message", enterkeyhint: "send" });
  const sendBtn = h("button", { type: "button", class: "send-btn", "aria-label": "Send message", title: "Send (Enter)" }, icon("send"));
  const composerWrap = h("div", { class: "composer-wrap" },
    h("div", { class: "composer" }, input, sendBtn),
    h("div", { class: "composer-hint" }, "Enter to send · Shift+Enter for a new line · ", modKey, "+Shift+O new chat"));
  const el = h("div", { class: "chat" }, scroller, jump, composerWrap);

  new ResizeObserver(() => el.style.setProperty("--composer-h", composerWrap.offsetHeight + "px")).observe(composerWrap);

  // ----- composer -----
  function autosize() {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, window.innerHeight * 0.4) + "px";
  }
  let draftTimer;
  input.addEventListener("input", () => {
    autosize();
    updateComposer();
    clearTimeout(draftTimer);
    const v = view;
    draftTimer = setTimeout(() => drafts.set(v.id, input.value), 300);
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      if (!view.busy) submit();
    }
  });
  sendBtn.addEventListener("click", () => (view.busy ? stop(view) : submit()));

  function updateComposer() {
    if (!view) return;
    const busy = view.busy;
    sendBtn.classList.toggle("stop", busy);
    sendBtn.replaceChildren(icon(busy ? "stop" : "send"));
    sendBtn.setAttribute("aria-label", busy ? "Stop" : "Send message");
    sendBtn.title = busy ? "Stop the reply" : "Send (Enter)";
    sendBtn.disabled = busy ? view.stopRequested : !input.value.trim();
    input.placeholder = busy ? "Bosun is replying… (you can keep typing)" : "Message Bosun";
  }

  // ----- scrolling: stick to the bottom unless the user scrolled up -----
  scroller.addEventListener("scroll", () => {
    if (!view) return;
    view.stick = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 60;
    jump.hidden = view.stick;
  }, { passive: true });

  function toBottom(smooth) {
    if (!view) return;
    view.stick = true;
    jump.hidden = true;
    scroller.scrollTo({ top: scroller.scrollHeight, behavior: smooth && !reducedMotion() ? "smooth" : "auto" });
  }

  // changed runs after v's log grew.
  function changed(v) {
    if (v !== view) return;
    if (v.stick) scroller.scrollTop = scroller.scrollHeight;
    else jump.hidden = false;
  }

  // Copy buttons in rendered code blocks (markdown HTML has no handlers).
  scroller.addEventListener("click", async (e) => {
    const btn = e.target.closest(".md-copy");
    if (!btn) return;
    const code = btn.closest(".md-code")?.querySelector("pre code");
    if (code && (await copyText(code.textContent))) {
      btn.textContent = "Copied";
      setTimeout(() => (btn.textContent = "Copy"), 1500);
    }
  });

  // ----- views -----
  function makeView(id) {
    return { id, log: h("div", { class: "log" }), busy: false, stick: true, scrollTop: 0, stopRequested: false, abort: null };
  }

  function leave() {
    if (!view) return;
    clearTimeout(draftTimer);
    drafts.set(view.id, input.value);
    view.scrollTop = scroller.scrollTop;
    if (!view.busy) views.delete(view.id);
  }

  function show(v) {
    view = v;
    scroller.replaceChildren(v.log);
    input.value = drafts.get(v.id);
    autosize();
    updateComposer();
    updateHeader();
    requestAnimationFrame(() => {
      if (v.stick) scroller.scrollTop = scroller.scrollHeight;
      else scroller.scrollTop = v.scrollTop;
      jump.hidden = v.stick;
    });
  }

  function emptyState(v) {
    const pick = (text) => {
      input.value = text;
      autosize();
      updateComposer();
      drafts.set(v.id, text);
      input.focus();
      input.setSelectionRange(text.length, text.length);
    };
    return h("div", { class: "empty" },
      h("span", { class: "logo", "aria-hidden": "true" }, "B"),
      h("h1", {}, "What can I help with?"),
      h("p", {}, "Ask Bosun about your email, calendar, the web, or anything you've told it to remember."),
      h("div", { class: "examples" }, EXAMPLES.map(([title, text]) =>
        h("button", { type: "button", class: "example", onclick: () => pick(text) }, h("small", {}, title), text))));
  }

  function routineName(c) {
    if (!c) return "";
    if (c.source === "routine") return c.routine_name || (c.title || "").replace(/^routine:\s*/, "") || "a routine";
    const m = /^routine:\s*(.+)$/.exec(c.title || "");
    return m ? m[1] : "";
  }

  async function load(v) {
    v.log.replaceChildren(h("p", { class: "note" }, "Loading…"));
    let conv;
    try {
      conv = await api("GET", "/v1/conversations/" + encodeURIComponent(v.id));
    } catch (e) {
      v.log.replaceChildren(h("div", { class: "msg error", role: "alert" }, h("span", { class: "err-text" },
        e.status === 404 ? "This conversation does not exist (it may have been deleted)." : e.message)));
      return;
    }
    const { messages = [], ...meta } = conv;
    const c = convs.remember(meta);
    if (v === view) updateHeader();
    v.log.replaceChildren();
    const routine = routineName(c);
    if (routine) v.log.append(h("div", { class: "routine-note" }, icon("clock"), "Run by routine ", h("strong", {}, routine)));

    const results = new Map();
    for (const m of messages) {
      for (const b of m.content || []) {
        if (b.type === "tool_result") results.set(b.tool_use_id, { text: resultText(b.content), at: m.created_at, error: !!b.is_error });
      }
    }
    // Render the newest messages first; older ones on request, keeping the
    // scroll position.
    let start = Math.max(0, messages.length - CHUNK);
    renderMessages(v.log, messages.slice(start), results);
    if (start > 0) {
      const more = h("button", { type: "button", class: "btn small show-earlier" }, "Show earlier messages");
      more.addEventListener("click", () => {
        const from = Math.max(0, start - CHUNK);
        const tmp = h("div");
        renderMessages(tmp, messages.slice(from, start), results);
        start = from;
        const before = scroller.scrollHeight;
        more.after(...tmp.childNodes);
        if (start === 0) more.remove();
        scroller.scrollTop += scroller.scrollHeight - before;
      });
      v.log.prepend(more);
    }
    if (!messages.length && !routine) v.log.append(emptyState(v));
    // Approvals still waiting in this conversation (e.g. a turn started
    // from another device).
    for (const a of pending.list) {
      if (a.conversation_id === v.id) addApproval(v, a);
    }
    if (v === view) toBottom(false);
  }

  function addApproval(v, a) {
    if (v.log.querySelector(`[data-approval-id="${CSS.escape(a.id)}"]`)) return;
    v.log.append(approvalCard({ id: a.id, tool: a.tool, args: a.args, risk: a.risk, scope: a.scope, has_preview: a.has_preview }));
    changed(v);
  }

  // ----- header -----
  function updateHeader() {
    if (!view) return;
    const c = view.id ? convs.get(view.id) : null;
    if (!c) {
      shell.setHeader({ title: view.id ? "Loading…" : "New chat" });
      return;
    }
    shell.setHeader({
      title: act.displayTitle(c),
      chip: c.source === "routine" || c.source === "telegram" ? c.source : "",
      onRename: (t) => act.rename(c.id, t),
      menu: (startRename) => act.menuItems(c.id, { onRename: startRename, onDeleted: () => deleted(c.id) }),
    });
  }
  convs.listeners.add(() => updateHeader());

  // ----- sending -----
  async function submit(text = input.value) {
    const v = view;
    text = text.trim();
    if (!text || v.busy) return;
    if (!v.id) {
      let c;
      try {
        c = await convs.create();
      } catch (e) {
        errorToast(e, "Could not start a conversation");
        return;
      }
      if (v !== view) return;
      v.id = c.id;
      views.set(c.id, v);
      drafts.move("", c.id);
      // Show the first message as the title until the server names it.
      convs.remember({ id: c.id, title: c.title || (text.length > 40 ? text.slice(0, 39).trimEnd() + "…" : text) });
      convs.changed();
      history.replaceState(null, "", "#chat/" + encodeURIComponent(c.id));
      updateHeader();
    }
    input.value = "";
    drafts.clear(v.id);
    autosize();
    runTurn(v, text);
  }

  async function runTurn(v, text) {
    v.log.querySelector(".empty")?.remove();
    v.busy = true;
    v.stopRequested = false;
    v.lastUser = text;
    v.log.append(userMsg(text, nowISO()));
    let thinking = h("div", { class: "thinking", "aria-label": "Bosun is thinking" }, h("span"), h("span"), h("span"));
    v.log.append(thinking);
    v.stick = true;
    changed(v);
    if (v === view) updateComposer();
    convs.touch(v.id);

    let cur = null;              // assistant message being streamed
    let replyText = "";
    const rows = new Map();      // tool call id -> {row, start}
    let outcome = "", errMsg = "";
    const ctl = new AbortController();
    v.abort = ctl;
    const endText = () => {
      if (cur) { replyText += cur.text; cur.finish(); cur = null; }
    };

    const onEvent = (type, d) => {
      if (thinking) { thinking.remove(); thinking = null; }
      switch (type) {
        case "text":
          if (!cur) {
            cur = assistantMsg("", nowISO(), true, () => changed(v));
            v.log.append(cur.el);
          }
          cur.append(d.delta || "");
          break;
        case "tool_call":
          endText();
          rows.set(d.id, { row: toolRow(toolGroup(v.log), d.name, d.args), start: Date.now() });
          break;
        case "tool_result": {
          const r = rows.get(d.id);
          if (r) r.row.setResult(d.summary, Date.now() - r.start);
          break;
        }
        case "approval_required":
          endText();
          addApproval(v, { id: d.approval_id, tool: d.tool, args: d.args, risk: d.risk, scope: d.scope, has_preview: d.has_preview });
          announce("Approval needed: " + d.tool);
          break;
        case "approval_resolved":
          markResolved(d.approval_id, d.decision);
          break;
        case "done":
          outcome = d.stopped ? "stopped" : "done";
          break;
        case "error":
          if (v.stopRequested || /cancel/i.test(d.message || "")) outcome = "stopped";
          else { outcome = "error"; errMsg = d.message || "The turn failed."; }
          break;
      }
      changed(v);
    };

    try {
      await stream("POST", "/v1/conversations/" + encodeURIComponent(v.id) + "/messages", { content: text }, onEvent, ctl.signal);
    } catch (e) {
      if (e.name === "AbortError") outcome = "stopped";
      else if (!outcome) { outcome = "error"; errMsg = e.message || String(e); }
    }
    clearTimeout(v.stopTimer);
    if (thinking) thinking.remove();
    endText();
    for (const { row } of rows.values()) row.setResult(outcome === "stopped" ? "error: stopped" : "(no result)");
    if (!outcome) {
      outcome = v.stopRequested ? "stopped" : "error";
      errMsg = "The connection was lost before the reply finished.";
    }
    if (outcome === "stopped") {
      v.log.append(h("div", { class: "note" }, "Stopped."));
      announce("Stopped");
    } else if (outcome === "error") {
      addError(v, errMsg);
      announce("Error: " + errMsg);
    } else {
      announce("Bosun replied: " + replyText.slice(0, 400));
    }
    v.busy = false;
    v.abort = null;
    changed(v);
    convs.touch(v.id);
    convs.refreshSoon(1500); // pick up the server's title
    if (v === view) {
      updateComposer();
      if (document.activeElement === document.body) input.focus();
    } else {
      views.delete(v.id);
    }
  }

  function addError(v, msg) {
    const text = v.lastUser;
    const row = h("div", { class: "msg error", role: "alert" },
      h("span", { class: "err-text" }, msg),
      text ? h("button", { type: "button", class: "btn small", onclick: () => {
        if (v.busy) return;
        row.remove();
        runTurn(v, text);
      } }, icon("retry"), "Retry") : null);
    v.log.append(row);
  }

  async function stop(v) {
    if (!v.busy || v.stopRequested) return;
    v.stopRequested = true;
    if (v === view) updateComposer();
    try {
      await api("POST", "/v1/conversations/" + encodeURIComponent(v.id) + "/stop");
      // The stream should end on its own; don't wait forever.
      v.stopTimer = setTimeout(() => v.abort?.abort(), 5000);
    } catch (e) {
      // An older daemon has no stop endpoint: closing the stream cancels
      // the turn just the same.
      if (!unsupported(e) && e.status !== 404 && e.status !== 409) errorToast(e, "Stop");
      v.abort?.abort();
    }
  }

  function deleted(id) {
    const v = views.get(id);
    v?.abort?.abort();
    views.delete(id);
    drafts.clear(id);
    if (view && view.id === id) shell.navigate("#chat");
  }

  // ----- public -----
  return {
    el,
    currentId: () => (view ? view.id : ""),
    // open shows conversation id ("" for a new chat).
    open(id) {
      if (view && view.id === id && (id || !view.busy)) return;
      leave();
      if (!id) {
        const v = makeView("");
        v.log.append(emptyState(v));
        show(v);
        return;
      }
      let v = views.get(id);
      if (!v) {
        v = makeView(id);
        views.set(id, v);
        show(v);
        load(v);
      } else show(v);
      convs.markRead(id);
      convs.changed();
    },
    focus() { input.focus(); },
    deleted,
    // onEvent handles the user's event stream for turns not started here.
    onEvent(type, d) {
      const v = view;
      if (!v || !v.id) return;
      if (type === "approval_required" && d.conversation_id === v.id && !v.busy) {
        addApproval(v, { id: d.approval_id, tool: d.tool, args: d.args, risk: d.risk, scope: d.scope, has_preview: d.has_preview });
      } else if (type === "routine_finished" && d.conversation_id === v.id && !v.busy) {
        load(v);
      } else if (type === "conversation_updated" && d.id === v.id && d.unread) {
        convs.markRead(v.id);
      }
    },
    // hasRunningTurn lets the shell warn before the page unloads.
    hasRunningTurn: () => [...views.values()].some((v) => v.busy) || (view && view.busy),
  };
}
