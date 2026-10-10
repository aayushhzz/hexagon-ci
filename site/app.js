"use strict";

/* Lifejacket Coverage: static code analysis + test-coverage explorer.
   Vanilla JS, hash-routed, no runtime dependencies. All metrics are static
   reachability intersected with test inputs/assertions, not executed coverage. */

const app = document.getElementById("app");
const crumbs = document.getElementById("crumbs");

/* ----------------------------------------------------------------------------
   Safety helpers
   - esc(): escape every value interpolated into innerHTML.
   - attr(): escape for attribute context (adds quote/apostrophe).
   - All data (service names, RPC names, hash segments, JSON values) flows
     through these before touching the DOM.
---------------------------------------------------------------------------- */
function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => (
    { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
  ));
}
const attr = esc;

async function getJSON(path) {
  const res = await fetch(path, { cache: "no-cache" });
  if (!res.ok) throw new Error(`${res.status} loading ${path}`);
  return res.json();
}

/* ----------------------------------------------------------------------------
   Metric formatting with honest denominators.

   A percentage is only meaningful when the denominator is a real, known count.
   - mapped RPC, denominator > 0  -> real percentage.
   - mapped RPC, denominator == 0 -> "n/a" (the actual denominator is known to
     be zero: e.g. the RPC reads no leaf request fields, so there is nothing to
     cover; a 0% would be misleading).
   - unmapped RPC -> coverage is undefined, never a verified zero; shown as "Not measured".
---------------------------------------------------------------------------- */
function fmtPct(num, den, { unmapped = false } = {}) {
  if (unmapped) return { kind: "unknown", text: "Not measured", pct: null };
  if (!den || den <= 0) return { kind: "na", text: "n/a", pct: null };
  const pct = (100 * num) / den;
  return { kind: "value", text: pct.toFixed(1) + "%", pct };
}

function barClass(pct) {
  if (pct == null) return "na";
  if (pct >= 70) return "good";
  if (pct >= 30) return "warn";
  return "bad";
}

/* A percentage cell for tables: value + mini bar, or n/a / dash. */
function pctCell(res, { ratio = "" } = {}) {
  if (res.kind === "unknown") {
    return `<span class="pctcell"><span class="pv na" title="Not linked to any test, so this is not measured">Not measured</span></span>`;
  }
  if (res.kind === "na") {
    return `<span class="pctcell"><span class="pv na" title="${attr(ratio || "Nothing to measure here")}">n/a</span><span class="bar na" aria-hidden="true"></span></span>`;
  }
  const w = Math.max(0, Math.min(100, res.pct));
  return `<span class="pctcell"><span class="pv">${esc(res.text)}</span>`
    + `<span class="bar ${barClass(res.pct)}" aria-hidden="true"><span style="width:${w}%"></span></span></span>`;
}

/* Help tooltip: keyboard-focusable (tabindex) and tap-friendly (focus-within).
   Grounded directly in docs/running.md descriptions. */
function help(text) {
  return `<span class="help" tabindex="0" role="note" aria-label="${attr(text)}">i<span class="help-text">${esc(text)}</span></span>`;
}

const HELP = {
  field: "Input field coverage: of the request fields this endpoint's code actually reads, how many does at least one test fill with a real (non-empty) value. Matched by field name. A field is not rewarded for being used by more than one test, and this does not check the value is valid.",
  reached: "A test provides the value this if-condition looks at, so the condition is reached. This is reachability, not proof the branch ran, and providing a value is not the same as providing an invalid one.",
  errorPath: "A test sends an invalid value and expects an error response (a 4xx or 5xx), so the if-condition's error path is taken.",
  normalPath: "A passing test leaves the field out, so the if-condition is skipped and the normal path is taken.",
  branchBoth: "Branch coverage (also called decision coverage): both ways out of the if-condition are tested, the error path and the normal path.",
  status: "How many of the response codes this endpoint documents are actually checked by a test. The ones with no test (say an unchecked 403) are the gaps worth closing.",
  anomalies: "Request fields in the tests that did not line up with a field this endpoint reads. These are rename suggestions for a person to review. Nothing is changed automatically.",
  tests: "The Lifejacket tests that are linked to this endpoint.",
  depth: "The same input field coverage, split by how deeply nested each field is, so a few important top-level fields are not lost among large optional sub-sections.",
  mapped: "Linked means a test could be matched to this endpoint. Not linked means no test matched (the endpoint is internal or admin, or its name could not be worked out). A not-linked endpoint shows 'Not measured' instead of a fake zero, and does not mean no tests exist for it.",
};

function setCrumbs(parts) {
  crumbs.innerHTML = parts.map((p, i) => {
    const sep = i < parts.length - 1 ? '<span class="sep" aria-hidden="true">/</span>' : "";
    if (p.href) return `<a href="${attr(p.href)}">${esc(p.text)}</a>${sep}`;
    return `<span class="current" aria-current="page">${esc(p.text)}</span>${sep}`;
  }).join("");
}

function stateCard(title, msg, { back = true } = {}) {
  return `<div class="state"><h3>${esc(title)}</h3>`
    + `<p class="state-msg">${msg}</p>`
    + (back ? `<p class="state-actions"><a class="btn-back" href="#/">Back to services</a></p>` : "")
    + `</div>`;
}
function loadingCard(label) {
  return `<div class="state"><p class="state-msg skeleton">Loading ${esc(label)}</p></div>`;
}

