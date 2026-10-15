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

// imageURL fetches an image (with the bearer token, which <img src> cannot
// send) and returns an object URL for it; the caller revokes it.
export async function imageURL(path) {
  const res = await fetch(path, { headers: headers(false), cache: "no-store" });
  if (!res.ok) throw await toError(res);
  return URL.createObjectURL(await res.blob());
}

// unsupported reports whether e means this daemon lacks the endpoint (an
// older server): no route (the API's catch-all 404, or a non-JSON 404), a
// path that exists only for other methods (405), or 501.
export function unsupported(e) {
  if (!(e instanceof ApiError)) return false;
  if (e.status === 405 || e.status === 501) return true;
  return e.status === 404 && (e.code.startsWith("http_") || e.message === "no such endpoint");
}

// download fetches path with the bearer token and saves the response as a
// file (the server's Content-Disposition name, or fallbackName).
export async function download(path, fallbackName) {
  const res = await fetch(path, { headers: headers(false), cache: "no-store" });
  if (!res.ok) throw await toError(res);
  const cd = res.headers.get("Content-Disposition") || "";
  const m = cd.match(/filename\*?=(?:UTF-8'')?"?([^";]+)"?/i);
  saveBlob(await res.blob(), m ? decodeURIComponent(m[1]) : fallbackName);
}

// saveBlob offers blob to the user as a file download.
export function saveBlob(blob, name) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

// stream issues a request and calls onEvent(type, data) for every SSE event
// until the stream ends or signal aborts. It throws ApiError for a non-2xx
// response (before streaming starts). onOpen, if given, runs once the
// response headers have arrived.
export async function stream(method, path, body, onEvent, signal, onOpen) {
  const res = await fetch(path, {
    method,
    headers: { ...headers(body !== undefined), Accept: "text/event-stream" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });
  if (!res.ok) throw await toError(res);
  onOpen?.();
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
