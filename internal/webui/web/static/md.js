// A tiny markdown renderer: fenced code, inline code, **bold**, line breaks.
// All text is HTML-escaped first, so the result is safe to assign to innerHTML.

export function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

function inline(text) {
  return text
    .split(/(`[^`\n]+`)/)
    .map((part, i) => {
      if (i % 2 === 1) return "<code>" + esc(part.slice(1, -1)) + "</code>";
      return esc(part).replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>").replace(/\n/g, "<br>");
    })
    .join("");
}

// render returns an HTML string for model/user text. An unclosed fence (text
// still streaming) is rendered as an open code block.
export function render(text) {
  const fence = /```[^\n]*\n([\s\S]*?)(?:```|$)/g;
  let out = "";
  let last = 0;
  for (const m of text.matchAll(fence)) {
    out += inline(text.slice(last, m.index));
    out += "<pre><code>" + esc(m[1].replace(/\n$/, "")) + "</code></pre>";
    last = m.index + m[0].length;
    if (text[last] === "\n") last++; // the newline after a closing fence is not a line break
  }
  return out + inline(text.slice(last));
}
