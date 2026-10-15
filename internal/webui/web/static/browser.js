// Browser page: what the assistant's web browser is showing, with a Stop
// button. Polls GET /v1/browser (and the screenshot every 2s while a task is
// active).
import { api, imageURL } from "./api.js";
import { h, fmtTime } from "./dom.js";

export function renderBrowser(el) {
  const state = h("p", { class: "muted" }, "Loading…");
  const title = h("strong", {});
  const url = h("span", { class: "browser-url muted small" });
  const steps = h("span", { class: "small" });
  const err = h("p", { class: "error", role: "alert" });
  const shot = h("div", { class: "browser-shot" });
  const stop = h("button", { class: "btn", hidden: true, onclick: async () => {
    stop.disabled = true;
    try {
      await api("POST", "/v1/browser/stop");
      err.textContent = "";
    } catch (e) {
      err.textContent = e.message;
    }
    stop.disabled = false;
    tick(true);
  } }, "Stop browser");
  el.replaceChildren(h("div", {},
    h("h2", {}, "Browser"),
    h("p", { class: "muted small" }, "Your assistant's private browser. Actions that change your accounts always ask first; " +
      "to finish a sensitive step (like paying) yourself, deny the request and do it on the site directly."),
    h("div", { class: "browser-head" }, h("div", {}, title, h("br"), url), steps, stop),
    state, err, shot));

  let timer = null, objURL = null, gone = false, lastShot = 0;
  const showImage = async () => {
    try {
      const u = await imageURL("/v1/browser/screenshot");
      if (gone) return URL.revokeObjectURL(u);
      if (objURL) URL.revokeObjectURL(objURL);
      objURL = u;
      shot.replaceChildren(h("img", { src: u, alt: "Current browser page" }));
    } catch {
      /* no screenshot yet */
    }
  };
  const tick = async (force) => {
    if (gone) return;
    let st;
    try {
      st = await api("GET", "/v1/browser");
    } catch (e) {
      if (e.status === 501) {
        state.textContent = "The browser integration is not enabled on this server (integrations.browser in bosun.yaml).";
        return;
      }
      err.textContent = e.message;
      timer = setTimeout(tick, 5000);
      return;
    }
    err.textContent = "";
    stop.hidden = !st.running;
    title.textContent = st.running ? (st.title || "(untitled)") : "";
    url.textContent = st.running ? st.url || "" : "";
    steps.textContent = st.steps ? `${st.steps} of ${st.step_budget} steps` + (st.last_step_at ? " · last " + fmtTime(st.last_step_at) : "") : "";
    state.textContent = !st.running ? "Not running. It starts when the assistant opens a page."
      : st.active ? "Working…" : "Idle.";
    if (!st.running) {
      shot.replaceChildren();
    } else if (st.has_screenshot && (force || st.active || Date.now() - lastShot > 30000)) {
      lastShot = Date.now();
      await showImage();
    }
    if (!gone) timer = setTimeout(tick, st.active ? 2000 : 10000);
  };
  tick(true);
  return () => {
    gone = true;
    clearTimeout(timer);
    if (objURL) URL.revokeObjectURL(objURL);
  };
}
