// A small, hand-written markdown renderer for model and user text.
//
// Supported: ATX headings, paragraphs with hard line breaks, **bold**,
// *italic*, ~~strike~~, `inline code`, fenced code blocks (language label and
// a copy button), ordered and unordered lists (nested by indentation),
// blockquotes, tables (GFM pipes, with alignment), horizontal rules, links
// ([text](url) and bare http(s) URLs).
//
// Safety: the input is never interpreted as HTML. Every piece of source text
// is HTML-escaped before it is placed in the output, link targets are limited
// to http, https and mailto, and the only attributes emitted are fixed class
// names, escaped hrefs/titles, and target/rel. The result is safe to assign
// to innerHTML. This module has no DOM dependency, so it can be tested in
// isolation.
//
// Streaming: text that is still arriving (an unclosed fence, half a table)
// renders as well as it can and is simply re-rendered on the next chunk.

export function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

// safeURL returns url if it uses an allowed scheme, else "".
export function safeURL(url) {
  const u = String(url).trim();
  return /^(https?:\/\/|mailto:)/i.test(u) ? u : "";
}

// ---------- inline ----------

// Placeholders stand in for already-rendered HTML (code spans, links) while
// emphasis is applied to the escaped remainder. \u0001 cannot occur in the
// input: render strips control characters first.
const PH = "\u0001";

const INLINE_TOKEN = new RegExp(
  [
    "(`+)([^`]|[^`][\\s\\S]*?[^`])\\1(?!`)", // 1,2: code span
    "\\[([^\\]\\n]+)\\]\\(\\s*<?([^\\s)>]+)>?(?:\\s+\"([^\"\\n]*)\")?\\s*\\)", // 3,4,5: [text](url "title")
    "(https?:\\/\\/[^\\s<>\"`]+)", // 6: bare URL
  ].join("|"),
  "g",
);

function emphasis(s) {
  return s
    .replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[^\w])__(?=\S)([\s\S]*?\S)__(?!\w)/g, "$1<strong>$2</strong>")
    .replace(/(^|[^*\w])\*(?=[^\s*])([^*]*?[^\s*])\*(?!\*)/g, "$1<em>$2</em>")
    .replace(/(^|[^\w])_(?=[^\s_])([^_]*?[^\s_])_(?!\w)/g, "$1<em>$2</em>")
    .replace(/~~(?=\S)([\s\S]*?\S)~~/g, "<del>$1</del>");
}

function link(href, inner, title) {
  return '<a href="' + esc(href) + '" target="_blank" rel="noopener noreferrer" title="' + esc(title || href) + '">' + inner + "</a>";
}

// trimURL drops trailing punctuation that belongs to the sentence, keeping a
// closing parenthesis only when the URL opened one.
function trimURL(u) {
  let end = u.length;
  for (;;) {
    const c = u[end - 1];
    if (".,:;!?'".includes(c)) { end--; continue; }
    if (c === ")") {
      const s = u.slice(0, end);
      if ((s.match(/\(/g) || []).length < (s.match(/\)/g) || []).length) { end--; continue; }
    }
    break;
  }
  return u.slice(0, end);
}

// inline renders one block's inline content. Single newlines become <br>.
export function inline(text, noLinks) {
  const saved = [];
  const keep = (html) => PH + (saved.push(html) - 1) + PH;
  let src = "";
  let last = 0;
  for (const m of text.matchAll(INLINE_TOKEN)) {
    let token = m[0];
    let html;
    if (m[1] !== undefined) {
      html = "<code>" + esc(m[2].replace(/^ (.*) $/s, "$1")) + "</code>";
    } else if (m[3] !== undefined) {
      const href = safeURL(m[4]);
      html = href && !noLinks ? link(href, inline(m[3], true), m[5] ? m[5] + " — " + href : href) : null;
    } else {
      token = trimURL(m[6]);
      html = noLinks ? null : link(token, esc(token));
    }
    if (html === null) continue; // leave the text as is
    src += text.slice(last, m.index) + keep(html);
    last = m.index + token.length;
  }
  src += text.slice(last);
  // Escape, apply emphasis, restore placeholders, and turn newlines into
  // breaks (a trailing backslash or two trailing spaces mean the same).
  const out = emphasis(esc(src))
    .replace(/(?: {2,}|\\)?\n/g, "<br>");
  return out.replace(new RegExp(PH + "(\\d+)" + PH, "g"), (_, i) => saved[+i]);
}