/* ----------------------------------------------------------------------------
   Persisted service view state (search / filter / sort) across drilldown.
   Keyed per service so navigating RPC -> back restores the user's view.
---------------------------------------------------------------------------- */
const svcViews = Object.create(null);
function viewStateFor(svc) {
  if (!svcViews[svc]) {
    svcViews[svc] = { q: "", scope: "mapped", quick: new Set(), sort: "rpc", dir: 1 };
  }
  return svcViews[svc];
}

/* ----------------------------------------------------------------------------
   Stale async route guard. Each route() bumps the token; async work checks it
   so a slow fetch for a route the user already left never overwrites the view.
---------------------------------------------------------------------------- */
let routeToken = 0;

/* ============================ Services view ============================ */
async function viewServices(token) {
  setCrumbs([{ text: "Services" }]);
  app.innerHTML = loadingCard("services");
  const data = await getJSON("data/services.json");
  if (token !== routeToken) return;

  const services = (data.services || []).slice().sort((a, b) => a.service.localeCompare(b.service));
  const cards = services.map((s) => {
    // Service aggregates are precomputed percentages over mapped RPCs only.
    const mm = (label, pct, key) => {
      const b = barClass(pct);
      return `<div class="metric-mini">
        <div class="mm-top"><span class="mm-label">${esc(label)} ${help(HELP[key])}</span>
          <span class="mm-value">${pct.toFixed(1)}%</span></div>
        <div class="bar ${b}" aria-hidden="true"><span style="width:${Math.max(0, Math.min(100, pct))}%"></span></div>
      </div>`;
    };
    return `<a class="service-card" href="#/${encodeURIComponent(s.service)}">
      <div class="svc-name">${esc(s.service)}</div>
      <div class="svc-meta">${esc(s.rpcs)} RPCs (backend operations) &middot; ${esc(s.mapped)} test-linked &middot; ${esc(s.anomalies)} inputs missing from tests</div>
      <div class="service-metrics">
        ${mm("Input fields covered", s.field_pct, "field")}
        ${mm("Branches reached", s.present_pct, "reached")}
        ${mm("Error paths tested", s.negative_pct, "errorPath")}
        ${mm("Response codes checked", s.status_pct, "status")}
      </div>
    </a>`;
  }).join("");

  app.innerHTML = `
    <div class="page-head">
      <p class="eyebrow">Code and test comparison</p>
      <h1>Lifejacket coverage across services</h1>
      <p class="lede">For each backend RPC (a backend operation), this compares what the code reads and
      checks against the inputs and checks its linked Lifejacket tests supply. Pick a service to see its
      RPCs, where tests reach, and the gaps to close.</p>
    </div>
    <div class="callout accent">
      These numbers compare service code with Lifejacket test inputs and checks. They do not show results
      from a test run. Each service number counts only its test-linked RPCs. Not-linked internal RPCs are
      left out of the total rather than counted as zero.
    </div>
    <div class="service-grid">${cards || '<p class="empty-inline">No services found.</p>'}</div>
    <section class="methodology">
      <h2>How this is measured (in short)</h2>
      <ol class="method-steps">
        <li><strong>Read the code.</strong> We analyse each backend service's source and, for every RPC, follow the request object to see which input fields it reads and which checks (if-conditions) depend on them.</li>
        <li><strong>Read the tests.</strong> We parse the Lifejacket tests and, for each one, note the endpoint it calls, the request fields it fills with a real value, and the response codes it checks.</li>
        <li><strong>Link them.</strong> Each test endpoint is matched to the backend RPC that the gateway routes it to, so a test can be lined up with the code it exercises.</li>
        <li><strong>Compare.</strong> For a linked RPC we report how many of its input fields a test fills, how many of its checks a test reaches or pushes to failure, and how many documented response codes a test asserts.</li>
      </ol>
      <p class="method-note">This is a static comparison of code against test inputs and checks. It is not a test run, and it does not change any code. Where a request field or name does not line up, it is flagged for a person to review.</p>
    </section>`;
  bindHelp();
}

