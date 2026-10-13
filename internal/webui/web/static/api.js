// API client: bearer token in sessionStorage, JSON helpers, SSE over fetch.

const KEY = "bosun.token";

export const auth = {
  get token() {
    try { return sessionStorage.getItem(KEY) || ""; } catch { return ""; }
  },
  set token(v) {
    try { v ? sessionStorage.setItem(KEY, v) : sessionStorage.removeItem(KEY); } catch { /* storage blocked */ }
  },
  onExpired: () => {},
};

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

function headers(json) {
  const h = {};
  if (auth.token) h.Authorization = "Bearer " + auth.token;
  if (json) h["Content-Type"] = "application/json";
  return h;
}

async function toError(res, login) {
  let code = "http_" + res.status, msg = res.statusText;
  try {
    const b = await res.json();
    if (b.error) { code = b.error.code; msg = b.error.message; }
  } catch { /* not JSON */ }
  if (res.status === 401 && code === "unauthorized" && !login) auth.onExpired();
  return new ApiError(res.status, code, msg);
}

// api performs a JSON request and returns the parsed body (null for 204).
export async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: headers(body !== undefined),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw await toError(res, path === "/v1/login");
  return res.status === 204 ? null : res.json();
}

// stream issues a request and calls onEvent(type, data) for every SSE event
// until the stream ends or signal aborts. It throws ApiError for a non-2xx
// response (before streaming starts).
export async function stream(method, path, body, onEvent, signal) {
  const res = await fetch(path, {
    method,
    headers: { ...headers(body !== undefined), Accept: "text/event-stream" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });
  if (!res.ok) throw await toError(res);
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true }).replace(/\r\n/g, "\n");
    let i;
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const frame = buf.slice(0, i);
      buf = buf.slice(i + 2);
      let type = "message", data = "";
      for (const line of frame.split("\n")) {
        if (line.startsWith("event:")) type = line.slice(6).trim();
        else if (line.startsWith("data:")) data += line.slice(5).trim();
        // ": ping" comments and unknown fields are ignored
      }
      if (!data) continue;
      let parsed;
      try { parsed = JSON.parse(data); } catch { continue; }
      onEvent(type, parsed);
    }
  }
}
