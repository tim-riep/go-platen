// platen web panel. Plain JavaScript without dependencies; it talks to the
// panel API under /panel/v1. Every value from the server is inserted as
// text, never as HTML.
"use strict";

const state = { me: null, tab: "overview", timer: null };
const isAdmin = () => state.me && state.me.role === "admin";

// ---- helpers ----

function el(tag, attrs, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "class") e.className = v;
    else if (v === true) e.setAttribute(k, "");
    else e.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

class APIError extends Error {
  constructor(status, code, message) { super(message); this.status = status; this.code = code; }
}

async function api(method, path, body) {
  const opts = { method, headers: { "X-Platen-Panel": "1" } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch("/panel/v1" + path, opts);
  if (res.status === 204) return null;
  let data = null;
  try { data = await res.json(); } catch (_) { /* empty body */ }
  if (!res.ok) {
    const e = (data && data.error) || {};
    throw new APIError(res.status, e.code || "error", e.message || res.statusText);
  }
  return data;
}

// upload sends a file as request body, as the application API expects.
async function upload(path, file) {
  let type = file.type;
  if (!type) type = file.name.toLowerCase().endsWith(".pdf") ? "application/pdf" : "application/octet-stream";
  const res = await fetch("/panel/v1" + path, { method: "POST", headers: { "X-Platen-Panel": "1", "Content-Type": type }, body: file });
  let data = null;
  try { data = await res.json(); } catch (_) { /* empty body */ }
  if (!res.ok) {
    const e = (data && data.error) || {};
    throw new APIError(res.status, e.code || "error", e.message || res.statusText);
  }
  return data;
}

function fmtTime(s) {
  if (!s) return "never";
  const d = new Date(s);
  const sec = Math.round((Date.now() - d.getTime()) / 1000);
  if (sec < 60) return "just now";
  if (sec < 3600) return Math.round(sec / 60) + " min ago";
  if (sec < 86400) return Math.round(sec / 3600) + " h ago";
  return d.toLocaleString();
}

function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

function pill(text, kind) { return el("span", { class: "pill " + kind }, text); }

const jobKinds = { queued: "neutral", leased: "warn", printing: "warn", done: "ok", failed: "bad", canceled: "neutral" };

function online(w) {
  return w.last_heartbeat && Date.now() - new Date(w.last_heartbeat).getTime() < 120000;
}

function notice(text, bad) {
  const n = document.getElementById("notice");
  n.textContent = "";
  if (!text) { n.hidden = true; return; }
  n.className = "notice" + (bad ? " bad" : "");
  n.append(text);
  n.hidden = false;
}

function table(headers, rows, emptyText) {
  if (!rows.length) return el("div", { class: "card" }, el("div", { class: "empty" }, emptyText));
  return el("div", { class: "card" },
    el("table", {},
      el("thead", {}, el("tr", {}, headers.map(h => el("th", {}, h)))),
      el("tbody", {}, rows)));
}

function head(title, ...tools) {
  return el("div", { class: "head" }, el("h1", {}, title), el("div", { class: "toolbar" }, tools));
}

function adminButton(label, onclick, cls) {
  if (!isAdmin()) return null;
  return el("button", { class: cls || "", type: "button", onclick }, label);
}

// ---- dialog ----

// openDialog shows a form. fields: [{name, label, type, value, options,
// hint, required, placeholder}]. submit receives the values and may throw
// an APIError, which is shown in the dialog.
function openDialog(title, fields, submit, okLabel) {
  const dlg = document.getElementById("dialog");
  const body = document.getElementById("dialog-body");
  const err = document.getElementById("dialog-error");
  document.getElementById("dialog-title").textContent = title;
  document.getElementById("dialog-ok").textContent = okLabel || "Save";
  document.getElementById("dialog-ok").hidden = !submit;
  document.getElementById("dialog-cancel").textContent = submit ? "Cancel" : "Close";
  body.textContent = "";
  err.hidden = true;
  const inputs = {};
  for (const f of fields) {
    if (f.type === "text-block") { body.append(f.node); continue; }
    const id = "f-" + f.name;
    let input;
    if (f.type === "select") {
      input = el("select", { id, name: f.name }, f.options.map(o => {
        const [v, l] = Array.isArray(o) ? o : [o, o];
        return el("option", { value: v, selected: String(f.value) === String(v) }, l);
      }));
    } else if (f.type === "textarea") {
      input = el("textarea", { id, name: f.name, rows: 4, placeholder: f.placeholder || "" });
      input.value = f.value || "";
    } else if (f.type === "checkbox") {
      input = el("input", { id, name: f.name, type: "checkbox", checked: !!f.value });
      body.append(el("div", { class: "field" }, el("label", { class: "check" }, input, f.label)));
      inputs[f.name] = input;
      continue;
    } else if (f.type === "checklist") {
      const box = el("div", { class: "field" }, el("label", {}, f.label));
      const checks = f.options.map(o => {
        const c = el("input", { type: "checkbox", value: o, checked: (f.value || []).includes(o) });
        box.append(el("label", { class: "check" }, c, o));
        return c;
      });
      if (!f.options.length) box.append(el("span", { class: "hint" }, "No printers yet."));
      body.append(box);
      inputs[f.name] = { checklist: checks };
      continue;
    } else if (f.type === "file") {
      input = el("input", { id, name: f.name, type: "file", required: f.required, accept: f.accept });
    } else {
      input = el("input", { id, name: f.name, type: f.type || "text", required: f.required, placeholder: f.placeholder || "", readonly: f.readonly,
        min: f.min, max: f.max });
      input.value = f.value === undefined || f.value === null ? "" : f.value;
    }
    inputs[f.name] = input;
    body.append(el("div", { class: "field" }, el("label", { for: id }, f.label), input, f.hint ? el("span", { class: "hint" }, f.hint) : null));
  }
  const values = () => {
    const out = {};
    for (const [k, i] of Object.entries(inputs)) {
      if (i.checklist) out[k] = i.checklist.filter(c => c.checked).map(c => c.value);
      else if (i.type === "file") out[k] = i.files[0] || null;
      else if (i.type === "checkbox") out[k] = i.checked;
      else out[k] = i.value.trim();
    }
    return out;
  };
  const form = document.getElementById("dialog-form");
  form.onsubmit = async (ev) => {
    ev.preventDefault();
    if (!submit) { dlg.close(); return; }
    try {
      const next = await submit(values());
      if (next) next(); else dlg.close();
    } catch (e) {
      err.textContent = e.message;
      err.hidden = false;
    }
  };
  document.getElementById("dialog-cancel").onclick = () => dlg.close();
  if (!dlg.open) dlg.showModal();
}

function showSecret(title, text, secret) {
  openDialog(title, [{ type: "text-block", node: el("div", {}, el("p", {}, text), el("div", { class: "secret" }, secret)) }], null);
}

async function confirmAction(title, text, okLabel, action) {
  openDialog(title, [{ type: "text-block", node: el("p", {}, text) }], async () => { await action(); render(); }, okLabel);
}

// ---- views ----

async function viewOverview() {
  const [printers, workers, jobs] = await Promise.all([api("GET", "/printers"), api("GET", "/workers"), api("GET", "/jobs?limit=200")]);
  const p = printers.items, w = workers.items, j = jobs.items;
  const count = s => j.filter(x => x.status === s).length;
  const tile = (n, l) => el("div", { class: "tile" }, el("div", { class: "n" }, n), el("div", { class: "l" }, l));
  return el("div", {},
    head("Overview"),
    el("div", { class: "tiles" },
      tile(p.filter(x => x.reachable).length + " / " + p.length, "printers reachable"),
      tile(w.filter(online).length + " / " + w.length, "workers online"),
      tile(count("queued") + count("leased") + count("printing"), "jobs waiting or printing"),
      tile(count("failed"), "failed among the last " + j.length + " jobs")),
    el("h2", {}, "Printers"),
    printerTable(p, w, false));
}

function printerStatus(p) {
  const status = p.status === "paused" ? pill("paused", "warn") : pill("ready", "ok");
  const reach = p.reachable === null || p.reachable === undefined ? pill("unknown", "neutral")
    : p.reachable ? pill("reachable", "ok") : pill("unreachable", "bad");
  return el("span", { class: "toolbar" }, status, reach);
}

function printerTable(printers, workers, withActions) {
  const name = id => (workers.find(w => w.id === id) || {}).name || id;
  return table(["Printer", "Worker", "Address", "Backend", "Status", ""], printers.map(p => el("tr", {},
    el("td", {}, el("strong", {}, p.name), el("div", { class: "muted mono" }, p.id)),
    el("td", {}, name(p.worker_id)),
    el("td", { class: "mono" }, p.address),
    el("td", {}, p.backend, el("div", { class: "muted" }, p.paper_size + ", " + p.resolution_dpi + " dpi")),
    el("td", {}, printerStatus(p), p.reachability_error ? el("div", { class: "muted" }, p.reachability_error) : null),
    el("td", { class: "actions" }, withActions ? [
      adminButton("Print…", () => printFile(p, printers), "link"),
      adminButton("Edit", () => editPrinter(p, workers), "link"),
      adminButton(p.status === "paused" ? "Resume" : "Pause", async () => {
        await api("PATCH", "/printers/" + encodeURIComponent(p.id), { status: p.status === "paused" ? "ready" : "paused" });
        render();
      }, "link"),
      adminButton("Delete", () => confirmAction("Delete printer", "Delete printer " + p.id + "? Printers with unfinished jobs cannot be deleted.", "Delete",
        () => api("DELETE", "/printers/" + encodeURIComponent(p.id))), "link"),
    ] : null))), "No printers yet.");
}

async function viewPrinters() {
  const [printers, workers] = await Promise.all([api("GET", "/printers"), api("GET", "/workers")]);
  return el("div", {},
    head("Printers", adminButton("Add printer", () => editPrinter(null, workers.items))),
    printerTable(printers.items, workers.items, true));
}

function optionsText(o) { return Object.entries(o || {}).map(([k, v]) => k + "=" + v).join("\n"); }

function parseOptions(text) {
  const out = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i <= 0) throw new Error("Options must be key=value, one per line: " + t);
    out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}

