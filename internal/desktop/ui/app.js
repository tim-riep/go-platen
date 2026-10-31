"use strict";

const $ = (id) => document.getElementById(id);
let state = null;
let editing = false;

async function call(method, path, body) {
  const opts = { method, headers: { "X-Platen-Desktop": "1" } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch("api/" + path, opts);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || resp.statusText);
  return data;
}

function el(tag, props, ...children) {
  const e = document.createElement(tag);
  Object.assign(e, props || {});
  for (const c of children) if (c != null) e.append(c);
  return e;
}

function show(id, on) { $(id).hidden = !on; }

function render(st) {
  state = st;
  if (!editing && document.activeElement !== $("address")) $("address").value = st.address || "";
  const connected = !!st.address;
  $("connect-btn").textContent = connected && $("address").value.trim() === st.address ? "Update printers" : "Connect & install printers";
  $("connect-btn").disabled = st.busy;
  $("address").readOnly = !!st.locked;
  show("locked", !!st.locked);

  const me = $("me");
  me.replaceChildren();
  if (st.me) {
    const row = (k, v) => { if (v) me.append(el("dt", { textContent: k }), el("dd", { textContent: v })); };
    row("This device", `${st.me.fqdn} (${st.me.ip})`);
    row("NetBird user", st.me.netbird_user);
    row("Panel role", st.me.panel_role);
    row("Server version", st.me.version);
  }
  show("me", !!st.me);
  $("error").textContent = st.error || "";
  show("error", !!st.error);
  $("receiver-error").textContent = st.receiver_error || "";
  show("receiver-error", !!st.receiver_error);

  show("printers-section", connected);
  const list = $("printers");
  list.replaceChildren();
  if (connected && st.printers.length === 0) list.append(el("li", { className: "hint", textContent: "The server has no printers." }));
  for (const p of st.printers) {
    let badge = ["ready", "ok"];
    if (p.status !== "ready") badge = [p.status, "warn"];
    else if (p.reachable === false) badge = ["unreachable", "warn"];
    const detail = [p.id, p.paper_size];
    if (!p.accepts_pdf) detail.push("no PDF support, not installed");
    else if (p.local_name) detail.push("installed as “" + p.local_name + "”");
    else detail.push("not installed");
    list.append(el("li", {},
      el("span", { className: p.local_name ? "ok" : "warn", textContent: p.local_name ? "✓" : "○" }),
      el("div", { className: "grow" }, p.name, el("small", { textContent: detail.join(" · ") })),
      el("span", { className: "badge " + badge[1], textContent: badge[0] })));
  }
  $("last-sync").textContent = st.last_sync ? "Updated " + new Date(st.last_sync).toLocaleTimeString() + "." : "";
  $("sync").disabled = st.busy;

  show("jobs-section", st.jobs.length > 0);
  const jobs = $("jobs");
  jobs.replaceChildren();
  for (const j of st.jobs) {
    const text = j.failed ? "Not printed: " + (j.last_error || "") : "Waiting: " + (j.last_error || "sending");
    const btn = el("button", { type: "button", textContent: "Discard" });
    btn.onclick = () => act(() => call("POST", "jobs/" + encodeURIComponent(j.id) + "/discard"));
    jobs.append(el("li", {},
      el("div", { className: "grow" }, j.title, el("small", { className: j.failed ? "bad" : "", textContent: text })), btn));
  }
  $("retry").disabled = st.waiting === 0;

  show("login-row", st.login !== undefined && st.login !== null);
  $("login").checked = !!st.login;
  show("disconnect", connected && !st.locked);
  $("disconnect").disabled = st.busy;
  $("version").textContent = "Platen " + st.version;
}

async function act(f) {
  try {
    render(await f());
  } catch (e) {
    await refresh();
    $("error").textContent = e.message;
    show("error", true);
  }
}

async function refresh() {
  try { render(await call("GET", "state")); } catch (e) { /* app stopped */ }
}

$("address").addEventListener("input", () => { editing = true; if (state) render(state); });
$("connect").addEventListener("submit", (ev) => {
  ev.preventDefault();
  editing = false;
  $("connect-btn").disabled = true;
  act(() => call("POST", "connect", { address: $("address").value.trim() }));
});
$("sync").onclick = () => act(() => call("POST", "sync"));
$("retry").onclick = () => act(() => call("POST", "retry"));
$("login").onchange = () => act(() => call("POST", "login", { enabled: $("login").checked }));
$("disconnect").onclick = () => {
  if (confirm("Remove the platen printers from this computer?")) act(() => call("POST", "disconnect"));
};

refresh();
setInterval(refresh, 3000);