/* ============================ Service view ============================ */
async function viewService(svc, token) {
  setCrumbs([{ text: "Services", href: "#/" }, { text: svc }]);
  app.innerHTML = loadingCard(svc);
  const data = await getJSON(`data/${encodeURIComponent(svc)}/index.json`);
  if (token !== routeToken) return;

  const t = data.totals;
  const rows = data.rpcs || [];
  const vs = viewStateFor(svc);

  const stat = (label, res, sub, key) => {
    const valClass = res.kind === "value" ? "" : "na";
    return `<div class="stat">
      <div class="stat-label">${esc(label)} ${key ? help(HELP[key]) : ""}</div>
      <div class="stat-value ${valClass}">${esc(res.text)}</div>
      <div class="stat-sub">${esc(sub)}</div>
    </div>`;
  };

  const fieldRes = fmtPct(t.field_covered, t.field_total);
  const presentRes = fmtPct(t.present_arm, t.branch_total);
  const negRes = fmtPct(t.negative_arm, t.branch_total);
  const absentRes = fmtPct(t.absent_arm, t.reachable_on_happy);
  const statusRes = fmtPct(t.status_asserted, t.status_documented);

  app.innerHTML = `
    <div class="page-head">
      <p class="eyebrow">Service</p>
      <h1>${esc(svc)}</h1>
      <p class="lede">${esc(t.mapped)} of ${esc(t.rpcs)} RPCs (backend operations) are linked to a Lifejacket test.
      The remaining ${esc(t.unmapped)} are not linked (internal or admin RPCs, or operations whose name could
      not be derived) and are left out of the numbers below.</p>
    </div>

    <div class="callout">
      These numbers compare service code with Lifejacket test inputs and checks. They do not show results from
      a test run. The service totals count only its test-linked RPCs. A not-linked RPC has no linked tests and
      shows "Not measured" rather than a real zero; this does not mean no tests exist for it.
    </div>

    <div class="stat-strip">
      ${stat("RPCs", { kind: "value", text: String(t.rpcs) }, `${t.mapped} test-linked / ${t.unmapped} not linked`)}
      ${stat("Input fields covered", fieldRes, `${t.field_covered} / ${t.field_total} input fields`, "field")}
      ${stat("Branches reached", presentRes, `${t.present_arm} / ${t.branch_total} branches`, "reached")}
      ${stat("Error paths tested", negRes, `${t.negative_arm} / ${t.branch_total} branches`, "errorPath")}
      ${stat("Normal paths tested", absentRes, `${t.absent_arm} / ${t.reachable_on_happy} branches`, "normalPath")}
      ${stat("Response codes checked", statusRes, `${t.status_asserted} / ${t.status_documented} documented`, "status")}
      ${stat("Inputs missing from tests", { kind: "value", text: String(t.anomalies) }, "Request fields that did not line up with an input", "anomalies")}
    </div>

    <h2 class="flush">RPCs (backend operations)</h2>
    <p class="section-note">Search, filter, and sort. Your view stays the same while you open an RPC and come back.</p>

    <div class="toolbar" role="search">
      <div class="search">
        <label class="vh" for="q">Filter RPCs</label>
        <input id="q" type="search" placeholder="Search RPC or operation name..." autocomplete="off" />
      </div>
      <div class="filter-group" role="group" aria-label="Scope">
        <button class="seg" data-scope="mapped" type="button">Test-linked</button>
        <button class="seg" data-scope="unmapped" type="button">Not linked</button>
        <button class="seg" data-scope="all" type="button">All</button>
      </div>
      <div class="chip-filters" role="group" aria-label="Quick filters">
        <button class="toggle-chip" data-quick="fieldgaps" type="button" aria-pressed="false">Inputs missing from tests</button>
        <button class="toggle-chip" data-quick="neggaps" type="button" aria-pressed="false">No error tests</button>
        <button class="toggle-chip" data-quick="anomalies" type="button" aria-pressed="false">Needs mapping review</button>
      </div>
    </div>

    <div id="tbl" aria-live="polite"></div>`;

  // Restore persisted controls.
  const qInput = document.getElementById("q");
  qInput.value = vs.q;
  syncScopeButtons(svc);
  syncQuickButtons(svc);

  qInput.addEventListener("input", (e) => { vs.q = e.target.value; renderTable(svc, rows); });
  document.querySelectorAll(".seg[data-scope]").forEach((b) => b.addEventListener("click", () => {
    vs.scope = b.dataset.scope; syncScopeButtons(svc); renderTable(svc, rows);
  }));
  document.querySelectorAll(".toggle-chip[data-quick]").forEach((b) => b.addEventListener("click", () => {
    const k = b.dataset.quick;
    if (vs.quick.has(k)) vs.quick.delete(k); else vs.quick.add(k);
    syncQuickButtons(svc); renderTable(svc, rows);
  }));

  renderTable(svc, rows);
  bindHelp();
}

function syncScopeButtons(svc) {
  const vs = viewStateFor(svc);
  document.querySelectorAll(".seg[data-scope]").forEach((b) =>
    b.setAttribute("aria-pressed", String(b.dataset.scope === vs.scope)));
}
function syncQuickButtons(svc) {
  const vs = viewStateFor(svc);
  document.querySelectorAll(".toggle-chip[data-quick]").forEach((b) =>
    b.setAttribute("aria-pressed", String(vs.quick.has(b.dataset.quick))));
}