// ---------- blocks ----------

const RE = {
  fence: /^ {0,3}(`{3,}|~{3,})\s*([^`\s]*)[^`]*$/,
  heading: /^ {0,3}(#{1,6})(?:\s+(.*?))?(?:\s+#+)?\s*$/,
  hr: /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/,
  quote: /^ {0,3}> ?/,
  item: /^( *)([-*+]|\d{1,9}[.)])(?: +(.*)|$)/,
  tableSep: /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/,
};

const indentOf = (line) => line.match(/^ */)[0].length;

// splitRow splits a table row on unescaped pipes outside code spans.
function splitRow(line) {
  let s = line.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
  const cells = [];
  let cur = "", code = false;
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (c === "\\" && s[i + 1] === "|") { cur += "|"; i++; continue; }
    if (c === "`") code = !code;
    if (c === "|" && !code) { cells.push(cur.trim()); cur = ""; continue; }
    cur += c;
  }
  cells.push(cur.trim());
  return cells;
}

function table(header, sep, rows) {
  const align = splitRow(sep).map((c) => (c.startsWith(":") && c.endsWith(":") ? "c" : c.endsWith(":") ? "r" : ""));
  const cell = (tag, text, i) => "<" + tag + (align[i] ? ' class="al-' + align[i] + '"' : "") + ">" + inline(text) + "</" + tag + ">";
  const head = splitRow(header);
  let out = '<div class="md-table"><table><thead><tr>' + head.map((t, i) => cell("th", t, i)).join("") + "</tr></thead>";
  if (rows.length) {
    out += "<tbody>" + rows.map((r) => {
      const cells = splitRow(r);
      while (cells.length < head.length) cells.push("");
      return "<tr>" + cells.slice(0, head.length).map((t, i) => cell("td", t, i)).join("") + "</tr>";
    }).join("") + "</tbody>";
  }
  return out + "</table></div>";
}

