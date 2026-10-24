package controller

import (
	"net/http"
)

// handleUI serves the embedded single page application.
//
// The UI is a hand written single page application. There is no
// framework, no build step, no bundler and no external asset. The
// whole interface is a few kilobytes of HTML, CSS and ES6, which
// keeps the controller binary small and the first paint instant.
func (e *Engine) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardHTML))
}

// dashboardHTML is the embedded web application.
//
// The design goals are, in order:
//
//  1. Fast. No framework, no polling storms, no layout thrash. The
//     graph is rendered with a single SVG pass and updated in place.
//  2. Transparent. Every number on screen comes from the API and is
//     labelled with where it came from. Nothing is decorative.
//  3. Legible. High contrast, generous spacing, and a type scale
//     that works at a glance.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en" data-theme="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="color-scheme" content="dark light">
<meta name="theme-color" content="#0b0e14">
<title>Barq CI</title>
<style>
/* ------------------------------------------------------------------ *
 * Design tokens
 * ------------------------------------------------------------------ */
:root {
  --bg: #0b0e14;
  --bg-soft: #11151f;
  --panel: #151a26;
  --panel-2: #1b2130;
  --border: #232a3a;
  --border-soft: #1c2230;
  --text: #e6edf6;
  --text-dim: #9aa7bd;
  --text-faint: #64708a;
  --accent: #6ea8fe;
  --accent-soft: rgba(110,168,254,.14);
  --success: #4ec9a4;
  --success-soft: rgba(78,201,164,.14);
  --warn: #e3b341;
  --warn-soft: rgba(227,179,65,.14);
  --danger: #f2777a;
  --danger-soft: rgba(242,119,122,.14);
  --violet: #b48ead;
  --violet-soft: rgba(180,142,173,.14);
  --pink: #db61a2;
  --radius: 10px;
  --radius-sm: 6px;
  --shadow: 0 1px 2px rgba(0,0,0,.4), 0 8px 24px -12px rgba(0,0,0,.6);
  --mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace;
  --sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Inter, Roboto, sans-serif;
}
html[data-theme="light"] {
  --bg: #f6f8fb;
  --bg-soft: #ffffff;
  --panel: #ffffff;
  --panel-2: #f2f5f9;
  --border: #d9e0ea;
  --border-soft: #e6ebf2;
  --text: #10151f;
  --text-dim: #55617a;
  --text-faint: #8a94a8;
  --accent: #0b62d6;
  --accent-soft: rgba(11,98,214,.1);
  --success: #12805c;
  --success-soft: rgba(18,128,92,.1);
  --warn: #9a6700;
  --warn-soft: rgba(154,103,0,.12);
  --danger: #c0362c;
  --danger-soft: rgba(192,54,44,.1);
  --violet: #8250df;
  --violet-soft: rgba(130,80,223,.1);
  --pink: #bf3989;
  --shadow: 0 1px 2px rgba(16,21,31,.06), 0 8px 24px -14px rgba(16,21,31,.18);
}

* { box-sizing: border-box; }
html, body { height: 100%; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font-family: var(--sans);
  font-size: 14px;
  line-height: 1.5;
  -webkit-font-smoothing: antialiased;
  text-rendering: optimizeLegibility;
}
a { color: var(--accent); text-decoration: none; }
a:hover { text-decoration: underline; }
button, input, select, textarea { font: inherit; color: inherit; }
::-webkit-scrollbar { width: 10px; height: 10px; }
::-webkit-scrollbar-track { background: transparent; }
::-webkit-scrollbar-thumb { background: var(--border); border-radius: 6px; border: 2px solid var(--bg); }
::-webkit-scrollbar-thumb:hover { background: var(--text-faint); }

/* ------------------------------------------------------------------ *
 * Layout
 * ------------------------------------------------------------------ */
.app { display: flex; flex-direction: column; min-height: 100vh; }

header.top {
  display: flex; align-items: center; gap: 14px;
  padding: 0 18px; height: 56px;
  background: var(--bg-soft);
  border-bottom: 1px solid var(--border);
  position: sticky; top: 0; z-index: 40;
}
.brand { display: flex; align-items: center; gap: 9px; font-weight: 650; letter-spacing: .02em; }
.brand svg { display: block; }
.brand .v { color: var(--text-faint); font-size: 11px; font-weight: 500; font-family: var(--mono); }

nav.tabs { display: flex; gap: 2px; margin-left: 10px; }
nav.tabs button {
  background: none; border: 0; padding: 7px 12px; border-radius: var(--radius-sm);
  color: var(--text-dim); cursor: pointer; font-size: 13px; font-weight: 500;
  transition: background .12s, color .12s;
}
nav.tabs button:hover { background: var(--panel); color: var(--text); }
nav.tabs button[aria-selected="true"] { background: var(--accent-soft); color: var(--accent); }
nav.tabs button .count {
  display: inline-block; margin-left: 6px; padding: 0 5px; border-radius: 20px;
  background: var(--panel-2); color: var(--text-faint); font-size: 10px;
  font-family: var(--mono); vertical-align: 1px;
}
.spacer { flex: 1; }

.chip {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 4px 10px; border-radius: 20px;
  border: 1px solid var(--border); background: var(--panel);
  font-size: 11.5px; font-weight: 500; color: var(--text-dim);
  white-space: nowrap;
}
.chip .dot { width: 6px; height: 6px; border-radius: 50%; background: var(--text-faint); }
.chip.ok { color: var(--success); border-color: color-mix(in srgb, var(--success) 40%, var(--border)); }
.chip.ok .dot { background: var(--success); }
.chip.warn { color: var(--warn); border-color: color-mix(in srgb, var(--warn) 40%, var(--border)); }
.chip.warn .dot { background: var(--warn); }
.chip.bad { color: var(--danger); border-color: color-mix(in srgb, var(--danger) 40%, var(--border)); }
.chip.bad .dot { background: var(--danger); }

.icon-btn {
  display: inline-flex; align-items: center; justify-content: center;
  width: 32px; height: 32px; border-radius: var(--radius-sm);
  border: 1px solid var(--border); background: var(--panel);
  cursor: pointer; color: var(--text-dim);
  transition: background .12s, color .12s, border-color .12s;
}
.icon-btn:hover { color: var(--text); border-color: var(--text-faint); }

main { flex: 1; display: grid; grid-template-columns: 300px 1fr; min-height: 0; }
@media (max-width: 980px) { main { grid-template-columns: 1fr; } aside.sidebar { display: none; } }

aside.sidebar {
  border-right: 1px solid var(--border);
  background: var(--bg-soft);
  display: flex; flex-direction: column; min-height: 0;
}
aside.sidebar .sec { padding: 14px 14px 8px; }
aside.sidebar .sec h3 {
  margin: 0 0 8px; font-size: 10.5px; font-weight: 650;
  letter-spacing: .09em; text-transform: uppercase; color: var(--text-faint);
  display: flex; align-items: center; justify-content: space-between;
}
aside.sidebar .scroll { overflow-y: auto; flex: 1; padding: 0 8px 12px; }

section.content { min-width: 0; display: flex; flex-direction: column; }
.content-head {
  display: flex; align-items: center; gap: 10px; flex-wrap: wrap;
  padding: 14px 18px; border-bottom: 1px solid var(--border);
  background: var(--bg-soft);
}
.content-body { padding: 18px; overflow-y: auto; flex: 1; }

/* ------------------------------------------------------------------ *
 * Components
 * ------------------------------------------------------------------ */
.card {
  background: var(--panel); border: 1px solid var(--border);
  border-radius: var(--radius); box-shadow: var(--shadow);
}
.card > h3 {
  margin: 0; padding: 12px 14px; font-size: 12px; font-weight: 650;
  letter-spacing: .04em; text-transform: uppercase; color: var(--text-dim);
  border-bottom: 1px solid var(--border-soft);
  display: flex; align-items: center; justify-content: space-between; gap: 8px;
}
.card > .body { padding: 14px; }

.grid { display: grid; gap: 14px; }
.grid.c2 { grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); }
.grid.c3 { grid-template-columns: repeat(auto-fit, minmax(230px, 1fr)); }
.grid.c4 { grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); }

.stat { padding: 14px; }
.stat .label { font-size: 11px; color: var(--text-faint); text-transform: uppercase; letter-spacing: .06em; font-weight: 600; }
.stat .value { font-size: 26px; font-weight: 640; margin-top: 4px; font-variant-numeric: tabular-nums; letter-spacing: -.02em; }
.stat .sub { font-size: 11.5px; color: var(--text-dim); margin-top: 2px; }
.stat .value.good { color: var(--success); }
.stat .value.bad { color: var(--danger); }
.stat .value.warn { color: var(--warn); }
.stat .value.accent { color: var(--accent); }