function renderTable(svc, allRows) {
  const vs = viewStateFor(svc);
  const q = vs.q.trim().toLowerCase();

  let rows = allRows.filter((r) => {
    if (vs.scope === "mapped" && r.unmapped) return false;
    if (vs.scope === "unmapped" && !r.unmapped) return false;
    if (q && !(r.rpc.toLowerCase().includes(q) || (r.operation || "").toLowerCase().includes(q))) return false;
    if (vs.quick.has("fieldgaps") && !(r.gap_count > 0)) return false;
    if (vs.quick.has("neggaps") && !(!r.unmapped && r.negative_pct === 0 && r.field_total > 0)) return false;
    if (vs.quick.has("anomalies") && !(r.anomaly_count > 0)) return false;
    return true;
  });

  const dir = vs.dir;
  rows.sort((a, b) => {
    let x = a[vs.sort], y = b[vs.sort];
    if (x == null) x = typeof y === "number" ? -1 : "";
    if (y == null) y = typeof x === "number" ? -1 : "";
    if (typeof x === "string") { x = x.toLowerCase(); y = String(y).toLowerCase(); }
    return (x < y ? -1 : x > y ? 1 : 0) * dir;
  });

  const sortBtn = (key, label, num) => {
    const active = vs.sort === key;
    const ariaSort = active ? (dir > 0 ? "ascending" : "descending") : "none";
    return `<th class="${num ? "num" : ""}" aria-sort="${ariaSort}" scope="col">
      <button class="sort-btn" data-sort="${attr(key)}" type="button">
        <span>${esc(label)}</span><span class="sort-ind" aria-hidden="true"></span>
      </button></th>`;
  };

  const body = rows.map((r) => {
    const unmapped = !!r.unmapped;
    const tag = unmapped
      ? '<span class="tag unmapped" title="No test could be linked to this operation">not linked</span>'
      : "";
    const ratio = (c, t) => `${c}/${t}`;
    const fieldRes = fmtPct(r.field_covered, r.field_total, { unmapped });
    // present/negative/status come pre-divided as percentages; treat as value unless unmapped.
    const pv = (pct) => unmapped ? { kind: "unknown", text: "Not measured", pct: null } : { kind: "value", text: pct.toFixed(1) + "%", pct };
    const href = `#/${encodeURIComponent(svc)}/${encodeURIComponent(r.rpc)}`;
    return `<tr>
      <td><a class="rpc-link" href="${attr(href)}">${esc(r.rpc)}</a> ${tag}</td>
      <td>${r.operation ? `<span class="op">${esc(r.operation)}</span>` : '<span class="empty-inline">No operation link</span>'}</td>
      <td class="num">${esc(r.tests)}</td>
      <td class="num">${pctCell(fieldRes, { ratio: ratio(r.field_covered, r.field_total) })}</td>
      <td class="num">${pctCell(pv(r.present_pct))}</td>
      <td class="num">${pctCell(pv(r.negative_pct))}</td>
      <td class="num">${pctCell(unmapped && r.status_pct === 0 ? { kind: "unknown", text: "Not measured" } : pv(r.status_pct))}</td>
      <td class="num">${r.anomaly_count ? esc(r.anomaly_count) : '<span class="muted">0</span>'}</td>
    </tr>`;
  }).join("");

  const empty = `<tr><td colspan="8"><p class="empty-inline" style="text-align:center;padding:20px">No RPCs match the current search and filters.</p></td></tr>`;

  document.getElementById("tbl").innerHTML = `
    <div class="table-wrap">
    <table>
      <thead><tr>
        ${sortBtn("rpc", "RPC")}
        ${sortBtn("operation", "Operation")}
        ${sortBtn("tests", "Tests", true)}
        ${sortBtn("field_pct", "Input fields covered", true)}
        ${sortBtn("present_pct", "Branches reached", true)}
        ${sortBtn("negative_pct", "Error paths tested", true)}
        ${sortBtn("status_pct", "Response codes", true)}
        ${sortBtn("anomaly_count", "Missing inputs", true)}
      </tr></thead>
      <tbody>${body || empty}</tbody>
    </table>
    </div>
    <p class="rows-note">${rows.length} RPC${rows.length === 1 ? "" : "s"} shown. The columns apply to test-linked RPCs only.
    Not-linked rows show <code>Not measured</code> because there is nothing to measure, which is not the same as zero.</p>`;

  document.querySelectorAll(".sort-btn[data-sort]").forEach((btn) => btn.addEventListener("click", () => {
    const key = btn.dataset.sort;
    if (vs.sort === key) vs.dir *= -1;
    else { vs.sort = key; vs.dir = (key === "rpc" || key === "operation") ? 1 : -1; }
    renderTable(svc, allRows);
  }));
}

