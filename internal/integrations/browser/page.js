// Page script of the Bosun browser integration. It runs in an isolated world
// (Page.createIsolatedWorld), so page scripts cannot replace the functions it
// uses; it shares the DOM with the page. It is evaluated as
// "(<this function>)(op, args)" and returns a JSON-serialisable value.
(function (op, a) {
  "use strict";
  const ATTR = "data-bosun-ref";
  const MAX = (a && a.max) || 30000;
  const ROLES = new Set(["button", "link", "checkbox", "radio", "tab", "menuitem", "menuitemcheckbox",
    "menuitemradio", "option", "switch", "combobox", "textbox", "searchbox", "slider", "spinbutton", "treeitem"]);
  const SKIP = new Set(["SCRIPT", "STYLE", "NOSCRIPT", "TEMPLATE", "HEAD", "META", "LINK", "OBJECT", "EMBED",
    "CANVAS", "SVG", "svg"]);
  const BLOCK = new Set(["ADDRESS", "ARTICLE", "ASIDE", "BLOCKQUOTE", "DD", "DIV", "DL", "DT", "FIELDSET",
    "FIGCAPTION", "FIGURE", "FOOTER", "FORM", "HEADER", "HR", "LI", "MAIN", "NAV", "OL", "P", "PRE", "SECTION",
    "TABLE", "TR", "UL", "BR", "TD", "TH", "CAPTION", "LABEL", "DETAILS", "DIALOG"]);
  const TEXTY = new Set(["", "text", "email", "search", "tel", "url", "number", "password", "date",
    "datetime-local", "month", "time", "week"]);

  function clean(s, n) {
    s = String(s || "").replace(/\s+/g, " ").trim();
    return s.length > n ? s.slice(0, n - 1) + "…" : s;
  }
  function visible(el) {
    try {
      if (el.checkVisibility && !el.checkVisibility({ checkVisibilityCSS: true })) return false;
    } catch (e) { /* older engines */ }
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  }
  function inputType(el) {
    return el.tagName === "INPUT" ? String(el.getAttribute("type") || "text").toLowerCase() : "";
  }
  function interactive(el) {
    const t = el.tagName;
    if (t === "A") return el.hasAttribute("href");
    if (t === "BUTTON" || t === "SELECT" || t === "TEXTAREA" || t === "SUMMARY") return true;
    if (t === "INPUT") return inputType(el) !== "hidden";
    const role = String(el.getAttribute("role") || "").toLowerCase();
    if (ROLES.has(role)) return true;
    if (el.isContentEditable && el.hasAttribute("contenteditable")) return true;
    return el.hasAttribute("onclick");
  }
  function roleOf(el) {
    const r = String(el.getAttribute("role") || "").toLowerCase();
    if (r) return r;
    switch (el.tagName) {
      case "A": return "link";
      case "BUTTON": case "SUMMARY": return "button";
      case "SELECT": return "combobox";
      case "TEXTAREA": return "textbox";
      case "INPUT": {
        const ty = inputType(el);
        if (["button", "submit", "reset", "image"].includes(ty)) return "button";
        if (ty === "checkbox" || ty === "radio") return ty;
        if (ty === "range") return "slider";
        if (ty === "search") return "searchbox";
        if (ty === "file") return "file";
        return "textbox";
      }
    }
    return el.isContentEditable ? "textbox" : "generic";
  }
  function textOf(el) {
    return el.innerText !== undefined ? el.innerText : el.textContent;
  }
  // labelText is a label's text without the text of the controls inside it
  // (a select's options, a button's label).
  function labelText(l) {
    const c = l.cloneNode(true);
    c.querySelectorAll("input,select,textarea,button").forEach((e) => e.remove());
    return c.textContent;
  }
  function nameOf(el) {
    let s = el.getAttribute("aria-label");
    if (!s) {
      const lb = el.getAttribute("aria-labelledby");
      if (lb) s = lb.split(/\s+/).map((id) => { const e = document.getElementById(id); return e ? textOf(e) : ""; }).join(" ");
    }
    if (!s && el.labels && el.labels.length) s = Array.from(el.labels).map(labelText).join(" ");
    const ty = inputType(el);
    if (!s && ["button", "submit", "reset"].includes(ty)) s = el.value || (ty === "submit" ? "Submit" : "");
    if (!s && ty === "image") s = el.getAttribute("alt");
    if (!s && !["INPUT", "SELECT", "TEXTAREA"].includes(el.tagName)) s = textOf(el);
    if (!s && el.querySelector) { const img = el.querySelector("img[alt]"); if (img) s = img.getAttribute("alt"); }
    if (!s) s = el.getAttribute("title") || el.getAttribute("placeholder") || el.getAttribute("name") || "";
    return clean(s, 100);
  }
  function formOf(el) {
    return el.form || (el.closest ? el.closest("form") : null);
  }
  function info(el) {
    const f = formOf(el);
    const ty = inputType(el);
    const tag = el.tagName;
    return {
      tag: tag,
      type: tag === "BUTTON" ? String(el.getAttribute("type") || "").toLowerCase() : ty,
      role: roleOf(el),
      name: nameOf(el),
      text: clean(textOf(el), 200),
      value: ["button", "submit", "reset"].includes(ty) ? clean(el.value, 100) : "",
      aria_label: clean(el.getAttribute("aria-label"), 100),
      title: clean(el.getAttribute("title"), 100),
      in_form: !!f,
      form_method: f ? String(f.getAttribute("method") || "get").toLowerCase() : "",
      form_action: f ? String(f.action || "") : "",
      form_has_password: f ? !!f.querySelector("input[type=password]") : false,
      href: tag === "A" ? String(el.href || "") : "",
      editable: (tag === "INPUT" && TEXTY.has(ty)) || tag === "TEXTAREA" || el.isContentEditable ||
        ["textbox", "searchbox"].includes(String(el.getAttribute("role") || "").toLowerCase()),
      password: ty === "password",
      disabled: !!el.disabled || el.getAttribute("aria-disabled") === "true",
      select: tag === "SELECT",
    };
  }
  // Every element with a ref, across open shadow roots.
  function allRefd(root, out) {
    root.querySelectorAll("[" + ATTR + "]").forEach((e) => out.push(e));
    root.querySelectorAll("*").forEach((e) => { if (e.shadowRoot) allRefd(e.shadowRoot, out); });
    return out;
  }
  function byRef(ref) {
    if (!/^ref_\d+$/.test(String(ref))) return null;
    const hits = allRefd(document, []).filter((e) => e.getAttribute(ATTR) === ref);
    return hits.length === 1 ? hits[0] : null;
  }
  let next = 0;
  const seen = new Set();
  function initRefs() {
    for (const e of allRefd(document, [])) {
      const m = /^ref_(\d+)$/.exec(e.getAttribute(ATTR) || "");
      if (m) next = Math.max(next, +m[1]);
    }
  }
  function refOf(el) {
    let r = el.getAttribute(ATTR);
    if (!r || !/^ref_\d+$/.test(r) || seen.has(r)) {
      r = "ref_" + (++next);
      el.setAttribute(ATTR, r);
    }
    seen.add(r);
    return r;
  }
  function shortHref(el) {
    try {
      const u = new URL(el.href, location.href);
      if (u.protocol === "javascript:") return "(script)";
      const s = u.origin === location.origin ? u.pathname + u.search : u.host + u.pathname;
      return clean(s, 80);
    } catch (e) { return ""; }
  }
  function describe(el) {
    const role = roleOf(el);
    let s = "[" + refOf(el) + "] " + role + " " + JSON.stringify(nameOf(el));
    const ty = inputType(el);
    if (el.tagName === "SELECT") {
      const sel = el.options[el.selectedIndex];
      s += " value=" + JSON.stringify(sel ? clean(sel.text, 60) : "");
      const opts = Array.from(el.options).slice(0, 25).map((o) => clean(o.text, 40));
      s += " options=" + JSON.stringify(opts) + (el.options.length > 25 ? " (+" + (el.options.length - 25) + " more)" : "");
    } else if (ty === "checkbox" || ty === "radio") {
      if (el.checked) s += " checked";
    } else if (ty === "password") {
      if (el.value) s += " value=(hidden)";
    } else if (el.tagName === "TEXTAREA" || (el.tagName === "INPUT" && TEXTY.has(ty))) {
      if (el.value) s += " value=" + JSON.stringify(clean(el.value, 100));
      else if (el.placeholder) s += " placeholder=" + JSON.stringify(clean(el.placeholder, 60));
    } else if (el.tagName === "A") {
      const h = shortHref(el);
      if (h) s += " -> " + h;
    }
    if (el.disabled || el.getAttribute("aria-disabled") === "true") s += " disabled";
    const st = stateInfo(el);
    if (st) s += " " + st;
    return s;
  }
  function stateInfo(el) {
    const i = info(el);
    if ((i.tag === "INPUT" && (i.type === "submit" || i.type === "image")) || (i.tag === "BUTTON" && (i.type === "submit" || (i.type === "" && i.in_form)))) return "(submits a form)";
    return "";
  }

  function snapshot() {
    initRefs();
    const lines = [];
    let total = 0, cur = "", truncated = false;
    const push = (l) => {
      if (truncated) return;
      if (total + l.length > MAX) { truncated = true; return; }
      lines.push(l); total += l.length + 1;
    };
    const flush = () => { const t = clean(cur, 4000); if (t) push(t); cur = ""; };
    const walk = (n) => {
      if (truncated) return;
      if (n.nodeType === 3) { cur += n.nodeValue + " "; return; }
      if (n.nodeType !== 1) return;
      const el = n;
      if (SKIP.has(el.tagName)) return;
      if (el.tagName === "IFRAME" || el.tagName === "FRAME") {
        if (visible(el)) { flush(); push("[embedded frame not shown]"); }
        return;
      }
      if (!visible(el) && el.tagName !== "BODY" && el.tagName !== "HTML") {
        // display:contents children can still be visible.
        if (getComputedStyle(el).display !== "contents") return;
      }
      if (interactive(el)) { flush(); push(describe(el)); return; }
      const tag = el.tagName;
      if (/^H[1-6]$/.test(tag)) { flush(); push("#".repeat(+tag[1]) + " " + clean(textOf(el), 200)); return; }
      if (tag === "IMG") { const alt = clean(el.getAttribute("alt"), 80); if (alt) cur += "[image: " + alt + "] "; return; }
      const block = BLOCK.has(tag);
      if (block) flush();
      for (const c of el.childNodes) walk(c);
      if (el.shadowRoot) for (const c of el.shadowRoot.childNodes) walk(c);
      if (block) flush();
    };
    if (document.body) walk(document.body);
    flush();
    return { url: location.href, title: document.title, text: lines.join("\n"), truncated: truncated,
      scroll_y: Math.round(window.scrollY), scroll_max: Math.max(0, Math.round(document.documentElement.scrollHeight - window.innerHeight)) };
  }

  function find(query) {
    initRefs();
    const words = String(query || "").toLowerCase().split(/\s+/).filter(Boolean);
    const out = [];
    const visit = (root) => {
      root.querySelectorAll("*").forEach((el) => {
        if (out.length >= 50) return;
        if (el.shadowRoot) visit(el.shadowRoot);
        if (!interactive(el) || !visible(el)) return;
        const hay = (roleOf(el) + " " + nameOf(el) + " " + (el.getAttribute("placeholder") || "") + " " +
          (el.getAttribute("name") || "") + " " + (el.tagName === "A" ? shortHref(el) : "")).toLowerCase();
        if (words.every((w) => hay.includes(w))) out.push(describe(el));
      });
    };
    visit(document);
    return { lines: out };
  }

  function closestInteractive(el) {
    for (let e = el; e && e.nodeType === 1; e = e.parentElement || (e.getRootNode && e.getRootNode().host)) {
      if (interactive(e)) return e;
    }
    return null;
  }

  function target(ref) {
    const el = byRef(ref);
    if (!el) return { error: "notfound" };
    el.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
    const r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0 || !visible(el)) return { error: "hidden", info: info(el) };
    const x = r.left + r.width / 2, y = r.top + r.height / 2;
    let hit = document.elementFromPoint(x, y);
    while (hit && hit.shadowRoot) {
      const h2 = hit.shadowRoot.elementFromPoint(x, y);
      if (!h2 || h2 === hit) break;
      hit = h2;
    }
    const inside = (h) => {
      for (let e = h; e; e = e.parentElement || (e.getRootNode && e.getRootNode().host)) if (e === el) return true;
      return false;
    };
    const res = { x: x, y: y, rect: { x: r.left, y: r.top, w: r.width, h: r.height }, info: info(el) };
    if (hit && !inside(hit)) {
      const c = closestInteractive(hit) || hit;
      res.covered = info(c);
    } else if (hit) {
      const c = closestInteractive(hit);
      if (c && c !== el) res.hit = info(c);
    }
    return res;
  }

  function prepareClick(ref) {
    const el = byRef(ref);
    if (!el) return { error: "notfound" };
    // Keep navigation in this tab: a new tab would be invisible to the agent.
    const anchor = el.closest ? el.closest("a[target]") : null;
    if (anchor) anchor.setAttribute("target", "_self");
    const f = formOf(el);
    if (f && f.getAttribute("target")) f.setAttribute("target", "_self");
    return { ok: true };
  }

  function focusSelect(ref) {
    const el = byRef(ref);
    if (!el) return { error: "notfound" };
    el.focus();
    if (typeof el.select === "function") el.select();
    else if (el.isContentEditable) {
      const range = document.createRange();
      range.selectNodeContents(el);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
    }
    return { ok: document.activeElement === el || el.contains(document.activeElement) };
  }

  function selectOption(ref, value) {
    const el = byRef(ref);
    if (!el) return { error: "notfound" };
    if (el.tagName !== "SELECT") return { error: "notselect" };
    const want = String(value).trim().toLowerCase();
    const opts = Array.from(el.options);
    const o = opts.find((o) => o.value === value) || opts.find((o) => clean(o.text, 1000).toLowerCase() === want) ||
      opts.find((o) => clean(o.text, 1000).toLowerCase().includes(want));
    if (!o) return { error: "nooption", options: opts.slice(0, 25).map((o) => clean(o.text, 40)) };
    el.value = o.value;
    el.dispatchEvent(new Event("input", { bubbles: true }));
    el.dispatchEvent(new Event("change", { bubbles: true }));
    return { ok: true, text: clean(o.text, 80) };
  }

  function formDetails(ref) {
    const el = byRef(ref);
    if (!el) return { error: "notfound" };
    const f = formOf(el);
    const out = { element: { role: roleOf(el), name: nameOf(el), text: clean(textOf(el), 200) }, url: location.href };
    if (f) {
      const fields = [];
      let hidden = 0;
      for (const c of Array.from(f.elements)) {
        const ty = inputType(c);
        if (ty === "hidden") { hidden++; continue; }
        if (["submit", "button", "reset", "image"].includes(ty) || c.tagName === "BUTTON" || c.tagName === "FIELDSET" || c.tagName === "OBJECT" || c.tagName === "OUTPUT") continue;
        if ((ty === "checkbox" || ty === "radio") && !c.checked) continue;
        let v;
        if (ty === "password") v = c.value ? "•••••• (hidden)" : "";
        else if (c.tagName === "SELECT") { const s = c.options[c.selectedIndex]; v = s ? s.text : ""; }
        else if (ty === "file") v = "(file upload not supported)";
        else v = c.value;
        fields.push({ name: clean(c.getAttribute("name") || nameOf(c), 60), type: ty || c.tagName.toLowerCase(), value: clean(v, 200) });
        if (fields.length >= 40) break;
      }
      out.form = { method: String(f.getAttribute("method") || "get").toLowerCase(), action: String(f.action || ""), fields: fields, hidden_fields: hidden };
    }
    return out;
  }

  function highlight(ref, on) {
    const id = "__bosun_highlight";
    const old = document.getElementById(id);
    if (old) old.remove();
    if (!on) return { ok: true };
    const el = byRef(ref);
    if (!el) return { error: "notfound" };
    el.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
    const r = el.getBoundingClientRect();
    const d = document.createElement("div");
    d.id = id;
    d.style.cssText = "position:fixed;pointer-events:none;z-index:2147483647;box-sizing:border-box;" +
      "border:4px solid #e0115f;border-radius:4px;box-shadow:0 0 0 4000px rgba(0,0,0,.12);" +
      "left:" + (r.left - 6) + "px;top:" + (r.top - 6) + "px;width:" + (r.width + 12) + "px;height:" + (r.height + 12) + "px";
    document.documentElement.appendChild(d);
    return { ok: true };
  }

  function loginFields() {
    const vis = (e) => visible(e) && !e.disabled && !e.readOnly;
    const all = Array.from(document.querySelectorAll("input")).filter(vis);
    const pw = all.find((e) => inputType(e) === "password");
    const userTypes = new Set(["", "text", "email", "tel"]);
    let user = null;
    const scope = pw ? (formOf(pw) || document) : document;
    const cands = Array.from(scope.querySelectorAll("input")).filter((e) => vis(e) && userTypes.has(inputType(e)));
    user = cands.find((e) => /username|email/i.test(e.getAttribute("autocomplete") || "")) ||
      cands.find((e) => inputType(e) === "email") ||
      (pw ? cands.filter((e) => e.compareDocumentPosition(pw) & Node.DOCUMENT_POSITION_FOLLOWING).pop() : null) ||
      cands.find((e) => /user|login|email|account/i.test((e.getAttribute("name") || "") + " " + (e.id || "")));
    initRefs();
    return { user: user ? refOf(user) : "", password: pw ? refOf(pw) : "" };
  }

  function scroll(dir) {
    const h = window.innerHeight * 0.8;
    if (dir === "up") window.scrollBy(0, -h);
    else if (dir === "down") window.scrollBy(0, h);
    else if (dir === "top") window.scrollTo(0, 0);
    else if (dir === "bottom") window.scrollTo(0, document.documentElement.scrollHeight);
    return { scroll_y: Math.round(window.scrollY), scroll_max: Math.max(0, Math.round(document.documentElement.scrollHeight - window.innerHeight)) };
  }

  switch (op) {
    case "snapshot": return snapshot();
    case "find": return find(a.query);
    case "target": return target(a.ref);
    case "prepareClick": return prepareClick(a.ref);
    case "focusSelect": return focusSelect(a.ref);
    case "select": return selectOption(a.ref, a.value);
    case "formDetails": return formDetails(a.ref);
    case "highlight": return highlight(a.ref, a.on);
    case "loginFields": return loginFields();
    case "scroll": return scroll(a.direction);
    case "state": return { url: location.href, title: document.title, ready: document.readyState };
  }
  return { error: "unknown op" };
})