async function editPrinter(p, workers) {
  const backends = (await api("GET", "/backends")).items.map(b => b.name);
  const isNew = !p;
  p = p || { paper_size: "A4", resolution_dpi: 600, status: "ready", backend: backends[0], worker_id: (workers[0] || {}).id };
  openDialog(isNew ? "Add printer" : "Edit printer " + p.id, [
    { name: "id", label: "ID", value: p.id, required: isNew, readonly: !isNew, hint: isNew ? "Lower case letters, digits, - and _; used in the API." : "" },
    { name: "name", label: "Name", value: p.name },
    { name: "worker_id", label: "Worker", type: "select", value: p.worker_id, options: workers.map(w => [w.id, w.name + " (" + w.id + ")"]) },
    { name: "address", label: "Address", value: p.address, required: true, placeholder: "192.168.1.20 or host:9100", hint: "In the worker's local network; port 9100 if omitted." },
    { name: "backend", label: "Backend", type: "select", value: p.backend, options: backends },
    { name: "paper_size", label: "Paper", type: "select", value: p.paper_size, options: ["A4", "A5", "A6", "Letter", "Legal"] },
    { name: "resolution_dpi", label: "Resolution", type: "select", value: p.resolution_dpi, options: [[300, "300 dpi"], [600, "600 dpi"], [1200, "1200 dpi"]] },
    { name: "status", label: "Status", type: "select", value: p.status, options: [["ready", "Ready"], ["paused", "Paused"]] },
    { name: "options", label: "Options", type: "textarea", value: optionsText(p.options), placeholder: "dither=halftone", hint: "One key=value per line, e.g. dither, source, media, toner_save." },
  ], async v => {
    const body = { name: v.name || v.id, worker_id: v.worker_id, address: v.address, backend: v.backend, paper_size: v.paper_size,
      resolution_dpi: Number(v.resolution_dpi), status: v.status, options: parseOptions(v.options) };
    if (isNew) await api("POST", "/printers", Object.assign({ id: v.id }, body));
    else await api("PATCH", "/printers/" + encodeURIComponent(p.id), body);
    render();
  });
}