/* ============================ RPC detail view ============================ */
async function viewRpc(svc, rpc, token) {
  setCrumbs([
    { text: "Services", href: "#/" },
    { text: svc, href: `#/${encodeURIComponent(svc)}` },
    { text: rpc },
  ]);
  app.innerHTML = loadingCard(rpc);
  const d = await getJSON(`data/${encodeURIComponent(svc)}/${encodeURIComponent(rpc)}.json`);
  if (token !== routeToken) return;

  const c = d.coverage || {};
  const prov = d.provenance || {};
  const f = c.fields || {};
  const b = c.branches || {};
  const st = c.status || {};
  const depth = f.by_depth || {};
  const unmapped = !!c.unmapped;

  const leaf = f.leaf_fields_read_by_rpc || 0;
  const covered = f.covered_count || 0;
  const fieldRes = fmtPct(covered, leaf, { unmapped });

  // ---- sections (for jump nav + counts) ----
  const fieldGaps = f.gaps || [];
  const branchGaps = b.gaps || [];
  const coveredFields = f.covered || [];
  const anomalies = (c.anomalies || {}).unmatched_test_fields || [];
  const testFacts = c.test_facts || [];
  const sinks = prov.sinks || [];
  const errorsRaw = prov.errors || [];
  const unresolved = prov.unresolved || [];
  const documented = st.documented || [];

  // ---- header + mapping note ----
  const mapTag = unmapped
    ? '<span class="tag unmapped">not linked</span>'
    : '<span class="tag mapped">test-linked</span>';

  const unmappedNote = unmapped ? `
    <div class="callout">
      No test could be linked to this RPC (it is an internal or admin RPC, or its operation name could not be
      derived). The information below is read from the code, but there are no linked tests to compare against,
      so test coverage cannot be measured here. This does not mean no tests exist for it. Read the input and check counts as
      the surface that would need tests, not as a real zero.
    </div>` : "";

  // nothing-to-measure note for test-linked RPCs that read no input fields
  const zeroFieldNote = (!unmapped && leaf === 0) ? `
    <div class="callout">
      This RPC reads no input fields directly, so there is nothing to cover and "Input fields covered" shows n/a
      rather than 0%. The check and response-code sections below still apply.
    </div>` : "";

  // ---- input field coverage: depth breakdown ----
  const depthRow = (name, band, key) => {
    if (!band || !band.total) return "";
    const res = fmtPct(band.set, band.total, { unmapped });
    return `<div class="kv depth-row">
      <span class="k">${esc(name)}</span>
      <span class="v ${res.kind !== "value" ? "na" : ""}">${esc(res.text)} <small>${band.set}/${band.total}</small></span>
    </div>`;
  };

  const depthLegend = {
    top_level: "top-level (in.X)",
    nested: "nested (in.X.Y)",
    deep: "deep (in.X.Y.Z…)",
  };

  const fieldPanel = `
    <div class="panel">
      <h3>Input field coverage ${help(HELP.field)}</h3>
      <p class="panel-sub">Does a test fill each input the code reads? (the data)</p>
      <div class="kv-list">
        <div class="kv"><span class="k">Input fields read by the code</span><span class="v">${leaf}</span></div>
        <div class="kv"><span class="k">Covered (filled by a test)</span>
          <span class="v ${fieldRes.kind !== "value" ? "na" : ""}">${esc(fieldRes.text)} <small>${covered}/${leaf}</small></span></div>
        <div class="kv"><span class="k">Filled by positive tests</span><span class="v">${f.set_by_positive_tests || 0}</span></div>
        <div class="kv"><span class="k">Filled by negative tests</span><span class="v">${f.set_by_negative_tests || 0}</span></div>
        <div class="kv"><span class="k">By nesting depth ${help(HELP.depth)}</span><span class="v"></span></div>
        ${depthRow(depthLegend.top_level, depth.top_level)}
        ${depthRow(depthLegend.nested, depth.nested)}
        ${depthRow(depthLegend.deep, depth.deep)}
      </div>
    </div>`;

  const branchPanel = `
    <div class="panel">
      <h3>Branch coverage</h3>
      <p class="panel-sub">Does a test exercise each if-condition that reads an input? (the logic)</p>
      <div class="kv-list">
        <div class="kv"><span class="k">Input-driven branches (if-conditions)</span><span class="v">${b.total || 0}</span></div>
        <div class="kv"><span class="k">Reached by a passing request</span><span class="v">${b.happy_path_reached ? "yes" : "no"}</span></div>
        <div class="kv"><span class="k">Reached: a test fills the input ${help(HELP.reached)}</span>
          <span class="v">${esc(fmtPct(b.present_arm_exercised || 0, b.total || 0, { unmapped }).text)} <small>${b.present_arm_exercised || 0}/${b.total || 0}</small></span></div>
        <div class="kv"><span class="k">Error path tested ${help(HELP.errorPath)}</span>
          <span class="v">${esc(fmtPct(b.negative_arm_exercised || 0, b.total || 0, { unmapped }).text)} <small>${b.negative_arm_exercised || 0}/${b.total || 0}</small></span></div>
        <div class="kv"><span class="k">Normal path tested ${help(HELP.normalPath)}</span>
          <span class="v">${esc(fmtPct(b.absent_arm_exercised || 0, b.reachable_on_happy_path || 0, { unmapped }).text)} <small>${b.absent_arm_exercised || 0}/${b.reachable_on_happy_path || 0}</small></span></div>
        <div class="kv"><span class="k">Both paths tested = branch coverage ${help(HELP.branchBoth)}</span>
          <span class="v">${esc(fmtPct(b.both_arms_exercised || 0, b.total || 0, { unmapped }).text)} <small>${b.both_arms_exercised || 0}/${b.total || 0}</small></span></div>
      </div>
    </div>`;

  // ---- HTTP statuses: explicit asserted / missing (not just colour) ----
  const asserted = new Set(st.asserted || []);
  const statusRows = documented.length ? documented.map((code) => {
    const hit = asserted.has(code);
    return `<tr>
      <td class="code">${esc(code)}</td>
      <td><span class="status-state ${hit ? "asserted" : "missing"}">
        <span class="dot" aria-hidden="true"></span>${hit ? "Checked by a test" : "Documented, not checked"}
      </span></td>
    </tr>`;
  }).join("") : "";
  const statusSection = `
    <h2 id="s-status">HTTP response codes ${help(HELP.status)}</h2>
    <p class="section-note">The documented response codes come from the generated client spec. Each row says
    whether a test checks for it. The "not checked" rows (for example an unchecked 403) are the gaps to close.</p>
    ${documented.length ? `<div class="panel" style="padding:0">
      <table class="status-table">
        <thead><tr><th scope="col">Response code</th><th scope="col">Checked by a test</th></tr></thead>
        <tbody>${statusRows}</tbody>
      </table></div>`
      : `<div class="panel"><p class="empty-inline">No HTTP response codes are documented for this operation${unmapped ? " (not linked to any test)." : "."}</p></div>`}`;

  // ---- covered field evidence ----
  const coveredSection = `
    <h2 id="s-covered">Covered inputs and evidence</h2>
    <p class="section-note">Input fields that at least one linked test fills in, with the tests that fill them.
    <code>alias</code> means the match came through a reviewed HTTP-to-RPC field-map entry rather than an exact name.</p>
    <div class="panel" style="padding:0">
      ${coveredFields.length ? coveredFields.map((cf) => `
        <div class="evidence-item">
          <div class="ev-path">${esc(cf.path)}</div>
          <div class="ev-meta">match: <span class="badge ${cf.match === "alias" ? "match-alias" : ""}">${esc(cf.match || "exact")}</span></div>
          <div class="ev-tests">${(cf.by_tests || []).map((tn) => `<span class="badge">${esc(tn)}</span>`).join("") || '<span class="empty-inline">no named test</span>'}</div>
        </div>`).join("")
        : `<div class="evidence-item"><p class="empty-inline">No covered input fields${unmapped ? " (not linked to any test)." : "."}</p></div>`}
    </div>`;

  // ---- field gaps ----
  const fieldGapSection = `
    <h2 id="s-fieldgaps">Inputs missing from tests <span class="muted">(${fieldGaps.length})</span></h2>
    <p class="section-note">Input fields the RPC reads but no linked test fills in. This is the list to add tests for.
    Paths start at <code>in</code> (the RPC request); dots follow nested fields and <code>[*]</code> means any repeated element.
    These are paths read from the code, not descriptions of what the field means. Check the related code before writing a test.</p>
    ${fieldGaps.length ? `<ul class="gap-list scroll">${fieldGaps.map((g) => `
      <li class="gap-item"><span class="gap-cond">${esc(g)}</span></li>`).join("")}</ul>`
      : `<div class="panel"><p class="empty-inline">No inputs are missing from tests.</p></div>`}`;

  // ---- branch gaps with source location + predicate paths ----
  const branchGapSection = `
    <h2 id="s-branchgaps">Branches no test reaches <span class="muted">(${branchGaps.length})</span></h2>
    <p class="section-note">Input-driven if-conditions where no linked test fills the input they read.
    Each shows the condition, its source location, and the request paths it reads. Add a test that fills those paths.
    Check the code first to decide on a valid or invalid value and the response you expect. Filling an input is not the same as supplying an invalid one.</p>
    ${branchGaps.length ? `<ul class="gap-list scroll">${branchGaps.map((g) => `
      <li class="gap-item">
        <span class="gap-cond">${esc(g.cond || "(condition unavailable)")}</span>
        ${g.at ? `<div class="gap-at">${esc(g.at)}</div>` : ""}
        ${(g.predicate_paths && g.predicate_paths.length) ? `<div class="gap-paths">reads ${g.predicate_paths.map((p) => `<code>${esc(p)}</code>`).join(" ")}</div>` : ""}
      </li>`).join("")}</ul>`
      : `<div class="panel"><p class="empty-inline">No checks are missing test inputs.</p></div>`}`;

  // ---- sinks (where request data goes) ----
  const SINK_LABELS = {
    db_read: "database read",
    db_write: "database write",
    rpc: "service call",
    interface: "interface call",
    dispatch: "interface call",
    opaque: "analysis stops here",
  };
  const sinkLabel = (k) => SINK_LABELS[k] || (k || "destination");
  const sinkSection = `
    <h2 id="s-sinks">Where request data goes <span class="muted">(${sinks.length})</span></h2>
    <p class="section-note">Where the request data ends up: a database read or write, a call to another service,
    an interface call, or a point where the analysis stops. Each one lists the request paths that flow into it.</p>
    ${sinks.length ? `<div class="panel" style="padding:0">${sinks.map((s) => `
      <div class="sink-item">
        <div class="sink-head">
          <span class="sink-kind ${esc((s.kind || "").replace(/[^a-z_]/gi, ""))}">${esc(sinkLabel(s.kind))}</span>
          <span class="sink-target">${esc(s.target || "(unnamed)")}</span>
        </div>
        ${s.at ? `<div class="sink-at">${esc(s.at)}</div>` : ""}
        ${(s.via_paths && s.via_paths.length) ? `<div class="sink-paths">via ${s.via_paths.slice(0, 24).map((p) => `<code>${esc(p)}</code>`).join(" ")}${s.via_paths.length > 24 ? ` <span class="muted">+${s.via_paths.length - 24} more</span>` : ""}</div>` : ""}
      </div>`).join("")}</div>`
      : `<div class="panel"><p class="empty-inline">No destinations recorded for this RPC.</p></div>`}`;

  // ---- error symbols ----
  const errSection = (() => {
    if (!errorsRaw.length) return "";
    const tainted = errorsRaw.filter((e) => e.symbol && e.tainted);
    const untainted = errorsRaw.filter((e) => e.symbol && !e.tainted);
    const chipsOf = (arr, cls) => [...new Set(arr.map((e) => e.symbol))].sort()
      .map((sym) => `<span class="chip ${cls}">${esc(sym)}</span>`).join("");
    return `
      <h2 id="s-errors">Error exits <span class="muted">(${new Set(errorsRaw.filter(e=>e.symbol).map(e=>e.symbol)).size})</span></h2>
      <p class="section-note">Errors this RPC can return. The first group is linked to the request inputs and is
      a good starting point for error tests. The rest are reachable but are not driven directly by request inputs.</p>
      <div class="panel stack">
        <div><div class="ev-meta" style="margin-bottom:8px">Errors linked to request inputs (${new Set(tainted.map(e=>e.symbol)).size})</div>
          <div class="chips">${chipsOf(tainted, "tainted") || '<span class="empty-inline">none</span>'}</div></div>
        <div><div class="ev-meta" style="margin:8px 0">Other reachable errors (${new Set(untainted.map(e=>e.symbol)).size})</div>
          <div class="chips">${chipsOf(untainted, "untainted") || '<span class="empty-inline">none</span>'}</div></div>
      </div>`;
  })();

  // ---- anomalies (human-reviewed mapping suggestions) ----
  const anomalySection = anomalies.length ? `
    <h2 id="s-anomalies">Mapping review <span class="muted">(${anomalies.length})</span> ${help(HELP.anomalies)}</h2>
    <p class="section-note">HTTP request fields that did not line up one-to-one with an RPC input field. These are
    suggestions for a person to confirm in <code>fieldmap.json</code>. Nothing is renamed automatically.
    A field with no suggestion may simply be ignored by the RPC.</p>
    <div class="panel" style="padding:0">
      ${anomalies.map((a) => `
        <div class="anomaly-item">
          <div class="anomaly-field">${esc(a.field)}</div>
          <div class="anomaly-sugg">${(a.fuzzy_suggestions && a.fuzzy_suggestions.length)
            ? "possible RPC paths: " + a.fuzzy_suggestions.map((s) => `<code>${esc(s)}</code>`).join("")
            : '<span class="anomaly-none">No suggestion available. Check the service code before adding a mapping.</span>'}</div>
        </div>`).join("")}
    </div>` : "";

  // ---- test facts (expandable) ----
  const testFactSection = `
    <h2 id="s-tests">Linked tests <span class="muted">(${testFacts.length})</span> ${help(HELP.tests)}</h2>
    <p class="section-note">The Lifejacket tests linked to this RPC, with the inputs each one fills in, the
    response codes it checks for, and whether it expects success.</p>
    ${testFacts.length ? testFacts.map((tf) => {
      const pos = tf.asserts_success;
      const fieldsSet = tf.fields_set || [];
      const statuses = tf.statuses || [];
      const neg = statuses.some((code) => Number(code) >= 400 && Number(code) < 600);
      const assertionLabel = pos ? "expects success" : neg ? "expects an error response" : "no outcome found";
      const labels = tf.labels || [];
      return `<details class="testfact">
        <summary>
          <span class="tf-name">${esc(tf.name || "(unnamed test)")}</span>
          <span class="tf-assert"><span class="pill ${pos ? "pos" : neg ? "neg" : ""}">${assertionLabel}</span></span>
        </summary>
        <div class="tf-body stack">
          <div class="ev-meta mono" style="word-break:break-all">${esc(tf.file || "")}</div>
          ${labels.length ? `<div class="tf-labels">${labels.map((l) => `<span class="tf-label">${esc(l)}</span>`).join("")}</div>` : ""}
          <div><span class="muted">inputs filled in:</span> ${fieldsSet.length ? fieldsSet.map((x) => `<code>${esc(x)}</code>`).join(" ") : '<span class="empty-inline">none (for example a test that sends an empty request)</span>'}</div>
          <div><span class="muted">response codes checked:</span> ${statuses.length ? statuses.map((s) => `<span class="chip">${esc(s)}</span>`).join(" ") : '<span class="empty-inline">none stated</span>'}</div>
        </div>
      </details>`;
    }).join("")
      : `<div class="panel"><p class="empty-inline">No linked tests${unmapped ? " (not linked to any test)." : "."}</p></div>`}`;

  // ---- agent prompt ----
  const promptSection = `
    <h2 id="s-prompt">Hand off to a test-writing agent</h2>
    <p class="section-note">A ready-to-paste brief listing the gaps above. Copying it does not change anything.
    It is the same prompt stored with this RPC.</p>
    <div class="prompt-wrap">
      <div class="prompt-bar">
        <span class="pb-label">Agent prompt</span>
        <button class="copybtn" id="copyPrompt" type="button">Copy prompt</button>
      </div>
      <label class="vh" for="promptText">Agent prompt text</label>
      <textarea id="promptText" readonly spellcheck="false">${esc(d.agent_prompt || "")}</textarea>
      <p class="copy-hint" id="copyHint">The prompt describes gaps to close; it does not run or modify tests.</p>
    </div>`;

  // ---- raw data fallback ----
  const rawSection = `
    <h2 id="s-raw">Raw data</h2>
    <p class="section-note">The underlying JSON, in case a section above does not show what you need.</p>
    <details class="raw"><summary>Coverage JSON</summary><pre class="json">${esc(JSON.stringify(c, null, 2))}</pre></details>
    <details class="raw"><summary>Code analysis JSON</summary><pre class="json">${esc(JSON.stringify(prov, null, 2))}</pre></details>`;

  // ---- unresolved note ----
  const unresolvedNote = unresolved.length ? `
    <div class="callout">
      ${unresolved.length} point${unresolved.length === 1 ? "" : "s"} where the analysis could not follow the
      request data any further (for example a call with several possible targets, or a called function the
      analysis could not read). At these points it stops rather than dropping the data. See the provenance JSON below.
    </div>` : "";

  // ---- jump nav ----
  const jnItems = [
    ["s-overview", "Coverage", null],
    ["s-status", "Response codes", documented.length],
    ["s-covered", "Covered inputs", coveredFields.length],
    ["s-fieldgaps", "Inputs missing from tests", fieldGaps.length],
    ["s-branchgaps", "Branches no test reaches", branchGaps.length],
    ["s-sinks", "Where request data goes", sinks.length],
    errorsRaw.length ? ["s-errors", "Error exits", new Set(errorsRaw.filter(e=>e.symbol).map(e=>e.symbol)).size] : null,
    anomalies.length ? ["s-anomalies", "Mapping review", anomalies.length] : null,
    ["s-tests", "Linked tests", testFacts.length],
    ["s-prompt", "Agent handoff", null],
    ["s-raw", "Raw data", null],
  ].filter(Boolean);

  const jumpNav = `<nav class="jumpnav" aria-label="On this page"><h3>On this page</h3>
    <ol>${jnItems.map(([id, label, count]) => `<li><a href="#${id}" data-jump="${attr(id)}">
      <span>${esc(label)}</span>${count != null ? `<span class="jn-count">${esc(count)}</span>` : ""}</a></li>`).join("")}</ol></nav>`;

  app.innerHTML = `
    <div class="page-head">
      <p class="eyebrow">${esc(svc)} &middot; RPC (backend operation)</p>
      <h1>${esc(rpc)} ${mapTag}</h1>
      <p class="lede">
        ${c.operation ? `operation <code>${esc(c.operation)}</code> &middot; ` : ""}
        ${esc(c.tests || 0)} linked test${(c.tests || 0) === 1 ? "" : "s"}
        ${prov.func ? `<br><span class="rpc-type">${esc(prov.func)}</span>` : ""}
        ${prov.request_type ? `<br><span class="rpc-type">${esc(prov.request_type)} → ${esc(prov.response_type || "?")}</span>` : ""}
      </p>
    </div>

    ${unmappedNote}${zeroFieldNote}${unresolvedNote}

    <div class="detail-layout">
      ${jumpNav}
      <div class="detail-main">
        <h2 id="s-overview" class="flush">Coverage</h2>
        <p class="section-note">Two kinds of coverage, both comparing this operation's code against its linked tests.
        <strong>Input field coverage</strong> asks whether tests fill the inputs the code reads (the data).
        <strong>Branch coverage</strong> asks whether tests exercise the if-conditions that read those inputs (the logic).
        Filling an input suggests a branch can be reached; it does not confirm the branch ran or passed.</p>
        <div class="panel-grid">${fieldPanel}${branchPanel}</div>
        ${statusSection}
        ${coveredSection}
        ${fieldGapSection}
        ${branchGapSection}
        ${sinkSection}
        ${errSection}
        ${anomalySection}
        ${testFactSection}
        ${promptSection}
        ${rawSection}
      </div>
    </div>`;

  bindHelp();
  bindCopy();
  bindJump();
}

