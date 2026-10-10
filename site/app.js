"use strict";

const app = document.getElementById("app");
const crumbs = document.getElementById("crumbs");

// ---- helpers ----
const el = (html) => { const t = document.createElement("template"); t.innerHTML = html.trim(); return t.content.firstChild; };
const esc = (s) => String(s == null ? "" : s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const pctFloat = (v) => (v == null ? 0 : v);

function barClass(pct) {
  if (pct >= 70) return "good";
  if (pct >= 30) return "warn";
  return "bad";
}

function pctCell(pct, label) {
  const p = pctFloat(pct);
  return `<div class="pctcell"><span>${p.toFixed(1)}%</span><div class="bar mini ${barClass(p)}"><span style="width:${Math.min(p, 100)}%"></span></div></div>`
    + (label ? `<div class="muted" style="font-size:11px;text-align:right">${esc(label)}</div>` : "");
}

async function getJSON(path) {
  const res = await fetch(path, { cache: "no-cache" });
  if (!res.ok) throw new Error(`${path}: ${res.status}`);
  return res.json();
}

function setCrumbs(parts) {
  crumbs.innerHTML = parts
    .map((p, i) => (p.href ? `<a href="${p.href}">${esc(p.text)}</a>` : `<span>${esc(p.text)}</span>`) + (i < parts.length - 1 ? '<span class="sep">/</span>' : ""))
    .join("");
}

function card(label, value, sub) {
  return `<div class="card"><div class="label">${esc(label)}</div><div class="value">${esc(value)}${sub ? ` <small>${esc(sub)}</small>` : ""}</div></div>`;
}

function originRows(byOrigin) {
  const order = [["service", "service logic"], ["generated", "generated (validators)"], ["library", "library"]];
  return order.map(([key, label]) => {
    const o = byOrigin[key] || {};
    const emph = key === "service" ? ' style="font-weight:600"' : "";
    return `<div class="kv"${emph}><span class="k">${label}: present / negative</span>`
      + `<span>${o.present_arm_exercised || 0}/${o.total || 0} (${(o.present_arm_pct || 0).toFixed(1)}%)`
      + ` &middot; ${o.negative_arm_exercised || 0} (${(o.negative_arm_pct || 0).toFixed(1)}%)</span></div>`;
  }).join("");
}

// ---- views ----
async function viewServices() {
  setCrumbs([{ text: "Services" }]);
  app.innerHTML = `<div class="loading">Loading services...</div>`;
  const data = await getJSON("data/services.json");
  const list = data.services.map((s) => `
    <a class="service-card" href="#/${encodeURIComponent(s.service)}">
      <div class="name">${esc(s.service)}</div>
      <div class="muted">${s.rpcs} RPCs &middot; ${s.mapped} mapped</div>
      <div class="cards" style="margin-top:12px">
        ${miniStat("field", s.field_pct)}
        ${miniStat("branch present", s.present_pct)}
        ${miniStat("negative", s.negative_pct)}
        ${miniStat("status", s.status_pct)}
      </div>
    </a>`).join("");
  app.innerHTML = `<h1>Services</h1><p class="sub">Backend services analyzed for input/branch/status coverage.</p>${list}`;
}

function miniStat(label, pct) {
  const p = pctFloat(pct);
  return `<div class="card" style="padding:10px"><div class="label">${esc(label)}</div><div class="value" style="font-size:18px">${p.toFixed(1)}%</div><div class="bar ${barClass(p)}"><span style="width:${Math.min(p, 100)}%"></span></div></div>`;
}

let serviceState = { rows: [], svc: "", q: "", onlyMapped: true, sort: "rpc", dir: 1 };

async function viewService(svc) {
  setCrumbs([{ text: "Services", href: "#/" }, { text: svc }]);
  app.innerHTML = `<div class="loading">Loading ${esc(svc)}...</div>`;
  const data = await getJSON(`data/${encodeURIComponent(svc)}/index.json`);
  const t = data.totals;
  serviceState = { rows: data.rpcs, svc, q: "", onlyMapped: true, sort: "rpc", dir: 1 };

  const totals = `
    <div class="cards">
      ${card("RPCs", t.rpcs, `${t.mapped} mapped`)}
      ${card("Field cov", t.field_pct + "%", `${t.field_covered}/${t.field_total}`)}
      ${card("Branch present", t.present_pct + "%", `${t.present_arm}/${t.branch_total}`)}
      ${card("Branch negative", t.negative_pct + "%", `${t.negative_arm}/${t.branch_total}`)}
      ${card("Absent arm", t.absent_pct + "%", `${t.absent_arm}/${t.reachable_on_happy}`)}
      ${card("Status cov", t.status_pct + "%", `${t.status_asserted}/${t.status_documented}`)}
      ${card("Anomalies", t.anomalies, "field mismatches")}
    </div>`;

  app.innerHTML = `
    <h1>${esc(svc)}</h1>
    <p class="sub">${t.mapped} of ${t.rpcs} RPCs are exercised by the test suite. The rest are unmapped (internal or no matching operation).</p>
    ${totals}
    <h2>RPCs</h2>
    <div class="toolbar">
      <input id="q" type="search" placeholder="Filter RPCs..." />
      <label><input id="onlyMapped" type="checkbox" checked /> mapped only</label>
    </div>
    <div id="tbl"></div>`;

  document.getElementById("q").addEventListener("input", (e) => { serviceState.q = e.target.value.toLowerCase(); renderTable(); });
  document.getElementById("onlyMapped").addEventListener("change", (e) => { serviceState.onlyMapped = e.target.checked; renderTable(); });
  renderTable();
}

function renderTable() {
  const s = serviceState;
  let rows = s.rows.filter((r) => (!s.onlyMapped || !r.unmapped) && (r.rpc.toLowerCase().includes(s.q) || (r.operation || "").toLowerCase().includes(s.q)));
  rows.sort((a, b) => {
    let x = a[s.sort], y = b[s.sort];
    if (typeof x === "string") { x = x.toLowerCase(); y = (y || "").toLowerCase(); }
    return (x < y ? -1 : x > y ? 1 : 0) * s.dir;
  });
  const head = (key, label, num) => `<th class="${num ? "num" : ""}" data-sort="${key}">${esc(label)}${s.sort === key ? (s.dir > 0 ? " ▲" : " ▼") : ""}</th>`;
  const body = rows.map((r) => `
    <tr class="row" data-rpc="${esc(r.rpc)}">
      <td><a href="#/${encodeURIComponent(s.svc)}/${encodeURIComponent(r.rpc)}">${esc(r.rpc)}</a>${r.unmapped ? ' <span class="tag unmapped">unmapped</span>' : ""}</td>
      <td class="muted">${esc(r.operation)}</td>
      <td class="num">${r.tests}</td>
      <td class="num">${pctCell(r.field_pct)}</td>
      <td class="num">${pctCell(r.svc_present_pct)}</td>
      <td class="num">${pctCell(r.svc_negative_pct)}</td>
      <td class="num">${pctCell(r.status_pct)}</td>
      <td class="num">${r.anomaly_count || ""}</td>
    </tr>`).join("");
  document.getElementById("tbl").innerHTML = `
    <table>
      <thead><tr>
        ${head("rpc", "RPC")}${head("operation", "operation")}${head("tests", "tests", true)}
        ${head("field_pct", "field", true)}${head("svc_present_pct", "svc present", true)}
        ${head("svc_negative_pct", "svc negative", true)}${head("status_pct", "status", true)}${head("anomaly_count", "anom", true)}
      </tr></thead>
      <tbody>${body || `<tr><td colspan="8" class="muted">no matches</td></tr>`}</tbody>
    </table>
    <p class="muted" style="font-size:12px;margin-top:8px">${rows.length} shown</p>`;
  document.querySelectorAll("th[data-sort]").forEach((th) => th.addEventListener("click", () => {
    const key = th.dataset.sort;
    if (s.sort === key) s.dir *= -1; else { s.sort = key; s.dir = (key === "rpc" || key === "operation") ? 1 : -1; }
    renderTable();
  }));
  document.querySelectorAll("tr.row").forEach((tr) => tr.addEventListener("click", (e) => {
    if (e.target.tagName === "A") return;
    location.hash = `#/${encodeURIComponent(s.svc)}/${encodeURIComponent(tr.dataset.rpc)}`;
  }));
}

async function viewRpc(svc, rpc) {
  setCrumbs([{ text: "Services", href: "#/" }, { text: svc, href: `#/${encodeURIComponent(svc)}` }, { text: rpc }]);
  app.innerHTML = `<div class="loading">Loading ${esc(rpc)}...</div>`;
  const d = await getJSON(`data/${encodeURIComponent(svc)}/${encodeURIComponent(rpc)}.json`);
  const c = d.coverage, f = c.fields || {}, b = c.branches || {}, st = c.status || {};
  const depth = f.by_depth || {};

  const statusChips = (st.documented || []).map((code) => {
    const hit = (st.asserted || []).includes(code);
    return `<span class="chip ${hit ? "good" : "bad"}">${code}</span>`;
  }).join("") || '<span class="muted">none documented</span>';

  const depthRow = (name, band) => band ? `<div class="kv"><span class="k">${name}</span><span>${band.set}/${band.total} (${(band.pct || 0).toFixed(0)}%)</span></div>` : "";

  const fieldGaps = (f.gaps || []);
  const branchGaps = (b.gaps || []).map((x) => x.cond);
  const anomalies = (c.anomalies || {}).unmatched_test_fields || [];
  const errors = (d.provenance.errors || []).filter((e) => e.symbol).map((e) => e.symbol);
  const uniqErrors = [...new Set(errors)].sort();

  app.innerHTML = `
    <h1>${esc(rpc)} ${c.unmapped ? '<span class="tag unmapped">unmapped</span>' : ""}</h1>
    <p class="sub">operation <code>${esc(c.operation || "-")}</code> &middot; ${c.tests || 0} test(s) &middot; <code>${esc(d.provenance.func || "")}</code></p>

    <div class="metricgrid">
      <div class="panel">
        <h3>Input fields</h3>
        <div class="kv"><span class="k">covered</span><span>${f.covered_count || 0}/${f.leaf_fields_read_by_rpc || 0} (${(f.coverage_pct || 0).toFixed(1)}%)</span></div>
        <div class="kv"><span class="k">set by positive tests</span><span>${f.set_by_positive_tests || 0}</span></div>
        <div class="kv"><span class="k">set by negative tests</span><span>${f.set_by_negative_tests || 0}</span></div>
        ${depthRow("top-level", depth.top_level)}
        ${depthRow("nested", depth.nested)}
        ${depthRow("deep", depth.deep)}
      </div>
      <div class="panel">
        <h3>Branches (service logic emphasised)</h3>
        <div class="kv"><span class="k">total (all origins)</span><span>${b.total || 0}</span></div>
        <div class="kv"><span class="k">happy path reached</span><span>${b.happy_path_reached ? "yes" : "no"}</span></div>
        ${originRows(b.by_origin || {})}
        <div class="kv"><span class="k">absent arm (happy path)</span><span>${b.absent_arm_exercised || 0}/${b.reachable_on_happy_path || 0} (${(b.absent_arm_pct || 0).toFixed(1)}%)</span></div>
        <div class="kv"><span class="k">both arms</span><span>${b.both_arms_exercised || 0}</span></div>
      </div>
    </div>

    <h2>HTTP statuses</h2>
    <div class="panel"><div class="chips">${statusChips}</div>
      <p class="muted" style="font-size:12px;margin-top:10px">green = asserted by a test, red = documented but untested.</p></div>

    <h2>Gaps</h2>
    <div class="metricgrid">
      <div class="panel"><h3>Field gaps (${fieldGaps.length})</h3>
        ${fieldGaps.length ? `<ul class="gaps">${fieldGaps.map((g) => `<li>${esc(g)}</li>`).join("")}</ul>` : '<p class="muted">none</p>'}</div>
      <div class="panel"><h3>Branch gaps (${branchGaps.length})</h3>
        ${branchGaps.length ? `<ul class="gaps">${branchGaps.map((g) => `<li>${esc(g)}</li>`).join("")}</ul>` : '<p class="muted">none</p>'}</div>
    </div>

    ${anomalies.length ? `<h2>Anomalies</h2><div class="panel">${anomalies.map((a) => `<div class="kv"><span class="k">${esc(a.field)}</span><span>${esc((a.fuzzy_suggestions || []).join(", ") || "no suggestion")}</span></div>`).join("")}</div>` : ""}

    ${uniqErrors.length ? `<h2>Error symbols (${uniqErrors.length})</h2><div class="panel"><div class="chips">${uniqErrors.map((e) => `<span class="chip">${esc(e)}</span>`).join("")}</div></div>` : ""}

    <h2>Offload test generation to an agent</h2>
    <div class="panel prompt">
      <button class="copybtn" id="copyPrompt">Copy</button>
      <textarea id="promptText" readonly>${esc(d.agent_prompt)}</textarea>
    </div>

    <h2>Raw data</h2>
    <div class="panel">
      <details><summary>coverage JSON</summary><pre class="json">${esc(JSON.stringify(c, null, 2))}</pre></details>
      <details><summary>provenance (sinks / errors / unresolved)</summary><pre class="json">${esc(JSON.stringify(d.provenance, null, 2))}</pre></details>
    </div>`;

  const btn = document.getElementById("copyPrompt");
  btn.addEventListener("click", async () => {
    const text = document.getElementById("promptText").value;
    try { await navigator.clipboard.writeText(text); } catch (_) {
      const ta = document.getElementById("promptText"); ta.focus(); ta.select(); document.execCommand("copy");
    }
    btn.textContent = "Copied"; btn.classList.add("done");
    setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("done"); }, 1500);
  });
}

// ---- router ----
async function route() {
  const hash = location.hash.replace(/^#\/?/, "");
  const parts = hash.split("/").filter(Boolean).map(decodeURIComponent);
  try {
    if (parts.length === 0) await viewServices();
    else if (parts.length === 1) await viewService(parts[0]);
    else await viewRpc(parts[0], parts[1]);
  } catch (e) {
    app.innerHTML = `<div class="panel"><h3>Could not load</h3><p class="muted">${esc(e.message)}</p><p><a href="#/">Back to services</a></p></div>`;
  }
  window.scrollTo(0, 0);
}

window.addEventListener("hashchange", route);
route();