// printFile sends a document to a printer, like the ERP does. Without a
// printer the dialog offers a choice.
function printFile(printer, printers) {
  const fields = [];
  if (!printer) {
    fields.push({ name: "printer", label: "Printer", type: "select", value: (printers[0] || {}).id,
      options: printers.map(p => [p.id, p.name + " (" + p.backend + ")"]) });
  }
  fields.push(
    { name: "file", label: "File", type: "file", required: true,
      hint: "PDF for pdf-to-raster and brother-raster printers; raw printers receive the file unchanged (e.g. ZPL)." },
    { name: "copies", label: "Copies", type: "number", value: 1, min: 1, max: 999 },
    { name: "title", label: "Title", placeholder: "defaults to the file name" });
  openDialog(printer ? "Print on " + printer.name : "Print a file", fields, async v => {
    if (!v.file) throw new Error("Choose a file.");
    const id = printer ? printer.id : v.printer;
    const params = new URLSearchParams({ copies: v.copies || "1", title: v.title || v.file.name });
    const job = await upload("/printers/" + encodeURIComponent(id) + "/jobs?" + params, v.file);
    state.tab = "jobs";
    state.jobFilter = {};
    history.replaceState(null, "", "#jobs");
    render();
    notice("Job " + job.id + " queued for " + id + ".");
  }, "Print");
}