/* ---- clipboard with explicit failure handling ---- */
function bindCopy() {
  const btn = document.getElementById("copyPrompt");
  const ta = document.getElementById("promptText");
  const hint = document.getElementById("copyHint");
  if (!btn || !ta) return;
  const reset = () => {
    setTimeout(() => {
      btn.textContent = "Copy prompt";
      btn.classList.remove("done", "failed");
    }, 1800);
  };
  btn.addEventListener("click", async () => {
    const text = ta.value;
    let ok = false;
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text);
        ok = true;
      }
    } catch (_) { ok = false; }
    if (!ok) {
      // Fallback: select the textarea and try execCommand.
      try {
        ta.focus(); ta.select();
        ok = document.execCommand && document.execCommand("copy");
      } catch (_) { ok = false; }
    }
    if (ok) {
      btn.textContent = "Copied";
      btn.classList.add("done");
      if (hint) hint.textContent = "Copied to clipboard.";
    } else {
      btn.textContent = "Copy failed";
      btn.classList.add("failed");
      ta.focus(); ta.select();
      if (hint) hint.textContent = "Could not copy automatically. The text is selected, so press Ctrl/Cmd+C.";
    }
    reset();
  });
}

/* ---- smooth-ish in-page jump that also moves focus for a11y ---- */
function bindJump() {
  document.querySelectorAll("a[data-jump]").forEach((a) => a.addEventListener("click", (e) => {
    const id = a.dataset.jump;
    const target = document.getElementById(id);
    if (!target) return;
    e.preventDefault();
    target.scrollIntoView({ behavior: "smooth", block: "start" });
    target.setAttribute("tabindex", "-1");
    target.focus({ preventScroll: true });
    history.replaceState(null, "", location.hash.split("#").slice(0, 2).join("#") + "#" + id);
  }));
}