button.btn {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 7px 13px; border-radius: var(--radius-sm);
  border: 1px solid var(--border); background: var(--panel-2); color: var(--text);
  cursor: pointer; font-size: 13px; font-weight: 500;
  transition: background .12s, border-color .12s, transform .06s;
}
button.btn:hover { background: var(--border-soft); border-color: var(--text-faint); }
button.btn:active { transform: translateY(.5px); }
button.btn.primary { background: var(--accent); border-color: var(--accent); color: #06121f; font-weight: 600; }
button.btn.primary:hover { filter: brightness(1.08); }
button.btn.danger { color: var(--danger); border-color: color-mix(in srgb, var(--danger) 40%, var(--border)); }
button.btn.danger:hover { background: var(--danger-soft); }
button.btn.ghost { background: none; }
button.btn.sm { padding: 4px 9px; font-size: 12px; }
button.btn:disabled { opacity: .45; cursor: not-allowed; }

input.input, textarea.input, select.input {
  width: 100%; padding: 8px 11px;
  border-radius: var(--radius-sm); border: 1px solid var(--border);
  background: var(--bg); color: var(--text);
  font-size: 13px; outline: none;
  transition: border-color .12s, box-shadow .12s;
}
input.input:focus, textarea.input:focus, select.input:focus {
  border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft);
}
textarea.input { resize: vertical; min-height: 120px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }
label.field { display: block; margin-bottom: 12px; }
label.field > span { display: block; font-size: 12px; color: var(--text-dim); margin-bottom: 5px; font-weight: 500; }
label.field .hint { font-size: 11.5px; color: var(--text-faint); margin-top: 4px; }

