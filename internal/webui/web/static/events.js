// The user's event stream (GET /v1/events) with reconnect and backoff.
//
// It uses fetch streaming, not EventSource: the API accepts the token only
// in the Authorization header. Connection state is published so the shell
// can show a "disconnected, retrying" banner.
import { stream } from "./api.js";

export const connection = {
  state: "connecting", // "connecting" | "open" | "retrying"
  retryAt: 0,          // ms timestamp of the next attempt while retrying
  listeners: new Set(),
  retryNow: () => {},
  set(state, retryAt = 0) {
    this.state = state;
    this.retryAt = retryAt;
    this.listeners.forEach((f) => f(this));
  },
};

// startEvents follows the stream until the returned stop() is called.
// onEvent(type, data) gets every event; onConnect(first) runs after each
// successful (re)connect so callers can resync state from REST.
export function startEvents(onEvent, onConnect) {
  const ctl = new AbortController();
  let first = true;
  (async () => {
    let delay = 1000;
    while (!ctl.signal.aborted) {
      try {
        await stream("GET", "/v1/events", undefined, onEvent, ctl.signal, () => {
          delay = 1000;
          connection.set("open");
          onConnect?.(first);
          first = false;
        });
      } catch (e) {
        if (ctl.signal.aborted || e.status === 401) return;
      }
      if (ctl.signal.aborted) return;
      // The stream ended or failed: wait (with jitter), unless the user
      // asks to retry now.
      const wait = delay * (0.8 + Math.random() * 0.4);
      connection.set("retrying", Date.now() + wait);
      await new Promise((resolve) => {
        const t = setTimeout(resolve, wait);
        connection.retryNow = () => { clearTimeout(t); resolve(); };
      });
      connection.retryNow = () => {};
      if (!ctl.signal.aborted) connection.set("connecting");
      delay = Math.min(delay * 2, 30000);
    }
  })();
  return () => ctl.abort();
}
