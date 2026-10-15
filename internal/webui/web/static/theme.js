// Applies the saved theme (Settings: auto/light/dark) before first paint.
// A classic script, loaded synchronously from <head>; prefs.js changes it
// later through the same localStorage key.
(function () {
  var t = "auto";
  try { t = localStorage.getItem("bosun.theme") || "auto"; } catch (e) { /* storage blocked */ }
  if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
})();