.run-item {
  padding: 9px 11px; border-radius: var(--radius-sm); cursor: pointer;
  border: 1px solid transparent; margin-bottom: 2px;
  transition: background .1s, border-color .1s;
}
.run-item:hover { background: var(--panel); }
.run-item.sel { background: var(--accent-soft); border-color: color-mix(in srgb, var(--accent) 35%, transparent); }
.run-item .r1 { display: flex; align-items: center; gap: 7px; }
.run-item .nm { font-size: 13px; font-weight: 550; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.run-item .r2 { font-size: 11px; color: var(--text-faint); margin-top: 3px; display: flex; gap: 8px; font-family: var(--mono); }

.pill {
  display: inline-flex; align-items: center; gap: 5px;
  padding: 2px 7px; border-radius: 5px; font-size: 10.5px; font-weight: 650;
  letter-spacing: .03em; text-transform: uppercase;
  border: 1px solid transparent; white-space: nowrap;
}
.pill.SUCCESS { color: var(--success); background: var(--success-soft); }
.pill.RUNNING { color: var(--accent); background: var(--accent-soft); }
.pill.QUEUED { color: var(--text-dim); background: var(--panel-2); }
.pill.FAILED { color: var(--danger); background: var(--danger-soft); }
.pill.CANCELED { color: var(--text-faint); background: var(--panel-2); }
.pill.CACHED { color: var(--violet); background: var(--violet-soft); }
.pill.SPOT_MIGRATING { color: var(--pink); background: color-mix(in srgb, var(--pink) 14%, transparent); }
.pill.SUCCESS, .pill.FAILED, .pill.CACHED, .pill.SPOT_MIGRATING { animation: none; }
.pill.RUNNING::before { content: ""; width: 5px; height: 5px; border-radius: 50%; background: currentColor; animation: pulse 1.4s ease-in-out infinite; }
@keyframes pulse { 0%,100% { opacity: 1; } 50% { opacity: .3; } }

table.tbl { width: 100%; border-collapse: collapse; font-size: 12.5px; }
table.tbl th {
  text-align: left; padding: 8px 10px; font-size: 10.5px; font-weight: 650;
  text-transform: uppercase; letter-spacing: .06em; color: var(--text-faint);
  border-bottom: 1px solid var(--border); position: sticky; top: 0; background: var(--panel);
}
table.tbl td { padding: 7px 10px; border-bottom: 1px solid var(--border-soft); vertical-align: middle; }
table.tbl tr:last-child td { border-bottom: 0; }
table.tbl tr.clickable { cursor: pointer; }
table.tbl tr.clickable:hover td { background: var(--panel-2); }
table.tbl td.mono { font-family: var(--mono); font-size: 11.5px; color: var(--text-dim); }

/* ------------------------------------------------------------------ *
 * Graph
 * ------------------------------------------------------------------ */
.graph-wrap { position: relative; overflow: auto; background:
  radial-gradient(circle at 1px 1px, var(--border-soft) 1px, transparent 0) 0 0 / 22px 22px; }
#graph { display: block; }
#graph .wave-band { fill: var(--panel); opacity: .5; rx: 10; }
#graph .wave-label { fill: var(--text-faint); font-size: 10.5px; font-weight: 650; letter-spacing: .08em; text-transform: uppercase; }
#graph .edge { fill: none; stroke: var(--border); stroke-width: 1.6; }
#graph .edge.hot { stroke: var(--accent); }
#graph .node { cursor: pointer; }
#graph .node rect { stroke-width: 1.5; rx: 8; transition: filter .12s; }
#graph .node:hover rect { filter: brightness(1.25); }
#graph .node.sel rect { stroke-width: 2.5; }
#graph .node text { font-size: 11.5px; fill: var(--text); font-weight: 550; }
#graph .node .sub { font-size: 10px; fill: var(--text-faint); font-weight: 500; }
#graph .node .badge { font-size: 9px; font-weight: 700; letter-spacing: .05em; }

.legend { display: flex; gap: 14px; flex-wrap: wrap; font-size: 11.5px; color: var(--text-dim); }
.legend span { display: inline-flex; align-items: center; gap: 5px; }
.legend i { width: 9px; height: 9px; border-radius: 3px; display: inline-block; }

/* ------------------------------------------------------------------ *
 * Logs
 * ------------------------------------------------------------------ */
.logs {
  background: #05070c; color: #c9d5e6; border-radius: var(--radius);
  padding: 12px 14px; max-height: 460px; overflow: auto;
  font-family: var(--mono); font-size: 12px; line-height: 1.65;
  white-space: pre-wrap; word-break: break-word; margin: 0;
  border: 1px solid var(--border);
}
html[data-theme="light"] .logs { background: #0d1117; color: #c9d5e6; }
.logs .t { color: #4a5670; user-select: none; }
.logs .err { color: #f2777a; }
.logs .ok { color: #4ec9a4; }
.logs .warn { color: #e3b341; }

/* ------------------------------------------------------------------ *
 * Auth
 * ------------------------------------------------------------------ */
.auth-shell { min-height: 100vh; display: grid; place-items: center; padding: 24px;
  background: radial-gradient(1200px 600px at 50% -10%, color-mix(in srgb, var(--accent) 9%, transparent), transparent), var(--bg); }
.auth-card { width: 100%; max-width: 400px; }
.auth-card .brand { justify-content: center; margin-bottom: 20px; }
.auth-tabs { display: flex; gap: 4px; padding: 4px; background: var(--panel); border-radius: var(--radius-sm); margin-bottom: 18px; }
.auth-tabs button { flex: 1; padding: 7px; border: 0; border-radius: 5px; background: none; color: var(--text-dim); cursor: pointer; font-size: 13px; font-weight: 550; }
.auth-tabs button[aria-selected="true"] { background: var(--panel-2); color: var(--text); }
.auth-msg { padding: 9px 11px; border-radius: var(--radius-sm); font-size: 12.5px; margin-bottom: 12px; }
.auth-msg.err { background: var(--danger-soft); color: var(--danger); }
.auth-msg.ok { background: var(--success-soft); color: var(--success); }
.divider { display: flex; align-items: center; gap: 10px; color: var(--text-faint); font-size: 11.5px; margin: 16px 0; }
.divider::before, .divider::after { content: ""; flex: 1; height: 1px; background: var(--border); }

/* ------------------------------------------------------------------ *
 * Modal
 * ------------------------------------------------------------------ */
.modal-bg { position: fixed; inset: 0; background: rgba(3,6,12,.72); backdrop-filter: blur(3px);
  display: grid; place-items: center; z-index: 100; padding: 20px; }
.modal { width: 100%; max-width: 620px; max-height: 86vh; overflow: auto; background: var(--panel);
  border: 1px solid var(--border); border-radius: var(--radius); box-shadow: var(--shadow); }
.modal header { display: flex; align-items: center; justify-content: space-between;
  padding: 14px 16px; border-bottom: 1px solid var(--border); }
.modal header h2 { margin: 0; font-size: 15px; font-weight: 620; }
.modal .body { padding: 16px; }
.modal footer { padding: 12px 16px; border-top: 1px solid var(--border); display: flex; justify-content: flex-end; gap: 8px; }

.toast-wrap { position: fixed; bottom: 18px; right: 18px; z-index: 200; display: flex; flex-direction: column; gap: 8px; }
.toast { padding: 10px 14px; border-radius: var(--radius-sm); background: var(--panel);
  border: 1px solid var(--border); box-shadow: var(--shadow); font-size: 13px; max-width: 380px;
  animation: slidein .18s ease-out; }
.toast.err { border-color: color-mix(in srgb, var(--danger) 50%, var(--border)); color: var(--danger); }
.toast.ok { border-color: color-mix(in srgb, var(--success) 50%, var(--border)); color: var(--success); }
@keyframes slidein { from { opacity: 0; transform: translateY(8px); } to { opacity: 1; transform: none; } }

.empty { padding: 40px 20px; text-align: center; color: var(--text-faint); font-size: 13px; }
.mono { font-family: var(--mono); }
.dim { color: var(--text-dim); }
.faint { color: var(--text-faint); }
.row { display: flex; align-items: center; gap: 8px; }
.wrap { flex-wrap: wrap; }
.mt { margin-top: 14px; }
.mb { margin-bottom: 14px; }
.hidden { display: none !important; }
.bar { height: 5px; border-radius: 3px; background: var(--panel-2); overflow: hidden; }
.bar > i { display: block; height: 100%; background: var(--accent); border-radius: 3px; transition: width .3s; }
.bar > i.good { background: var(--success); }
.bar > i.bad { background: var(--danger); }
.spark { display: flex; align-items: flex-end; gap: 2px; height: 46px; }
.spark i { flex: 1; background: var(--accent-soft); border-radius: 2px 2px 0 0; min-height: 2px; }
.spark i.good { background: var(--success-soft); }
.spark i.bad { background: var(--danger-soft); }
</style>
</head>
<body>
<div id="root"></div>
<div class="toast-wrap" id="toasts"></div>
<script>
"use strict";
/* ==================================================================== *
 * Barq CI - embedded single page application
 * ==================================================================== */

var S = {
  user: null,
  authRequired: false,
  signupEnabled: false,
  oidcEnabled: false,
  theme: localStorage.getItem("barq.theme") || "dark",
  org: null,
  project: null,
  repo: null,
  orgs: [],
  projects: [],
  repos: [],
  runs: [],
  run: null,
  selectedStep: null,
  stats: null,
  health: null,
  users: [],
  secrets: [],
  alerts: [],
  plugins: [],
  logStream: null,
  poll: null,
  dirty: false,
  csrftoken: readCookie("barq_csrf"),
  dropdownOpen: false
};

/* ---------------------------------------------------------------- *
 * Utilities
 * ---------------------------------------------------------------- */
function el(tag, attrs, children) {
  var node = document.createElement(tag);
  if (attrs) {
    for (var key in attrs) {
      if (!Object.prototype.hasOwnProperty.call(attrs, key)) { continue; }
      var value = attrs[key];
      if (value === null || value === undefined || value === false) { continue; }
      if (key === "class") { node.className = value; }
      else if (key === "text") { node.textContent = value; }
      else if (key === "html") { node.innerHTML = value; }
      else if (key.slice(0, 2) === "on") { node.addEventListener(key.slice(2), value); }
      else if (value === true) { node.setAttribute(key, ""); }
      else { node.setAttribute(key, value); }
    }
  }
  (children || []).forEach(function (child) {
    if (child === null || child === undefined || child === false) { return; }
    node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
  });
  return node;
}
function svgEl(tag, attrs) {
  var node = document.createElementNS("http://www.w3.org/2000/svg", tag);
  if (attrs) {
    for (var key in attrs) {
      if (!Object.prototype.hasOwnProperty.call(attrs, key)) { continue; }
      if (attrs[key] === null || attrs[key] === undefined) { continue; }
      node.setAttribute(key, attrs[key]);
    }
  }
  return node;
}
function readCookie(name) {
  var parts = document.cookie.split(";");
  for (var i = 0; i < parts.length; i++) {
    var pair = parts[i].split("=");
    if (pair[0].trim() === name) { return decodeURIComponent(pair.slice(1).join("=")); }
  }
  return "";
}
function clearCookie(name) {
  document.cookie = name + "=; Path=/; Max-Age=0; SameSite=Lax";
}
function fmtMs(ms) {
  if (ms === null || ms === undefined) { return "-"; }
  if (ms < 1000) { return ms + "ms"; }
  if (ms < 60000) { return (ms / 1000).toFixed(1) + "s"; }
  if (ms < 3600000) { return Math.floor(ms / 60000) + "m " + Math.floor((ms % 60000) / 1000) + "s"; }
  return Math.floor(ms / 3600000) + "h " + Math.floor((ms % 3600000) / 60000) + "m";
}
function fmtNum(value) {
  if (value === null || value === undefined) { return "-"; }
  if (Math.abs(value) >= 1000000) { return (value / 1000000).toFixed(1) + "M"; }
  if (Math.abs(value) >= 1000) { return (value / 1000).toFixed(1) + "k"; }
  return String(value);
}
function fmtPct(value) {
  if (value === null || value === undefined) { return "-"; }
  return value.toFixed(1) + "%";
}
function fmtWhen(iso) {
  if (!iso) { return "-"; }
  var d = new Date(iso);
  var diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) { return "just now"; }
  if (diff < 3600) { return Math.floor(diff / 60) + "m ago"; }
  if (diff < 86400) { return Math.floor(diff / 3600) + "h ago"; }
  if (diff < 604800) { return Math.floor(diff / 86400) + "d ago"; }
  return d.toISOString().slice(0, 10);
}
function esc(text) {
  return String(text === null || text === undefined ? "" : text)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}
function toast(message, kind) {
  var wrap = document.getElementById("toasts");
  var node = el("div", { class: "toast " + (kind || ""), text: message });
  wrap.appendChild(node);
  setTimeout(function () { node.remove(); }, kind === "err" ? 6000 : 3000);
}
function api(path, options) {
  options = options || {};
  options.headers = options.headers || {};
  if (options.body && !options.headers["Content-Type"]) {
    options.headers["Content-Type"] = "application/json";
  }
  if (S.csrftoken) { options.headers["X-Barq-CSRF"] = S.csrftoken; }
  options.credentials = "same-origin";
  return fetch(path, options).then(function (response) {
    if (response.status === 401) {
      S.user = null;
      render();
      throw new Error("Your session has expired. Please sign in again.");
    }
    var type = response.headers.get("content-type") || "";
    if (type.indexOf("application/json") >= 0) {
      return response.json().then(function (data) {
        if (!response.ok) { throw new Error(data.error || "Request failed"); }
        return data;
      });
    }
    if (!response.ok) {
      return response.text().then(function (text) { throw new Error(text || "Request failed"); });
    }
    return response.text();
  });
}

/* ---------------------------------------------------------------- *
 * Horse logo SVG
 * ---------------------------------------------------------------- */
function logo(size) {
  var svg = svgEl("svg", { width: size || 24, height: size || 24, viewBox: "0 0 24 24", fill: "none", xmlns: "http://www.w3.org/2000/svg" });
  var path = svgEl("path", {
    d: "M12 2L3 7l6 6L4 17l5 5 6-6L21 7l-9-5z",
    stroke: "currentColor",
    "stroke-width": "2",
    "stroke-linecap": "round",
    "stroke-linejoin": "round",
    fill: "none"
  });
  svg.appendChild(path);
  return svg;
}

/* ---------------------------------------------------------------- *
 * Shell
 * ---------------------------------------------------------------- */
function render() {
  var root = document.getElementById("root");
  root.innerHTML = "";
  if (!S.user && S.authRequired) {
    root.appendChild(renderAuth());
  } else {
    root.appendChild(renderShell());
  }
}

function renderShell() {
  var app = el("div", { class: "app" });
  app.appendChild(renderHeader());
  var main = el("main", {});
  main.appendChild(renderSidebar());
  var section = el("section", { class: "content" });
  section.appendChild(renderContentHead());
  section.appendChild(renderContentBody());
  main.appendChild(section);
  app.appendChild(main);
  return app;
}

function renderHeader() {
  var header = el("header", { class: "top" });

  // Brand + Org selector
  var brand = el("div", { class: "brand" });
  brand.appendChild(logo(22));
  brand.appendChild(el("span", { style: "font-size:18px;font-weight:700;letter-spacing:-.02em", text: "Barq" }));
  brand.appendChild(el("span", { class: "v", text: "CI" }));
  header.appendChild(brand);

  // Org / Project / Repo breadcrumb
  var breadcrumb = el("div", { class: "breadcrumb" });
  if (S.org) {
    breadcrumb.appendChild(el("a", { text: S.org.name, href: "#", onclick: function(e){ e.preventDefault(); S.project=null; S.repo=null; S.run=null; render(); } }));
    breadcrumb.appendChild(el("span", { class: "sep", text: "/" }));
  }
  if (S.project) {
    breadcrumb.appendChild(el("a", { text: S.project.name, href: "#", onclick: function(e){ e.preventDefault(); S.repo=null; S.run=null; render(); } }));
    breadcrumb.appendChild(el("span", { class: "sep", text: "/" }));
  }
  if (S.repo) {
    breadcrumb.appendChild(el("span", { class: "current", text: S.repo.name }));
  }
  if (S.org || S.project || S.repo) {
    header.appendChild(breadcrumb);
  }

  // Main nav tabs
  var tabs = el("nav", { class: "tabs" });
  var defs = [
    ["runs", "Runs", S.runs.length],
    ["graph", "Graph", 0],
    ["settings", "Settings", 0]
  ];
  defs.forEach(function (def) {
    var button = el("button", {
      type: "button", "aria-selected": S.tab === def[0] ? "true" : "false",
      onclick: function () { S.tab = def[0]; render(); }
    }, [document.createTextNode(def[1])]);
    if (def[2]) { button.appendChild(el("span", { class: "count", text: String(def[2]) })); }
    tabs.appendChild(button);
  });
  header.appendChild(tabs);

  header.appendChild(el("div", { class: "spacer" }));

  // Health
  var health = el("span", { class: "chip" });
  if (S.health) {
    var state = S.health.state || "unknown";
    health.className = "chip " + (state === "healthy" ? "ok" : state === "degraded" ? "warn" : "bad");
    health.appendChild(el("i", { class: "dot" }));
    health.appendChild(document.createTextNode(
      state === "healthy" ? "Healthy" : state === "degraded" ? "Degraded" : "Unhealthy"));
  } else {
    health.appendChild(document.createTextNode("connecting"));
  }
  header.appendChild(health);

  // User profile dropdown
  if (S.user) {
    var userWrap = el("div", { style: "position:relative; display:flex; align-items:center; gap:10px;" });
    userWrap.appendChild(el("span", { class: "chip", text: S.user.email + " · " + S.user.role }));
    var dropdownBtn = el("button", {
      class: "icon-btn",
      type: "button",
      onclick: function() { S.dropdownOpen = !S.dropdownOpen; render(); }
    });
    dropdownBtn.appendChild(el("i", { style: "font-size:16px; font-weight:bold", text: "▼" }));
    userWrap.appendChild(dropdownBtn);
    header.appendChild(userWrap);

    if (S.dropdownOpen) {
      var dd = el("div", { class: "dropdown" });
      dd.appendChild(el("a", { href: "#", text: "Profile", onclick: function(){ S.dropdownOpen=false; S.tab="settings"; render(); } }));
      dd.appendChild(el("div", { class: "div" }));
      dd.appendChild(el("a", { href: "#", text: "Settings", onclick: function(){ S.dropdownOpen=false; S.tab="settings"; render(); } }));
      dd.appendChild(el("div", { class: "div" }));
      dd.appendChild(el("button", { class: "btn sm ghost", type: "button", text: "Sign out", style: "width:100%; justify-content:flex-start; color: var(--danger);", onclick: function() {
        S.dropdownOpen=false;
        api("/api/v1/auth/logout", { method: "POST" }).then(function () {
          S.user = null; clearCookie("barq_csrf"); render();
        });
      } }));
      header.appendChild(dd);
    }
  }

  // Theme toggle
  var themeBtn = el("button", {
    class: "icon-btn", type: "button", title: "Toggle theme",
    onclick: function () {
      S.theme = S.theme === "dark" ? "light" : "dark";
      localStorage.setItem("barq.theme", S.theme);
      document.documentElement.setAttribute("data-theme", S.theme);
    }
  });
  themeBtn.appendChild(logo(16));
  header.appendChild(themeBtn);

  return header;
}

function renderSidebar() {
  var aside = el("aside", { class: "sidebar" });

  // Org selector
  var sec = el("div", { class: "sec" });
  sec.appendChild(el("h3", { text: "Organization" }));
  var orgSelect = el("select", { class: "input", style: "width:100%;padding:6px 8px;font-size:12px" });
  orgSelect.appendChild(el("option", { value: "", text: "Select organization" }));
  S.orgs.forEach(function (o) {
    orgSelect.appendChild(el("option", { value: o.id, text: o.name, selected: S.org && S.org.id === o.id }));
  });
  orgSelect.addEventListener("change", function () {
    var id = orgSelect.value;
    if (!id) { S.org = null; S.project = null; S.repo = null; S.run = null; render(); return; }
    var found = S.orgs.find(function (o) { return o.id === id; });
    if (found) { S.org = found; S.project = null; S.repo = null; S.run = null; loadProjects(); render(); }
  });
  sec.appendChild(orgSelect);
  aside.appendChild(sec);

  // Project selector
  if (S.org) {
    var pSec = el("div", { class: "sec" });
    pSec.appendChild(el("h3", { text: "Project" }));
    var projSelect = el("select", { class: "input", style: "width:100%;padding:6px 8px;font-size:12px" });
    projSelect.appendChild(el("option", { value: "", text: "Select project" }));
    S.projects.forEach(function (p) {
      projSelect.appendChild(el("option", { value: p.id, text: p.name, selected: S.project && S.project.id === p.id }));
    });
    projSelect.addEventListener("change", function () {
      var id = projSelect.value;
      if (!id) { S.project = null; S.repo = null; S.run = null; render(); return; }
      var found = S.projects.find(function (p) { return p.id === id; });
      if (found) { S.project = found; S.repo = null; S.run = null; loadRepos(); render(); }
    });
    pSec.appendChild(projSelect);
    aside.appendChild(pSec);
  }

  // Repo selector
  if (S.project) {
    var rSec = el("div", { class: "sec" });
    rSec.appendChild(el("h3", { text: "Repository" }));
    var repoSelect = el("select", { class: "input", style: "width:100%;padding:6px 8px;font-size:12px" });
    repoSelect.appendChild(el("option", { value: "", text: "Select repository" }));
    S.repos.forEach(function (r) {
      repoSelect.appendChild(el("option", { value: r.id, text: r.name, selected: S.repo && S.repo.id === r.id }));
    });
    repoSelect.addEventListener("change", function () {
      var id = repoSelect.value;
      if (!id) { S.repo = null; S.run = null; render(); return; }
      var found = S.repos.find(function (r) { return r.id === id; });
      if (found) { S.repo = found; S.run = null; loadRuns(); render(); }
    });
    rSec.appendChild(repoSelect);
    aside.appendChild(rSec);
  }

  // Recent runs for selected repo
  if (S.repo) {
    var runSec = el("div", { class: "sec" });
    runSec.appendChild(el("h3", { text: "Recent runs" }));
    var scroll = el("div", { class: "scroll" });
    if (!S.runs.length) {
      scroll.appendChild(el("div", { class: "empty", text: "No runs yet" }));
    }
    S.runs.slice(0, 30).forEach(function (run) {
      var item = el("div", {
        class: "run-item" + (run.id === (S.run && S.run.id) ? " sel" : ""),
        onclick: function () { selectRun(run.id); }
      });
      var pill = el("span", { class: "pill " + run.status, text: run.status });
      var info = el("div", { class: "info" });
      info.appendChild(el("span", { class: "nm", text: run.name }));
      info.appendChild(el("span", { class: "r2", text: run.id.slice(0, 8) + "  ·  " + fmtWhen(run.created_at) }));
      item.appendChild(pill);
      item.appendChild(info);
      scroll.appendChild(item);
    });
    runSec.appendChild(scroll);
    aside.appendChild(runSec);
  }

  return aside;
}

function renderContentHead() {
  var head = el("div", { class: "content-head" });
  if (S.tab === "runs" || S.tab === "graph") {
    head.appendChild(el("h2", { style: "font-size:15px;font-weight:700;margin:0", text: S.run ? S.run.name : "Runs" }));
    if (S.run) {
      head.appendChild(el("span", { class: "pill " + S.run.status, text: S.run.status }));
      head.appendChild(el("span", { class: "faint mono", text: S.run.id }));
    }
    head.appendChild(el("div", { class: "spacer" }));
    head.appendChild(el("button", {
      class: "btn primary", type: "button", text: "Spawn pipeline",
      onclick: openSpawn
    }));
  } else {
    head.appendChild(el("h2", { style: "font-size:15px;font-weight:700;margin:0", text: "Settings" }));
  }
  return head;
}

function renderContentBody() {
  var body = el("div", { class: "content-body" });
  if (S.tab === "runs") { body.appendChild(renderRunsTab()); }
  else if (S.tab === "graph") { body.appendChild(renderGraphTab()); }
  else { body.appendChild(renderSettingsTab()); }
  return body;
}

/* ---------------------------------------------------------------- *
 * Runs tab
 * ---------------------------------------------------------------- */
function renderRunsTab() {
  var wrap = el("div", { class: "grid" });

  if (!S.run) {
    var card = el("div", { class: "card" });
    card.appendChild(el("h3", { text: "Runs" }));
    var body = el("div", { class: "body" });
    if (!S.runs.length) {
      body.appendChild(el("div", { class: "empty", text: "No runs yet. Spawn a pipeline to get started." }));
    } else {
      body.appendChild(renderRunTable(S.runs));
    }
    card.appendChild(body);
    wrap.appendChild(card);
    return wrap;
  }

  var run = S.run;
  var actions = el("div", { class: "card mb" });
  var aBody = el("div", { class: "body row wrap" });
  aBody.appendChild(el("button", { class: "btn", type: "button", text: "Full rebuild", onclick: function () { rebuild("full"); } }));
  aBody.appendChild(el("button", { class: "btn", type: "button", text: "Rerun failed wave", onclick: function () { rebuild("failed"); } }));
  aBody.appendChild(el("button", { class: "btn", type: "button", text: "Rerun this wave", onclick: function () { rebuildWave(); } }));
  aBody.appendChild(el("button", { class: "btn", type: "button", text: "Rerun selected step", onclick: function () { rebuildStep(); } }));
  aBody.appendChild(el("div", { class: "spacer" }));
  if (run.status !== "SUCCESS" && run.status !== "FAILED" && run.status !== "CANCELED") {
    aBody.appendChild(el("button", { class: "btn danger", type: "button", text: "Stop run", onclick: stopRun }));
  }
  actions.appendChild(aBody);
  wrap.appendChild(actions);

  var columns = el("div", { class: "grid c2" });

  var steps = el("div", { class: "card" });
  steps.appendChild(el("h3", {}, [document.createTextNode("Steps"), el("span", { class: "faint", text: Object.keys(run.steps || {}).length + " steps" })]));
  var sBody = el("div", { class: "body" });
  sBody.appendChild(renderStepTable(run));
  steps.appendChild(sBody);
  columns.appendChild(steps);

  var timing = el("div", { class: "card" });
  timing.appendChild(el("h3", { text: "Timing" }));
  var tBody = el("div", { class: "body" });
  var total = 0;
  var slowest = null;
  Object.keys(run.steps || {}).forEach(function (id) {
    var step = run.steps[id];
    total += step.duration_ms || 0;
    if (!slowest || (step.duration_ms || 0) > (slowest.duration_ms || 0)) { slowest = step; }
  });
  var cached = Object.keys(run.steps || {}).filter(function (id) { return run.steps[id].cache_hit; }).length;
  tBody.appendChild(renderStatRow("Pipeline duration", fmtMs(runDuration(run))));
  tBody.appendChild(renderStatRow("Sum of step time", fmtMs(total)));
  tBody.appendChild(renderStatRow("Slowest step", slowest ? slowest.id + " · " + fmtMs(slowest.duration_ms) : "-"));
  tBody.appendChild(renderStatRow("Cached steps", cached + " of " + Object.keys(run.steps || {}).length));
  tBody.appendChild(renderStatRow("Started", run.started_at ? new Date(run.started_at).toLocaleString() : "-"));
  tBody.appendChild(renderStatRow("Finished", run.finished_at ? new Date(run.finished_at).toLocaleString() : "-"));
  timing.appendChild(tBody);
  columns.appendChild(timing);

  wrap.appendChild(columns);

  var logs = el("div", { class: "card mt" });
  logs.appendChild(el("h3", {}, [document.createTextNode("Logs"),
    el("span", { class: "faint", text: "secret values are redacted before they reach this stream" })]));
  var lBody = el("div", { class: "body" });
  lBody.appendChild(el("pre", { class: "logs", id: "logbox", text: "Select a run to stream its logs." }));
  logs.appendChild(lBody);
  wrap.appendChild(logs);
  return wrap;
}

function runDuration(run) {
  if (!run || !run.started_at) { return 0; }
  var end = run.finished_at ? new Date(run.finished_at).getTime() : Date.now();
  return end - new Date(run.started_at).getTime();
}

function renderStatRow(label, value) {
  var row = el("div", { class: "row", style: "justify-content:space-between;padding:6px 0;border-bottom:1px solid var(--border-soft)" });
  row.appendChild(el("span", { class: "dim", style: "font-size:12.5px", text: label }));
  row.appendChild(el("span", { class: "mono", style: "font-size:12.5px", text: value }));
  return row;
}

function renderRunTable(runs) {
  var table = el("table", { class: "tbl" });
  var head = el("thead");
  head.appendChild(el("tr", {}, [
    el("th", { text: "Status" }), el("th", { text: "Pipeline" }),
    el("th", { text: "Duration" }), el("th", { text: "Created" })
  ]));
  table.appendChild(head);
  var body = el("tbody");
  runs.forEach(function (run) {
    var row = el("tr", { class: "clickable", onclick: function () { selectRun(run.id); } });
    row.appendChild(el("td", {}, [el("span", { class: "pill " + run.status, text: run.status })]));
    row.appendChild(el("td", { text: run.name }));
    row.appendChild(el("td", { class: "mono", text: fmtMs(runDuration(run)) }));
    row.appendChild(el("td", { class: "mono", text: fmtWhen(run.created_at) }));
    body.appendChild(row);
  });
  table.appendChild(body);
  return table;
}

function renderStepTable(run) {
  var table = el("table", { class: "tbl" });
  var head = el("thead");
  head.appendChild(el("tr", {}, [
    el("th", { text: "Step" }), el("th", { text: "Wave" }), el("th", { text: "Status" }),
    el("th", { text: "Time" }), el("th", { text: "Try" })
  ]));
  table.appendChild(head);
  var body = el("tbody");
  var ids = Object.keys(run.steps || {}).sort(function (a, b) {
    var sa = run.steps[a], sb = run.steps[b];
    if (sa.wave !== sb.wave) { return sa.wave - sb.wave; }
    return a < b ? -1 : 1;
  });
  ids.forEach(function (id) {
    var step = run.steps[id];
    var row = el("tr", {
      class: "clickable" + (S.selectedStep === id ? " sel" : ""),
      onclick: function () { S.selectedStep = id; render(); }
    });
    row.appendChild(el("td", { text: id }));
    row.appendChild(el("td", { class: "mono", text: String(step.wave) }));
    row.appendChild(el("td", {}, [el("span", { class: "pill " + step.status, text: step.status })]));
    row.appendChild(el("td", { class: "mono", text: step.cache_hit ? "cached" : fmtMs(step.duration_ms) }));
    row.appendChild(el("td", { class: "mono", text: String(step.attempt || 0) + 1 }));
    body.appendChild(row);
  });
  table.appendChild(body);
  return table;
}

/* ---------------------------------------------------------------- *
 * Graph tab — real-time DAG with per-step logs
 * ---------------------------------------------------------------- */
var STATUS_COLOR = {
  SUCCESS: "var(--success)", FAILED: "var(--danger)", RUNNING: "var(--accent)",
  QUEUED: "var(--text-dim)", CACHED: "var(--violet)", CANCELED: "var(--text-faint)",
  SPOT_MIGRATING: "var(--pink)"
};

function renderGraphTab() {
  var wrap = el("div", { class: "grid" });
  if (!S.run) {
    wrap.appendChild(el("div", { class: "card" }, [el("div", { class: "empty", text: "Select a run to see its execution graph." })]));
    return wrap;
  }
  var run = S.run;
  var card = el("div", { class: "card" });
  card.appendChild(el("h3", {}, [
    document.createTextNode("Execution graph"),
    el("span", { class: "faint", text: "waves run concurrently · click a node for its logs" })
  ]));
  var body = el("div", { class: "body" });

  // Split: graph left, logs right
  var split = el("div", { style: "display:flex; flex-direction:column; gap:14px; height:100%" });

  var graphCard = el("div", { class: "card", style: "flex:1; min-height:0" });
  graphCard.appendChild(el("h3", { style: "margin:0 0 10px 0; font-size:12px; font-weight:700; text-transform:uppercase; letter-spacing:.06em; color:var(--text-faint)", text: "DAG" }));
  var gBody = el("div", { class: "body", style: "padding:0; min-height:0" });
  var graph = el("div", { class: "graph-wrap", style: "flex:1; min-height:0" });
  graph.appendChild(buildGraph(run));
  gBody.appendChild(graph);
  graphCard.appendChild(gBody);
  split.appendChild(graphCard);

  var logCard = el("div", { class: "card", style: "flex:1; min-height:0" });
  logCard.appendChild(el("h3", { style: "margin:0 0 10px 0; font-size:12px; font-weight:700; text-transform:uppercase; letter-spacing:.06em; color:var(--text-faint)", text: "Step Logs" }));
  var lBody = el("div", { class: "body", style: "padding:0; min-height:0" });
  lBody.appendChild(el("pre", { class: "logs", id: "graph-logbox", text: S.selectedStep ? "" : "Click a node in the graph to view its logs." }));
  logCard.appendChild(lBody);
  split.appendChild(logCard);

  body.appendChild(split);
  card.appendChild(body);
  wrap.appendChild(card);

  // Stream logs for selected step
  if (S.selectedStep && S.run) {
    openStepLogs(S.run.id, S.selectedStep);
  }

  return wrap;
}

function buildGraph(run) {
  var compiled = run.compiled || { steps: [], waves: [] };
  var byId = {};
  (compiled.steps || []).forEach(function (step) { byId[step.id] = step; });
  var waves = compiled.waves || [];
  if (!waves.length) {
    var empty = el("div", { class: "empty", text: "This pipeline has no steps." });
    return empty;
  }

  var COL = 268, ROW = 96, PAD_X = 26, PAD_Y = 46, NODE_W = 224, NODE_H = 62;
  var positions = {};
  var maxRows = 0;
  waves.forEach(function (wave, waveIndex) {
    wave.forEach(function (id, rowIndex) {
      positions[id] = { x: PAD_X + waveIndex * COL, y: PAD_Y + rowIndex * ROW, wave: waveIndex };
      if (rowIndex + 1 > maxRows) { maxRows = rowIndex + 1; }
    });
  });

  var width = PAD_X * 2 + (waves.length - 1) * COL + NODE_W;
  var height = PAD_Y + maxRows * ROW + 24;
  var svg = svgEl("svg", { id: "graph", width: width, height: height, viewBox: "0 0 " + width + " " + height });

  var defs = svgEl("defs");
  var marker = svgEl("marker", {
    id: "arrow", viewBox: "0 0 10 10", refX: "9", refY: "5",
    markerWidth: "5", markerHeight: "5", orient: "auto-start-reverse"
  });
  marker.appendChild(svgEl("path", { d: "M 0 0 L 10 5 L 0 10 z", fill: "var(--border)" }));
  defs.appendChild(marker);
  svg.appendChild(defs);

  waves.forEach(function (wave, waveIndex) {
    var band = svgEl("rect", {
      class: "wave-band", x: PAD_X + waveIndex * COL - 12, y: PAD_Y - 26,
      width: NODE_W + 24, height: maxRows * ROW + 12
    });
    svg.appendChild(band);
    var label = svgEl("text", {
      class: "wave-label", x: PAD_X + waveIndex * COL, y: PAD_Y - 34
    });
    label.textContent = "Wave " + waveIndex + " · " + wave.length + " step" + (wave.length === 1 ? "" : "s");
    svg.appendChild(label);
  });

  Object.keys(byId).forEach(function (id) {
    var step = byId[id];
    var target = positions[id];
    if (!target) { return; }
    (step.depends_on || []).forEach(function (parentId) {
      var source = positions[parentId];
      if (!source) { return; }
      var x1 = source.x + NODE_W, y1 = source.y + NODE_H / 2;
      var x2 = target.x, y2 = target.y + NODE_H / 2;
      var mid = (x1 + x2) / 2;
      var hot = S.selectedStep === id || S.selectedStep === parentId;
      svg.appendChild(svgEl("path", {
        class: "edge" + (hot ? " hot" : ""),
        d: "M " + x1 + " " + y1 + " C " + mid + " " + y1 + ", " + mid + " " + y2 + ", " + x2 + " " + y2,
        "marker-end": "url(#arrow)"
      }));
    });
  });

  Object.keys(byId).forEach(function (id) {
    var step = byId[id];
    var target = positions[id];
    var state = (run.steps && run.steps[id]) || { status: "QUEUED" };
    var group = svgEl("g", { class: "node" + (S.selectedStep === id ? " sel" : "") });
    group.appendChild(svgEl("rect", {
      x: target.x, y: target.y, width: NODE_W, height: NODE_H,
      fill: "var(--panel)", stroke: STATUS_COLOR[state.status] || "var(--border)"
    }));
    var title = svgEl("text", { x: target.x + 12, y: target.y + 22 });
    title.textContent = id.length > 27 ? id.slice(0, 26) + "..." : id;
    group.appendChild(title);
    var meta = svgEl("text", { x: target.x + 12, y: target.y + 40, class: "sub" });
    var bits = [state.status];
    if (state.cache_hit) { bits.push("cached"); }
    if (step.shard_total > 1) { bits.push("shard " + (step.shard_index + 1) + "/" + step.shard_total); }
    if (state.duration_ms) { bits.push(fmtMs(state.duration_ms)); }
    meta.textContent = bits.join(" · ");
    group.appendChild(meta);
    var badge = svgEl("text", { x: target.x + NODE_W - 12, y: target.y + 22, class: "badge", "text-anchor": "end" });
    badge.setAttribute("fill", STATUS_COLOR[state.status] || "var(--text-faint)");
    badge.textContent = "W" + step.wave;
    group.appendChild(badge);
    group.addEventListener("click", function () {
      S.selectedStep = S.selectedStep === id ? null : id;
      if (S.selectedStep && S.run) { openStepLogs(S.run.id, S.selectedStep); }
      render();
    });
    svg.appendChild(group);
  });

  return svg;
}

function openStepLogs(runId, stepId) {
  var box = document.getElementById("graph-logbox");
  if (!box) { return; }
  box.textContent = "Fetching logs for " + stepId + "...\n";
  api("/api/v1/runs/" + runId + "/logs?step=" + encodeURIComponent(stepId)).then(function (data) {
    box.textContent = data || "No logs yet for " + stepId;
    box.scrollTop = box.scrollHeight;
  }).catch(function (e) {
    box.textContent = "Error loading logs: " + e.message;
  });
}

/* ---------------------------------------------------------------- *
 * Analytics tab (placeholder)
 * ---------------------------------------------------------------- */
function renderStatsTab() {
  var wrap = el("div", { class: "grid" });
  wrap.appendChild(el("div", { class: "card" }, [el("div", { class: "empty", text: "Analytics coming soon." })]));
  return wrap;
}

/* ---------------------------------------------------------------- *
 * Settings tab — RBAC, profile, secrets, credentials
 * ---------------------------------------------------------------- */
function renderSettingsTab() {
  var wrap = el("div", { class: "grid c2" });

  if (S.user) {
    var profile = el("div", { class: "card" });
    profile.appendChild(el("h3", { text: "Profile" }));
    var pBody = el("div", { class: "body" });
    var nameField = el("label", { class: "field" });
    nameField.appendChild(el("span", { text: "Display name" }));
    var nameInput = el("input", { class: "input", value: S.user.display_name || "" });
    nameField.appendChild(nameInput);
    pBody.appendChild(nameField);
    var emailField = el("label", { class: "field" });
    emailField.appendChild(el("span", { text: "Email" }));
    emailField.appendChild(el("input", { class: "input", value: S.user.email, disabled: true }));
    pBody.appendChild(emailField);
    pBody.appendChild(el("div", { class: "row" }, [
      el("span", { class: "chip", text: "Role: " + S.user.role }),
      el("button", {
        class: "btn", type: "button", text: "Save profile",
        onclick: function () {
          api("/api/v1/profile", {
            method: "PATCH",
            body: JSON.stringify({ display_name: nameInput.value })
          }).then(function () { toast("Profile updated", "ok"); loadSession(); })
            .catch(function (e) { toast(e.message, "err"); });
        }
      })
    ]));
    profile.appendChild(pBody);
    wrap.appendChild(profile);

    var password = el("div", { class: "card" });
    password.appendChild(el("h3", { text: "Change password" }));
    var pwBody = el("div", { class: "body" });
    var current = el("input", { class: "input", type: "password", placeholder: "Current password" });
    var next = el("input", { class: "input", type: "password", placeholder: "New password" });
    var confirm = el("input", { class: "input", type: "password", placeholder: "Confirm new password" });
    pwBody.appendChild(el("label", { class: "field" }, [el("span", { text: "Current" }), current]));
    pwBody.appendChild(el("label", { class: "field" }, [el("span", { text: "New" }), next,
      el("div", { class: "hint", text: "At least 12 characters, mixing letters with digits or symbols." })]));
    pwBody.appendChild(el("label", { class: "field" }, [el("span", { text: "Confirm" }), confirm]));
    pwBody.appendChild(el("button", {
      class: "btn primary", type: "button", text: "Change password",
      onclick: function () {
        if (next.value !== confirm.value) { toast("The passwords do not match", "err"); return; }
        api("/api/v1/profile/password", {
          method: "POST",
          body: JSON.stringify({ current_password: current.value, new_password: next.value })
        }).then(function () {
          toast("Password changed. Please sign in again.", "ok");
          S.user = null; clearCookie("barq_session"); clearCookie("barq_csrf"); render();
        }).catch(function (e) { toast(e.message, "err"); });
      }
    }));
    password.appendChild(pwBody);
    wrap.appendChild(password);
  }

  if (S.user && S.user.role === "admin") {
    // Users (RBAC)
    var users = el("div", { class: "card" });
    users.appendChild(el("h3", {}, [document.createTextNode("Users"), el("span", { class: "faint", text: String(S.users.length) })]));
    var uBody = el("div", { class: "body" });
    var table = el("table", { class: "tbl" });
    var head = el("thead");
    head.appendChild(el("tr", {}, [
      el("th", { text: "Email" }), el("th", { text: "Role" }), el("th", { text: "State" }), el("th", { text: "" })
    ]));
    table.appendChild(head);
    var tbody = el("tbody");
    S.users.forEach(function (user) {
      var select = el("select", { class: "input", style: "width:auto;padding:4px 8px" });
      ["viewer", "builder", "admin"].forEach(function (role) {
        var option = el("option", { value: role, text: role });
        if (user.role === role) { option.selected = "selected"; }
        select.appendChild(option);
      });
      select.addEventListener("change", function () {
        api("/api/v1/users/" + user.id, { method: "PATCH", body: JSON.stringify({ role: select.value }) })
          .then(function () { toast("Role updated", "ok"); loadUsers(); })
          .catch(function (e) { toast(e.message, "err"); });
      });
      tbody.appendChild(el("tr", {}, [
        el("td", { text: user.email }),
        el("td", {}, [select]),
        el("td", {}, [el("span", { class: "pill " + (user.disabled ? "FAILED" : "SUCCESS"), text: user.disabled ? "disabled" : "active" })]),
        el("td", {}, [el("button", {
          class: "btn sm", type: "button", text: user.disabled ? "Enable" : "Disable",
          onclick: function () {
            api("/api/v1/users/" + user.id, { method: "PATCH", body: JSON.stringify({ disabled: !user.disabled }) })
              .then(function () { toast("User updated", "ok"); loadUsers(); })
              .catch(function (e) { toast(e.message, "err"); });
          }
        })])
      ]));
    });
    table.appendChild(tbody);
    uBody.appendChild(table);
    users.appendChild(uBody);
    wrap.appendChild(users);

    // Secrets
    var secrets = el("div", { class: "card" });
    secrets.appendChild(el("h3", {}, [document.createTextNode("Secrets"), el("span", { class: "faint", text: "values are never returned" })]));
    var sBody = el("div", { class: "body" });
    var secretName = el("input", { class: "input", placeholder: "NPM_TOKEN" });
    var secretValue = el("input", { class: "input", type: "password", placeholder: "value" });
    sBody.appendChild(el("div", { class: "row mb" }, [secretName, secretValue,
      el("button", {
        class: "btn primary", type: "button", text: "Add",
        onclick: function () {
          api("/api/v1/secrets", {
            method: "POST",
            body: JSON.stringify({ name: secretName.value, value: secretValue.value })
          }).then(function () {
            toast("Secret stored", "ok"); secretValue.value = ""; loadSecrets();
          }).catch(function (e) { toast(e.message, "err"); });
        }
      })]));
    var sTable = el("table", { class: "tbl" });
    var sHead = el("thead");
    sHead.appendChild(el("tr", {}, [el("th", { text: "Name" }), el("th", { text: "Version" }), el("th", { text: "Fingerprint" }), el("th", { text: "" })]));
    sTable.appendChild(sHead);
    var sTbody = el("tbody");
    S.secrets.forEach(function (secret) {
      sTbody.appendChild(el("tr", {}, [
        el("td", { text: secret.name }),
        el("td", { class: "mono", text: "v" + secret.version }),
        el("td", { class: "mono", text: secret.fingerprint }),
        el("td", {}, [el("button", {
          class: "btn sm danger", type: "button", text: "Delete",
          onclick: function () {
            api("/api/v1/secrets/" + encodeURIComponent(secret.name), { method: "DELETE" })
              .then(function () { toast("Secret deleted", "ok"); loadSecrets(); })
              .catch(function (e) { toast(e.message, "err"); });
          }
        })])
      ]));
    });
    sTable.appendChild(sTbody);
    sBody.appendChild(sTable);
    secrets.appendChild(sBody);
    wrap.appendChild(secrets);

    // Alert destinations
    var alerts = el("div", { class: "card" });
    alerts.appendChild(el("h3", { text: "Alert destinations" }));
    var aBody = el("div", { class: "body" });
    var alertName = el("input", { class: "input", placeholder: "name" });
    var alertURL = el("input", { class: "input", placeholder: "https://hooks.example.com/..." });
    var alertSecret = el("input", { class: "input", type: "password", placeholder: "optional signing secret" });
    aBody.appendChild(el("div", { class: "grid", style: "grid-template-columns:1fr 2fr 1fr;gap:8px;margin-bottom:12px" },
      [alertName, alertURL, alertSecret]));
    aBody.appendChild(el("button", {
      class: "btn primary mb", type: "button", text: "Add destination",
      onclick: function () {
        api("/api/v1/alerts", {
          method: "POST",
          body: JSON.stringify({ name: alertName.value, url: alertURL.value, secret: alertSecret.value })
        }).then(function () {
          toast("Destination added", "ok"); alertName.value = ""; alertURL.value = ""; alertSecret.value = ""; loadAlerts();
        }).catch(function (e) { toast(e.message, "err"); });
      }
    }));
    var aTable = el("table", { class: "tbl" });
    var aHead = el("thead");
    aHead.appendChild(el("tr", {}, [el("th", { text: "Name" }), el("th", { text: "URL" }), el("th", { text: "" })]));
    aTable.appendChild(aHead);
    var aTbody = el("tbody");
    S.alerts.forEach(function (destination) {
      aTbody.appendChild(el("tr", {}, [
        el("td", { text: destination.name }),
        el("td", { class: "mono", text: destination.url }),
        el("td", {}, [el("button", {
          class: "btn sm danger", type: "button", text: "Remove",
          onclick: function () {
            api("/api/v1/alerts/" + encodeURIComponent(destination.id), { method: "DELETE" })
              .then(function () { toast("Destination removed", "ok"); loadAlerts(); })
              .catch(function (e) { toast(e.message, "err"); });
          }
        })])
      ]));
    });
    aTable.appendChild(aTbody);
    aBody.appendChild(aTable);
    alerts.appendChild(aBody);
    wrap.appendChild(alerts);

    // Plugin catalog
    var plugins = el("div", { class: "card" });
    plugins.appendChild(el("h3", { text: "Plugin catalog" }));
    var pBody = el("div", { class: "body" });
    var pTable = el("table", { class: "tbl" });
    var pHead = el("thead");
    pHead.appendChild(el("tr", {}, [el("th", { text: "URI" }), el("th", { text: "Entrypoint" })]));
    pTable.appendChild(pHead);
    var pTbody = el("tbody");
    S.plugins.forEach(function (plugin) {
      pTbody.appendChild(el("tr", {}, [
        el("td", { class: "mono", text: plugin.name }),
        el("td", { class: "mono", text: plugin.entrypoint })
      ]));
    });
    pTable.appendChild(pTbody);
    pBody.appendChild(pTable);
    plugins.appendChild(pBody);
    wrap.appendChild(plugins);
  }

  return wrap;
}

/* ---------------------------------------------------------------- *
 * Auth screen
 * ---------------------------------------------------------------- */
function renderAuth() {
  var shell = el("div", { class: "auth-shell" });
  var card = el("div", { class: "card auth-card" });
  var brand = el("div", { class: "brand" });
  brand.appendChild(logo(32));
  brand.appendChild(el("span", { style: "font-size:26px;font-weight:700;letter-spacing:-.02em", text: "Barq" }));
  brand.appendChild(el("span", { class: "v", text: "CI" }));
  card.appendChild(brand);

  var tabs = el("div", { class: "auth-tabs" });
  var mode = S.authMode || "login";
  var loginTab = el("button", {
    type: "button", "aria-selected": mode === "login" ? "true" : "false",
    text: "Sign in", onclick: function () { S.authMode = "login"; render(); }
  });
  var signupTab = el("button", {
    type: "button", "aria-selected": mode === "signup" ? "true" : "false",
    text: "Create account", onclick: function () { S.authMode = "signup"; render(); }
  });
  tabs.appendChild(loginTab);
  if (S.signupEnabled) { tabs.appendChild(signupTab); }
  card.appendChild(tabs);

  var body = el("div", { class: "body" });
  if (S.authMessage) {
    body.appendChild(el("div", { class: "auth-msg " + (S.authMessage.kind || "err"), text: S.authMessage.text }));
  }

  var email = el("input", { class: "input", type: "email", placeholder: "you@example.com", autocomplete: "username" });
  var password = el("input", { class: "input", type: "password", placeholder: "password", autocomplete: mode === "login" ? "current-password" : "new-password" });
  var display = el("input", { class: "input", placeholder: "display name (optional)" });

  body.appendChild(el("label", { class: "field" }, [el("span", { text: "Email" }), email]));
  body.appendChild(el("label", { class: "field" }, [el("span", { text: "Password" }), password,
    mode === "signup" ? el("div", { class: "hint", text: "At least 12 characters, mixing letters with digits or symbols." }) : null]));
  if (mode === "signup") {
    body.appendChild(el("label", { class: "field" }, [el("span", { text: "Display name" }), display]));
  }

  var submit = el("button", {
    class: "btn primary", type: "button", text: mode === "login" ? "Sign in" : "Create account",
    style: "width:100%;justify-content:center"
  });
  submit.addEventListener("click", function () {
    submit.disabled = true;
    var path = mode === "login" ? "/api/v1/auth/login" : "/api/v1/auth/signup";
    var payload = mode === "login"
      ? { email: email.value, password: password.value }
      : { email: email.value, password: password.value, display_name: display.value };
    api(path, { method: "POST", body: JSON.stringify(payload) })
      .then(function () { S.authMessage = null; return loadSession(); })
      .catch(function (e) { S.authMessage = { kind: "err", text: e.message }; })
      .then(function () { submit.disabled = false; render(); });
  });
  body.appendChild(submit);

  if (S.oidcEnabled) {
    body.appendChild(el("div", { class: "divider", text: "or" }));
    body.appendChild(el("a", {
      class: "btn", style: "width:100%;justify-content:center", href: "/api/v1/auth/oidc", text: "Continue with single sign-on"
    }));
  }
  card.appendChild(body);
  shell.appendChild(card);
  return shell;
}

/* ---------------------------------------------------------------- *
 * Modal
 * ---------------------------------------------------------------- */
function modal(title, content, footer) {
  var bg = el("div", { class: "modal-bg" });
  function close() { bg.remove(); }
  bg.addEventListener("click", function (event) { if (event.target === bg) { close(); } });
  var box = el("div", { class: "modal" });
  var head = el("header");
  head.appendChild(el("h2", { text: title }));
  head.appendChild(el("button", { class: "icon-btn", type: "button", text: "×", onclick: close }));
  box.appendChild(head);
  box.appendChild(el("div", { class: "body" }, [content]));
  if (footer) { box.appendChild(el("footer", {}, footer)); }
  bg.appendChild(box);
  document.body.appendChild(bg);
  return { close: close, box: box };
}

function openSpawn() {
  var area = el("textarea", {
    class: "input", rows: "14",
    placeholder: "name: my-pipeline\nsteps:\n  - id: build\n    image: alpine:3.20\n    run:\n      - echo hello"
  });
  area.value = "name: my-pipeline\nsteps:\n  - id: build\n    image: alpine:3.20\n    run:\n      - echo hello\n";
  var handle = modal("Spawn a pipeline", area, [
    el("button", { class: "btn", type: "button", text: "Cancel", onclick: function () { handle.close(); } }),
    el("button", {
      class: "btn primary", type: "button", text: "Spawn",
      onclick: function () {
        fetch("/api/v1/runs", {
          method: "POST",
          headers: Object.assign({ "Content-Type": "application/yaml" }, S.csrftoken ? { "X-Barq-CSRF": S.csrftoken } : {}),
          credentials: "same-origin",
          body: area.value
        }).then(function (response) {
          return response.text().then(function (text) {
            if (!response.ok) { throw new Error(text); }
            return JSON.parse(text);
          });
        }).then(function (run) {
          handle.close(); toast("Pipeline spawned", "ok"); selectRun(run.id);
        }).catch(function (e) { toast(e.message, "err"); });
      }
    })
  ]);
}

/* ---------------------------------------------------------------- *
 * Actions
 * ---------------------------------------------------------------- */
function rebuild(mode) {
  if (!S.run) { return; }
  api("/api/v1/runs/" + S.run.id + "/rebuild", { method: "POST", body: JSON.stringify({ mode: mode }) })
    .then(function (run) { toast("Rebuild started", "ok"); selectRun(run.id); })
    .catch(function (e) { toast(e.message, "err"); });
}
function rebuildWave() {
  if (!S.run) { return; }
  var wave = 0;
  if (S.selectedStep && S.run.steps[S.selectedStep]) { wave = S.run.steps[S.selectedStep].wave; }
  api("/api/v1/runs/" + S.run.id + "/rebuild", { method: "POST", body: JSON.stringify({ mode: "wave", wave: wave }) })
    .then(function (run) { toast("Wave " + wave + " restarted", "ok"); selectRun(run.id); })
    .catch(function (e) { toast(e.message, "err"); });
}
function rebuildStep() {
  if (!S.run || !S.selectedStep) { toast("Select a step first", "err"); return; }
  api("/api/v1/runs/" + S.run.id + "/rebuild", { method: "POST", body: JSON.stringify({ mode: "step", step_id: S.selectedStep }) })
    .then(function (run) { toast("Step restarted", "ok"); selectRun(run.id); })
    .catch(function (e) { toast(e.message, "err"); });
}
function stopRun() {
  if (!S.run) { return; }
  api("/api/v1/runs/" + S.run.id + "/stop", { method: "POST" })
    .then(function () { toast("Run stopped", "ok"); })
    .catch(function (e) { toast(e.message, "err"); });
}
function selectRun(id) {
  S.selectedStep = null;
  api("/api/v1/runs/" + id).then(function (run) {
    S.run = run; S.tab = "runs"; render(); openLogs(id);
  }).catch(function (e) { toast(e.message, "err"); });
}
function openLogs(id) {
  if (S.logStream) { S.logStream.close(); S.logStream = null; }
  var box = document.getElementById("logbox");
  if (box) { box.textContent = ""; }
  var stream = new EventSource("/api/v1/runs/" + id + "/logs");
  S.logStream = stream;
  stream.addEventListener("log", function (event) {
    var target = document.getElementById("logbox");
    if (!target) { return; }
    try {
      var chunk = JSON.parse(event.data);
      // Syntax highlight: color error lines
      var lines = chunk.split("\n");
      var html = "";
      lines.forEach(function(line) {
        var cls = "";
        if (/error|fail|panic|fatal/i.test(line)) cls = " class=\"err\"";
        else if (/warn/i.test(line)) cls = " class=\"warn\"";
        else if (/success|pass|ok|done/i.test(line)) cls = " class=\"ok\"";
        else if (/info/i.test(line)) cls = " class=\"info\"";
        html += "<div class=\"line\" + " + (cls ? cls : "") + ">" + esc(line) + "</div>";
      });
      target.innerHTML = html;
      target.scrollTop = target.scrollHeight;
    } catch (e) { /* ignore malformed frames */ }
  });
  stream.onerror = function () { /* EventSource reconnects on its own */ };
}

/* ---------------------------------------------------------------- *
 * Data loading
 * ---------------------------------------------------------------- */
function loadSession() {
  return fetch("/api/v1/auth/status").then(function (r) { return r.json(); }).then(function (status) {
    S.authRequired = status.required;
    S.signupEnabled = status.signup_enabled;
    S.oidcEnabled = status.oidc_enabled;
    return fetch("/api/v1/auth/session", { credentials: "same-origin" }).then(function (r) { return r.json(); });
  }).then(function (payload) {
    S.user = payload.user || null;
    S.csrftoken = readCookie("barq_csrf");
    if (S.user) { return loadRuns(); }
  });
}
function loadRuns() {
  return api("/api/v1/runs").then(function (payload) {
    S.runs = payload.runs || [];
    if (!S.run && S.runs.length) { S.run = S.runs[0]; }
    if (S.run) {
      return api("/api/v1/runs/" + S.run.id).then(function (run) { S.run = run; });
    }
  });
}
function loadStats() {
  return api("/api/v1/stats").then(function (data) { S.stats = data; })
    .catch(function () { /* statistics are best effort */ });
}
function loadHealth() {
  return api("/api/v1/system/health").then(function (data) { S.health = data; })
    .catch(function () { /* health is best effort */ });
}
function loadUsers() {
  if (!S.user || S.user.role !== "admin") { return Promise.resolve(); }
  return api("/api/v1/users").then(function (data) { S.users = data.users || []; }).catch(function () {});
}
function loadSecrets() {
  if (!S.user || S.user.role !== "admin") { return Promise.resolve(); }
  return api("/api/v1/secrets").then(function (data) { S.secrets = data.secrets || []; }).catch(function () {});
}
function loadAlerts() {
  if (!S.user || S.user.role !== "admin") { return Promise.resolve(); }
  return api("/api/v1/alerts").then(function (data) { S.alerts = data.destinations || []; }).catch(function () {});
}
function loadPlugins() {
  return api("/api/v1/plugins").then(function (data) { S.plugins = data.plugins || []; }).catch(function () {});
}

/* ---------------------------------------------------------------- *
 * Boot
 * ---------------------------------------------------------------- */
function tick() {
  if (!S.user) { return; }
  loadRuns().then(function () {
    if (S.tab === "stats") { loadStats(); }
    render();
  }).catch(function () { /* keep the last rendered state */ });
}

document.documentElement.setAttribute("data-theme", S.theme);

loadSession().then(function () {
  if (S.user) { loadHealth(); loadPlugins(); }
  render();
  setInterval(tick, 2500);
  setInterval(function () { if (S.user) { loadHealth(); } }, 10000);
}).catch(function (error) {
  S.authMessage = { kind: "err", text: error.message };
  render();
});
</script>
</body>
</html>