async function viewWorkers() {
  const workers = (await api("GET", "/workers")).items;
  return el("div", {},
    head("Workers", adminButton("Add worker", () => editWorker(null))),
    table(["Worker", "Location", "State", "NetBird peer", "Version", ""], workers.map(w => el("tr", {},
      el("td", {}, el("strong", {}, w.name), el("div", { class: "muted mono" }, w.id)),
      el("td", {}, w.location || ""),
      el("td", {}, w.id === "local" ? pill("built in", "neutral") : !w.enrolled ? pill("not enrolled", "warn") : null, " ",
        online(w) ? pill("online", "ok") : pill("offline", "bad"), el("div", { class: "muted" }, "heartbeat " + fmtTime(w.last_heartbeat))),
      el("td", { class: "mono" }, w.peer_fqdn || ""),
      el("td", { class: "mono" }, w.version || ""),
      el("td", { class: "actions" }, w.id === "local" ? null : [
        adminButton("Edit", () => editWorker(w), "link"),
        adminButton("Enrollment token", () => enroll(w), "link"),
        adminButton("Delete", () => confirmAction("Delete worker", "Delete worker " + w.id + "? Workers with printers cannot be deleted.", "Delete",
          () => api("DELETE", "/workers/" + encodeURIComponent(w.id))), "link"),
      ]))), "No workers yet."));
}

function editWorker(w) {
  const isNew = !w;
  w = w || {};
  openDialog(isNew ? "Add worker" : "Edit worker " + w.id, [
    { name: "id", label: "ID", value: w.id, required: isNew, readonly: !isNew },
    { name: "name", label: "Name", value: w.name },
    { name: "location", label: "Location", value: w.location },
  ], async v => {
    if (isNew) {
      await api("POST", "/workers", { id: v.id, name: v.name || v.id, location: v.location });
      render();
      return () => enroll({ id: v.id });
    }
    await api("PATCH", "/workers/" + encodeURIComponent(w.id), { name: v.name, location: v.location });
    render();
  });
}

function enroll(w) {
  openDialog("Enrollment token for " + w.id, [
    { name: "ttl", label: "Valid for", type: "select", value: 86400, options: [[3600, "1 hour"], [86400, "24 hours"], [604800, "7 days"]] },
  ], async v => {
    const t = await api("POST", "/workers/" + encodeURIComponent(w.id) + "/enrollment-tokens", { ttl_seconds: Number(v.ttl) });
    return () => showSecret("Enrollment token", "Single use, valid until " + new Date(t.expires_at).toLocaleString() +
      ". Give it to the worker as enrollment_token_file or PLATEN_ENROLLMENT_TOKEN. It is shown only now.", t.token);
  }, "Create token");
}