/* ---- help tooltips: ensure focus-within works on touch (toggle on click) ---- */
function bindHelp() {
  document.querySelectorAll(".help").forEach((h) => {
    h.addEventListener("click", (e) => { e.preventDefault(); e.stopPropagation(); h.focus(); });
    h.addEventListener("keydown", (e) => { if (e.key === "Escape") h.blur(); });
  });
}

/* ============================ Router ============================ */
function parseHash() {
  // Strip a trailing in-page anchor (#s-xxx) appended by jump links.
  let raw = location.hash.replace(/^#\/?/, "");
  const extra = raw.indexOf("#");
  if (extra !== -1) raw = raw.slice(0, extra);
  try {
    return raw.split("/").filter(Boolean).map((seg) => {
      try { return decodeURIComponent(seg); } catch (_) { return seg; }
    });
  } catch (_) {
    return null; // malformed
  }
}

async function route() {
  const token = ++routeToken;
  const parts = parseHash();

  if (parts === null) {
    app.innerHTML = stateCard("That link looks malformed",
      "The address could not be read. Start from the services list.");
    return;
  }

  try {
    if (parts.length === 0) await viewServices(token);
    else if (parts.length === 1) await viewService(parts[0], token);
    else await viewRpc(parts[0], parts[1], token);
  } catch (e) {
    if (token !== routeToken) return; // superseded by a newer navigation
    const msg = String((e && e.message) || e);
    const notFound = /\b404\b/.test(msg);
    app.innerHTML = stateCard(
      notFound ? "Not found" : "Could not load",
      notFound
        ? "No data exists for this path. It may be an unknown service or RPC name."
        : `Something went wrong while loading this view.<br><code>${esc(msg)}</code>`
    );
  }
  if (token === routeToken) {
    app.focus({ preventScroll: true });
    window.scrollTo(0, 0);
  }
}

window.addEventListener("hashchange", route);
route();
