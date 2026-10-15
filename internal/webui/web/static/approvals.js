// Approval cards (used inline in chat and on the Approvals page) and the
// global pending-approvals state fed by GET /v1/approvals and GET /v1/events.
import { api, stream, imageURL } from "./api.js";
import { h, shortJSON } from "./dom.js";

export const pending = {
  list: [],
  listeners: new Set(),
  async refresh() {
    try {
      this.list = (await api("GET", "/v1/approvals")).approvals || [];
    } catch { return; }
    this.listeners.forEach((f) => f(this.list));
  },
};

// resolve answers an approval. A 409 (someone else answered first) is not an
// error for the user; the approval_resolved event updates the card.
export async function resolve(id, decision, always) {
  try {
    await api("POST", "/v1/approvals/" + encodeURIComponent(id), always ? { decision, always: true } : { decision });
  } catch (e) {
    if (e.status !== 409) throw e;
  }
}

const LABEL = { approve: "Approved", deny: "Denied", expired: "Expired", canceled: "Canceled" };

// approvalCard renders {id, tool, args, risk, scope, has_preview} with
// Approve/Deny buttons, the preview image if there is one, and "Approve and
// always allow on <scope>" for scoped, non-destructive approvals.
export function approvalCard({ id, tool, args, risk, scope, has_preview }) {
  const status = h("span", { class: "appr-status" });
  const buttons = h("span", { class: "appr-actions" });
  const preview = has_preview ? h("a", { class: "appr-preview", target: "_blank", rel: "noopener", title: "Open full size" }) : null;
  const card = h("div", { class: "approval", "data-approval-id": id },
    h("div", { class: "appr-head" }, "Approval needed: ", h("strong", {}, tool), " ", h("span", { class: "risk risk-" + risk }, risk),
      scope ? h("span", { class: "appr-scope" }, " on ", h("strong", {}, scope)) : null),
    preview,
    h("code", { class: "appr-args" }, shortJSON(args, 600)),
    h("div", { class: "appr-foot" }, buttons, status));
  if (preview) {
    imageURL("/v1/approvals/" + encodeURIComponent(id) + "/preview").then((u) => {
      preview.href = u;
      preview.append(h("img", { src: u, alt: "Preview of what this action will do" }));
    }).catch(() => preview.remove());
  }
  const answer = async (decision, always) => {
    buttons.querySelectorAll("button").forEach((b) => (b.disabled = true));
    try {
      await resolve(id, decision, always);
    } catch (e) {
      status.textContent = e.message;
      buttons.querySelectorAll("button").forEach((b) => (b.disabled = false));
    }
  };
  buttons.append(
    h("button", { class: "btn primary", onclick: () => answer("approve") }, "Approve"),
    scope && risk !== "destructive"
      ? h("button", { class: "btn", onclick: () => answer("approve", true), title: "Also allow " + tool + " on " + scope + " from now on (undo on the Tools page or with bosun policy unset)" },
        "Approve and always allow on " + scope)
      : null,
    h("button", { class: "btn", onclick: () => answer("deny") }, "Deny"));
  return card;
}

// markResolved updates every card for an approval in the document.
export function markResolved(id, decision) {
  for (const card of document.querySelectorAll(`[data-approval-id="${CSS.escape(id)}"]`)) {
    card.classList.add("resolved", "res-" + decision);
    card.querySelector(".appr-actions")?.remove();
    card.querySelector(".appr-status").textContent = LABEL[decision] || decision;
  }
}

// startEvents follows GET /v1/events (fetch streaming: the API accepts the
// token only in the Authorization header, so EventSource cannot be used).
// It reconnects until stop() is called and resyncs after each reconnect.
export function startEvents(onEvent) {
  const ctl = new AbortController();
  (async () => {
    let delay = 1000;
    while (!ctl.signal.aborted) {
      try {
        await pending.refresh();
        await stream("GET", "/v1/events", undefined, (type, data) => {
          delay = 1000;
          if (type === "approval_required" || type === "approval_resolved") {
            if (type === "approval_resolved") markResolved(data.approval_id, data.decision);
            pending.refresh();
          }
          onEvent(type, data);
        }, ctl.signal);
      } catch (e) {
        if (ctl.signal.aborted || e.status === 401) return;
      }
      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 2, 15000);
    }
  })();
  return () => ctl.abort();
}

// renderApprovalsPage fills el with the pending list; it stays current via
// pending.listeners.
export function renderApprovalsPage(el) {
  const draw = (list) => {
    el.replaceChildren(h("div", {},
      h("h2", {}, "Pending approvals"),
      list.length === 0 ? h("p", { class: "muted" }, "Nothing is waiting for you.") : null,
      list.map((a) => h("div", { class: "approval-row" },
        approvalCard({ id: a.id, tool: a.tool, args: a.args, risk: a.risk, scope: a.scope, has_preview: a.has_preview }),
        h("div", { class: "muted small" }, a.conversation_id ? "conversation " + a.conversation_id : "no conversation")))));
  };
  draw(pending.list);
  pending.listeners.add(draw);
  return () => pending.listeners.delete(draw);
}