async function viewJobs() {
  const params = new URLSearchParams();
  const f = state.jobFilter || {};
  if (f.status) params.set("status", f.status);
  if (f.printer) params.set("printer", f.printer);
  params.set("limit", "200");
  const [jobs, printers] = await Promise.all([api("GET", "/jobs?" + params), api("GET", "/printers")]);
  const filter = (name, options) => el("select", { "aria-label": name, onchange: e => { state.jobFilter = Object.assign({}, f, { [name]: e.target.value }); render(); } },
    options.map(([v, l]) => el("option", { value: v, selected: (f[name] || "") === v }, l)));
  return el("div", {},
    head("Jobs", adminButton("Print a file", () => printFile(null, printers.items)), el("div", { class: "filters" },
      filter("status", [["", "All states"], ...Object.keys(jobKinds).map(s => [s, s])]),
      filter("printer", [["", "All printers"], ...printers.items.map(p => [p.id, p.name])]))),
    table(["Job", "Printer", "Status", "Document", "Created", ""], jobs.items.map(j => el("tr", {},
      el("td", {}, el("div", { class: "mono" }, j.id), j.title ? el("div", {}, j.title) : null, el("div", { class: "muted" }, j.app_client_id || "")),
      el("td", {}, j.printer_id),
      el("td", {}, pill(j.status, jobKinds[j.status] || "neutral"), j.attempts > 1 ? el("div", { class: "muted" }, j.attempts + " attempts") : null,
        j.error ? el("div", { class: "muted" }, j.error) : null),
      el("td", {}, j.content_type, el("div", { class: "muted" }, fmtSize(j.size) + (j.copies > 1 ? ", " + j.copies + " copies" : ""))),
      el("td", {}, fmtTime(j.created_at)),
      el("td", { class: "actions" }, jobActions(j)))), "No jobs."));
}

function jobActions(j) {
  const path = "/jobs/" + encodeURIComponent(j.id);
  const busy = j.status === "leased" || j.status === "printing";
  return [
    j.status === "queued" ? adminButton("Cancel", () => confirmAction("Cancel job", "Cancel job " + j.id + "?", "Cancel job",
      () => api("POST", path + "/cancel")), "link") : null,
    j.status === "failed" || j.status === "canceled" ? adminButton("Retry", () => confirmAction("Retry job",
      "Put job " + j.id + " back into the queue of " + j.printer_id + " with the same document?" +
      (j.error && j.error.startsWith("uncertain") ? " The job may already have been printed." : ""), "Retry",
      () => api("POST", path + "/retry")), "link") : null,
    busy ? null : adminButton("Delete", () => confirmAction("Delete job", "Delete job " + j.id + " and its document?", "Delete",
      () => api("DELETE", path)), "link"),
  ];
}

async function viewClients() {
  const [clients, printers] = await Promise.all([api("GET", "/app-clients"), api("GET", "/printers")]);
  return el("div", {},
    head("App clients", adminButton("Add app client", () => editClient(null, printers.items))),
    table(["App client", "Printers", "Created", ""], clients.items.map(c => el("tr", {},
      el("td", {}, el("strong", {}, c.name), el("div", { class: "muted mono" }, c.id)),
      el("td", {}, c.all_printers ? pill("all printers", "ok") : c.printers.length ? c.printers.join(", ") : el("span", { class: "muted" }, "none")),
      el("td", {}, fmtTime(c.created_at)),
      el("td", { class: "actions" },
        adminButton("Edit", () => editClient(c, printers.items), "link"),
        adminButton("New token", () => confirmAction("New token", "Replace the token of " + c.id + "? The old token stops working immediately.", "Replace token", async () => {
          const r = await api("POST", "/app-clients/" + encodeURIComponent(c.id) + "/rotate-token");
          setTimeout(() => showSecret("New token for " + c.id, "Use it as Bearer token for the application API. It is shown only now.", r.token), 0);
        }), "link"),
        adminButton("Delete", () => confirmAction("Delete app client", "Delete app client " + c.id + "? Its token stops working.", "Delete",
          () => api("DELETE", "/app-clients/" + encodeURIComponent(c.id))), "link")))), "No app clients yet."));
}

function editClient(c, printers) {
  const isNew = !c;
  c = c || { printers: [] };
  openDialog(isNew ? "Add app client" : "Edit app client " + c.id, [
    { name: "id", label: "ID", value: c.id, required: isNew, readonly: !isNew },
    { name: "name", label: "Name", value: c.name },
    { name: "all_printers", label: "Allow all printers, including future ones", type: "checkbox", value: c.all_printers },
    { name: "printers", label: "Allowed printers", type: "checklist", value: c.printers, options: printers.map(p => p.id) },
  ], async v => {
    const body = { name: v.name || v.id, all_printers: v.all_printers, printers: v.printers };
    if (isNew) {
      const r = await api("POST", "/app-clients", Object.assign({ id: v.id }, body));
      render();
      return () => showSecret("Token for " + r.id, "Use it as Bearer token for the application API. It is shown only now.", r.token);
    }
    await api("PATCH", "/app-clients/" + encodeURIComponent(c.id), body);
    render();
  });
}