function codeBlock(lang, body) {
  const l = lang.replace(/[^\w+#.-]/g, "").slice(0, 20);
  return '<div class="md-code"><div class="md-code-head"><span class="md-code-lang">' + esc(l || "text") +
    '</span><button type="button" class="md-copy" aria-label="Copy code">Copy</button></div><pre><code' +
    (l ? ' class="lang-' + esc(l) + '"' : "") + ">" + esc(body) + "</code></pre></div>";
}

// startsBlock reports whether line begins a non-paragraph block (so it ends
// a running paragraph).
function startsBlock(line, next) {
  return RE.fence.test(line) || RE.heading.test(line) || RE.hr.test(line) || RE.quote.test(line) ||
    RE.item.test(line) || (line.includes("|") && next !== undefined && RE.tableSep.test(next) && next.includes("-"));
}

// list parses a list starting at lines[i]; returns [html, nextIndex].
function list(lines, i) {
  const first = lines[i].match(RE.item);
  const base = first[1].length;
  const ordered = /\d/.test(first[2]);
  const startNum = ordered ? parseInt(first[2], 10) : 1;
  const items = []; // each: array of de-indented content lines
  let loose = false;
  let cur = null;
  let contentIndent = 0;
  for (; i < lines.length; i++) {
    const line = lines[i];
    const m = line.match(RE.item);
    if (m && m[1].length === base && /\d/.test(m[2]) === ordered) {
      cur = [m[3] || ""];
      items.push(cur);
      contentIndent = base + m[2].length + 1 + (m[3] ? (line.slice(base + m[2].length).match(/^ */)[0].length - 1) : 0);
      continue;
    }
    if (line.trim() === "") {
      // A blank line continues the list only if more indented content or
      // another item of this list follows.
      let j = i + 1;
      while (j < lines.length && lines[j].trim() === "") j++;
      if (j >= lines.length) break;
      const n = lines[j].match(RE.item);
      const sameList = n && n[1].length === base && /\d/.test(n[2]) === ordered;
      if (!sameList && indentOf(lines[j]) <= base) break;
      if (sameList) loose = true;
      cur.push("");
      continue;
    }
    if (indentOf(line) > base) {
      cur.push(line.slice(Math.min(indentOf(line), Math.max(contentIndent, base + 2))));
      continue;
    }
    // A lazy continuation line of the item's paragraph.
    if (!startsBlock(line, lines[i + 1]) && cur[cur.length - 1].trim() !== "") {
      cur.push(line.trim());
      continue;
    }
    break;
  }
  const tag = ordered ? "ol" : "ul";
  let html = "<" + tag + (ordered && startNum !== 1 ? ' start="' + startNum + '"' : "") + ">";
  for (const it of items) {
    while (it.length && it[it.length - 1].trim() === "") it.pop();
    let body = blocks(it);
    if (!loose) body = body.replace(/^<p>([\s\S]*?)<\/p>/, "$1"); // tight list: no paragraph margin
    const task = body.match(/^\[([ xX])\] /);
    if (task) body = '<span class="md-task" aria-hidden="true">' + (task[1] === " " ? "☐" : "☑") + "</span> " + body.slice(4);
    html += "<li>" + body + "</li>";
  }
  return [html + "</" + tag + ">", i];
}

function blocks(lines) {
  let out = "";
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === "") { i++; continue; }

    let m = line.match(RE.fence);
    if (m) {
      const fence = m[1];
      const body = [];
      for (i++; i < lines.length; i++) {
        const close = lines[i].match(/^ {0,3}(`{3,}|~{3,})\s*$/);
        if (close && close[1][0] === fence[0] && close[1].length >= fence.length) { i++; break; }
        body.push(lines[i]);
      }
      out += codeBlock(m[2], body.join("\n"));
      continue;
    }
    if (RE.hr.test(line)) { out += "<hr>"; i++; continue; }
    m = line.match(RE.heading);
    if (m) {
      const n = m[1].length;
      out += "<h" + n + ">" + inline(m[2] || "") + "</h" + n + ">";
      i++;
      continue;
    }
    if (RE.quote.test(line)) {
      const body = [];
      while (i < lines.length && lines[i].trim() !== "" && (RE.quote.test(lines[i]) || !startsBlock(lines[i], lines[i + 1]))) {
        body.push(lines[i].replace(RE.quote, ""));
        i++;
      }
      out += "<blockquote>" + blocks(body) + "</blockquote>";
      continue;
    }
    if (RE.item.test(line)) {
      const [html, next] = list(lines, i);
      out += html;
      i = next;
      continue;
    }
    if (line.includes("|") && i + 1 < lines.length && RE.tableSep.test(lines[i + 1]) && lines[i + 1].includes("-")) {
      const header = line, sep = lines[i + 1];
      const rows = [];
      for (i += 2; i < lines.length && lines[i].trim() !== "" && lines[i].includes("|"); i++) rows.push(lines[i]);
      out += table(header, sep, rows);
      continue;
    }
    const para = [line.trim()];
    for (i++; i < lines.length && lines[i].trim() !== "" && !startsBlock(lines[i], lines[i + 1]); i++) para.push(lines[i].trim());
    out += "<p>" + inline(para.join("\n")) + "</p>";
  }
  return out;
}

// render returns safe HTML for markdown text.
export function render(text) {
  const src = String(text ?? "")
    .replace(/\r\n?/g, "\n")
    .replace(/\t/g, "    ")
    .replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f]/g, "");
  return blocks(src.split("\n"));
}