async function viewUsers() {
  const users = (await api("GET", "/users")).items;
  return el("div", {},
    head("Panel users", adminButton("Add user", () => editUser(null))),
    el("p", { class: "muted" }, "Users are identified by their NetBird IPv4 address. Access also requires the NetBird peer that first used the address; reset the binding when a device was re-registered in NetBird."),
    table(["NetBird IP", "Name", "Role", "Peer", "Last visit", ""], users.map(u => el("tr", {},
      el("td", { class: "mono" }, u.ip, u.ip === state.me.ip ? el("div", { class: "muted" }, "you") : null),
      el("td", {}, u.name || ""),
      el("td", {}, pill(u.role, u.role === "admin" ? "ok" : "neutral")),
      el("td", {}, u.bound ? el("span", { class: "mono" }, u.peer_fqdn || u.peer_id) : pill("bound on first visit", "warn")),
      el("td", {}, fmtTime(u.last_seen)),
      el("td", { class: "actions" },
        adminButton("Edit", () => editUser(u), "link"),
        u.bound ? adminButton("Reset peer", () => confirmAction("Reset peer binding", "The next visit from " + u.ip + " binds the NetBird peer again.", "Reset",
          () => api("POST", "/users/" + encodeURIComponent(u.ip) + "/reset-peer")), "link") : null,
        adminButton("Remove", () => confirmAction("Remove user", "Remove " + u.ip + " from the panel?", "Remove",
          () => api("DELETE", "/users/" + encodeURIComponent(u.ip))), "link")))), "No users."));
}

function editUser(u) {
  const isNew = !u;
  u = u || { role: "viewer" };
  openDialog(isNew ? "Add panel user" : "Edit " + u.ip, [
    { name: "ip", label: "NetBird IPv4 address", value: u.ip, required: isNew, readonly: !isNew, placeholder: "100.64.0.12", hint: isNew ? "The peer behind the address is bound on its first visit." : "" },
    { name: "name", label: "Name", value: u.name },
    { name: "role", label: "Role", type: "select", value: u.role, options: [["viewer", "Viewer (read only)"], ["admin", "Admin"]] },
  ], async v => {
    if (isNew) await api("POST", "/users", { ip: v.ip, name: v.name, role: v.role });
    else await api("PATCH", "/users/" + encodeURIComponent(u.ip), { name: v.name, role: v.role });
    render();
  });
}

const views = { overview: viewOverview, printers: viewPrinters, workers: viewWorkers, jobs: viewJobs, clients: viewClients, users: viewUsers };

// ---- shell ----

async function render() {
  clearTimeout(state.timer);
  for (const b of document.querySelectorAll("#tabs button")) {
    b.hidden = b.hasAttribute("data-admin") && !isAdmin();
    b.toggleAttribute("aria-current", b.dataset.tab === state.tab);
    if (b.dataset.tab === state.tab) b.setAttribute("aria-current", "page");
  }
  const view = document.getElementById("view");
  try {
    const node = await views[state.tab]();
    view.textContent = "";
    view.append(node);
  } catch (e) {
    view.textContent = "";
    view.append(el("div", { class: "notice bad" }, e.message));
  }
  if (state.tab === "overview" || state.tab === "jobs") {
    state.timer = setTimeout(() => { if (!document.getElementById("dialog").open) render(); else state.timer = setTimeout(render, 10000); }, 10000);
  }
}

async function start() {
  for (const b of document.querySelectorAll("#tabs button")) {
    b.addEventListener("click", () => {
      notice("");
      state.tab = b.dataset.tab;
      history.replaceState(null, "", "#" + state.tab);
      render();
    });
  }
  const fromHash = location.hash.slice(1);
  if (views[fromHash]) state.tab = fromHash;
  try {
    state.me = await api("GET", "/me");
  } catch (e) {
    document.getElementById("tabs").hidden = true;
    notice(e.message, true);
    return;
  }
  const me = document.getElementById("me");
  me.append(el("strong", {}, state.me.name || state.me.peer_fqdn || state.me.ip), " · ", state.me.role, el("div", { class: "mono" }, state.me.ip + " · platen " + state.me.version));
  if (state.me.first_admin) {
    notice("You are the first user of this panel and became its admin. Add further users under Panel users; they are identified by their NetBird IPv4 address.");
  }
  if (state.tab === "users" && !isAdmin()) state.tab = "overview";
  render();
}

start();
