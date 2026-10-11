(() => {
  "use strict";

  const $ = (selector) => document.querySelector(selector);
  const canvas = $("#graph-canvas");
  const context = canvas.getContext("2d");
  const state = {
    datasets: [], active: null, graph: null, visibleNodes: [], visibleEdges: [],
    unresolved: [], positions: new Map(), nodeByID: new Map(), fileByPath: new Map(),
    functionLabelByID: new Map(), functionNodes: [], functionSearchEntries: [], functionSuggestions: [],
    focusTestCache: new Map(),
    unresolvedByCaller: new Map(), focusTestIDs: [],
    focusedID: "", selectedID: "", hoveredID: "", search: "", showTests: true,
    showAllFocusTests: false,
    activeFunctionSuggestion: -1,
    showFunctions: true, width: 0, height: 0, pixelRatio: 1,
    view: { x: 0, y: 0, zoom: 1 }, layoutWidth: 900, testGap: 112, pointer: null, moved: false, toastTimer: null,
    loadSequence: 0, loadController: null,
  };

  const COLORS = ["#8e83ff", "#45d7c1", "#ff9f78", "#73a8ff", "#e6bf6b", "#e98dc0"];
  const FOCUS_TEST_PREVIEW_LIMIT = 24;
  const esc = (value) => String(value ?? "").replace(/[&<>"']/g, (character) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[character]);
  const shortName = (node) => node?.qualname || node?.id?.split("::").pop() || "Unknown function";
  const humanNumber = (value) => new Intl.NumberFormat().format(value || 0);

  function normalizeGraph(raw) {
    if (!raw || raw.format_version !== 2) throw new Error("This file is not a supported graph-v2 snapshot.");
    if (!Array.isArray(raw.nodes) || !Array.isArray(raw.edges)) throw new Error("The graph is missing its nodes or edges.");
    const nodes = raw.nodes.filter((node) => node && typeof node.id === "string");
    const nodeIDs = new Set(nodes.map((node) => node.id));
    const edges = raw.edges.filter((edge) => edge && nodeIDs.has(edge.caller) && nodeIDs.has(edge.callee));
    return {
      ...raw,
      nodes,
      edges,
      tests: raw.tests && typeof raw.tests === "object" ? raw.tests : {},
      unresolved_calls: Array.isArray(raw.unresolved_calls) ? raw.unresolved_calls : [],
      files: Array.isArray(raw.files) ? raw.files : [],
    };
  }

  function hash(value) {
    let result = 2166136261;
    for (let index = 0; index < value.length; index += 1) {
      result ^= value.charCodeAt(index);
      result = Math.imul(result, 16777619);
    }
    return result >>> 0;
  }

  function layeredLayout(nodes, edges) {
    const points = new Map();
    if (!nodes.length) return points;
    const outgoing = new Map(nodes.map((node) => [node.id, []]));
    const incoming = new Map(nodes.map((node) => [node.id, []]));
    for (const edge of edges) {
      if (!outgoing.has(edge.caller) || !incoming.has(edge.callee)) continue;
      outgoing.get(edge.caller).push(edge.callee);
      incoming.get(edge.callee).push(edge.caller);
    }

    const depth = new Map();
    let deepest = 0;
    const traverse = (seeds, startDepth) => {
      const queue = [];
      for (const node of seeds) {
        if (depth.has(node.id)) continue;
        depth.set(node.id, startDepth);
        queue.push(node.id);
      }
      for (let index = 0; index < queue.length; index += 1) {
        const caller = queue[index];
        const nextDepth = depth.get(caller) + 1;
        for (const callee of outgoing.get(caller) || []) {
          if (depth.has(callee)) continue;
          depth.set(callee, nextDepth);
          deepest = Math.max(deepest, nextDepth);
          queue.push(callee);
        }
      }
    };

    const testRoots = nodes.filter((node) => node.is_test)
      .sort((a, b) => shortName(a).localeCompare(shortName(b)) || a.id.localeCompare(b.id));
    traverse(testRoots, 0);
    let remaining = nodes.filter((node) => !depth.has(node.id));
    while (remaining.length) {
      const remainingIDs = new Set(remaining.map((node) => node.id));
      const roots = remaining.filter((node) => !(incoming.get(node.id) || []).some((caller) => remainingIDs.has(caller)));
      traverse(roots.length ? roots : [remaining[0]], deepest + 1);
      remaining = nodes.filter((node) => !depth.has(node.id));
    }

    const layers = new Map();
    for (const node of nodes) {
      const level = depth.get(node.id) || 0;
      if (!layers.has(level)) layers.set(level, []);
      layers.get(level).push(node);
    }
    const largestLayer = Math.max(...[...layers.values()].map((layer) => layer.length));
    const layoutWidth = Math.max(900, Math.min(3600, Math.max(1, largestLayer - 1) * 7));
    state.layoutWidth = layoutWidth;
    state.testGap = layoutWidth / Math.max(1, testRoots.length - 1);
    for (const [level, layer] of [...layers.entries()].sort((a, b) => a[0] - b[0])) {
      if (level === 0) {
        layer.sort((a, b) => shortName(a).localeCompare(shortName(b)) || a.id.localeCompare(b.id));
      } else {
        const barycenter = (node) => {
          const parents = (incoming.get(node.id) || []).map((id) => points.get(id)).filter(Boolean);
          return parents.length ? parents.reduce((sum, point) => sum + point.x, 0) / parents.length : null;
        };
        const scores = new Map(layer.map((node) => [node.id, barycenter(node)]));
        layer.sort((a, b) => {
          const left = scores.get(a.id), right = scores.get(b.id);
          if (left !== null && right !== null && left !== right) return left - right;
          if (left === null && right !== null) return 1;
          if (left !== null && right === null) return -1;
          return a.id.localeCompare(b.id);
        });
      }
      layer.forEach((node, index) => {
        points.set(node.id, {
          x: layer.length === 1 ? 0 : (index / (layer.length - 1) - 0.5) * layoutWidth,
          y: level * 210,
        });
      });
    }
    return points;
  }

  function functionCenteredLayout(nodes, edges, focusID) {
    const points = new Map();
    const nodeByID = new Map(nodes.map((node) => [node.id, node]));
    const focus = nodes.find((node) => node.id === focusID);
    if (!focus) return layeredLayout(nodes, edges);
    const tests = nodes.filter((node) => node.is_test)
      .sort((a, b) => shortName(a).localeCompare(shortName(b)) || a.id.localeCompare(b.id));
    const callers = [...new Set(edges.filter((edge) => edge.callee === focusID && edge.type !== "test-coverage").map((edge) => edge.caller))]
      .map((id) => nodeByID.get(id)).filter((node) => node && !node.is_test)
      .sort((a, b) => shortName(a).localeCompare(shortName(b)) || a.id.localeCompare(b.id));
    const callees = [...new Set(edges.filter((edge) => edge.caller === focusID).map((edge) => edge.callee))]
      .map((id) => nodeByID.get(id)).filter((node) => node && !node.is_test)
      .sort((a, b) => shortName(a).localeCompare(shortName(b)) || a.id.localeCompare(b.id));
    const placeRow = (row, y, gap = 168) => {
      const width = Math.max(760, Math.min(18000, (row.length - 1) * gap));
      row.forEach((node, index) => points.set(node.id, {
        x: row.length === 1 ? 0 : (index / (row.length - 1) - 0.5) * width,
        y,
      }));
      return width;
    };
    let y = 0;
    let layoutWidth = 760;
    if (tests.length) {
      layoutWidth = Math.max(layoutWidth, placeRow(tests, y, 156));
      y += 210;
    }
    if (callers.length) {
      layoutWidth = Math.max(layoutWidth, placeRow(callers, y));
      y += 210;
    }
    points.set(focusID, { x: 0, y });
    y += 210;
    if (callees.length) layoutWidth = Math.max(layoutWidth, placeRow(callees, y));
    state.layoutWidth = layoutWidth;
    state.testGap = tests.length > 1 ? 156 : 112;
    return points;
  }

  function colorFor(node) {
    if (node.is_test) return "#ffae78";
    const path = node.path || "";
    const pkg = state.fileByPath.get(path)?.package || path.split("/").slice(0, -1).join("/") || "root";
    return COLORS[hash(pkg) % COLORS.length];
  }

  function showToast(message, isError = false) {
    const toast = $("#toast");
    toast.textContent = message;
    toast.classList.toggle("error", isError);
    toast.classList.add("visible");
    clearTimeout(state.toastTimer);
    state.toastTimer = setTimeout(() => toast.classList.remove("visible"), 3600);
  }

  function renderRepoList() {
    const query = $("#repo-search").value.trim().toLowerCase();
    const list = $("#repo-list");
    const matches = state.datasets.filter((dataset) => `${dataset.name} ${dataset.repository} ${dataset.summary || ""}`.toLowerCase().includes(query));
    list.innerHTML = matches.map((dataset) => {
      const graph = dataset.graph || {};
      const active = dataset.id === state.active?.id;
      const tests = dataset.testCount ?? Object.keys(graph.tests || {}).length;
      const files = dataset.goFiles ?? graph.file_count ?? graph.files?.length ?? 0;
      const initials = dataset.name.split(/\s+/).slice(0, 2).map((part) => part[0]).join("").toUpperCase();
      return `<button class="repo-card ${active ? "selected" : ""}" data-dataset="${esc(dataset.id)}" type="button" aria-current="${active ? "true" : "false"}">
        <span class="repo-card-icon ${esc(dataset.accent || "mint")}">${esc(initials)}</span>
        <span class="repo-card-copy"><strong>${esc(dataset.name)}</strong><small>${esc(dataset.repository)}</small></span>
        <span class="repo-card-chevron">›</span>
        <span class="repo-card-summary">${esc(dataset.summary || "Graph snapshot")}</span>
        <span class="repo-card-stats"><span>${humanNumber(files)} Go files</span><i>·</i><span>${humanNumber(tests)} tests</span></span>
      </button>`;
    }).join("");
    $("#dataset-count").textContent = String(state.datasets.length).padStart(2, "0");
    if (!matches.length) list.innerHTML = `<div class="no-repos">No repositories match “${esc(query)}”.</div>`;
  }

  function defaultTestGraphID() {
    const tests = Object.keys(state.graph.tests || {});
    if (tests.length <= 100) return "";
    const candidates = tests.map((id) => {
      const test = state.graph.tests[id];
      const nodes = Array.isArray(test?.nodes) ? test.nodes.length : 0;
      const edges = Number(test?.edge_count) || (Array.isArray(test?.edges) ? test.edges.length : 0);
      return { id, nodes, edges };
    });
    const closestToUsefulSize = (items) => items.sort((a, b) =>
      Math.abs(a.nodes - 48) - Math.abs(b.nodes - 48) || b.edges - a.edges || a.id.localeCompare(b.id)
    )[0]?.id || "";
    const bounded = candidates.filter((test) => test.nodes >= 12 && test.nodes <= 120);
    if (bounded.length) return closestToUsefulSize(bounded);
    const small = candidates.filter((test) => test.nodes > 1 && test.nodes < 12).sort((a, b) => b.nodes - a.nodes || b.edges - a.edges);
    if (small.length) return small[0].id;
    const large = candidates.filter((test) => test.nodes > 120).sort((a, b) => a.nodes - b.nodes || b.edges - a.edges);
    if (large.length) return large[0].id;
    return candidates.filter((test) => test.nodes > 0).sort((a, b) => b.nodes - a.nodes || b.edges - a.edges)[0]?.id || "";
  }

  function buildFunctionPicker() {
    state.functionNodes = state.graph.nodes.filter((node) => !node.is_test);
    state.focusTestCache = new Map();
    state.functionLabelByID = new Map();
    state.functionSearchEntries = [];
    const counts = new Map();
    for (const node of state.functionNodes) {
      const key = shortName(node).toLowerCase();
      counts.set(key, (counts.get(key) || 0) + 1);
    }
    const usedLabels = new Set();
    for (const node of state.functionNodes) {
      const name = shortName(node);
      const location = `${node.path || ""}${node.line ? `:${node.line}` : ""}`;
      let label = counts.get(name.toLowerCase()) > 1 ? `${name} — ${location || node.id}` : name;
      if (usedLabels.has(label.toLowerCase())) label = `${label} — ${node.id}`;
      usedLabels.add(label.toLowerCase());
      state.functionLabelByID.set(node.id, label);
      state.functionSearchEntries.push({ node, name: name.toLowerCase(), path: (node.path || "").toLowerCase(), id: node.id.toLowerCase() });
    }
    const degree = new Map();
    for (const edge of state.graph.edges) {
      degree.set(edge.caller, (degree.get(edge.caller) || 0) + 1);
      degree.set(edge.callee, (degree.get(edge.callee) || 0) + 1);
    }
    state.functionSuggestions = [...state.functionNodes]
      .sort((a, b) => (degree.get(b.id) || 0) - (degree.get(a.id) || 0) || shortName(a).localeCompare(shortName(b)))
      .slice(0, 8);
    const input = $("#function-search");
    input.disabled = state.functionNodes.length === 0;
    input.value = "";
    closeFunctionOptions();
  }

  function defaultFunctionGraphID() {
    const testCount = Object.keys(state.graph.tests || {}).length;
    if (state.graph.nodes.length <= 2000 && testCount <= 100) return "";
    const testID = defaultTestGraphID();
    const sample = testID ? state.graph.tests[testID] : null;
    if (sample && Array.isArray(sample.nodes)) {
      const degree = new Map();
      for (const edge of sample.edges || []) {
        degree.set(edge.caller, (degree.get(edge.caller) || 0) + 1);
        degree.set(edge.callee, (degree.get(edge.callee) || 0) + 1);
      }
      const candidates = sample.nodes.map((id) => state.nodeByID.get(id)).filter((node) => node && !node.is_test);
      candidates.sort((a, b) => (degree.get(b.id) || 0) - (degree.get(a.id) || 0) || shortName(a).localeCompare(shortName(b)));
      if (candidates.length) return candidates[0].id;
    }
    return state.functionSuggestions[0]?.id || "";
  }

  function updateAllFunctionsButton() {
    const button = $("#all-functions");
    const allFunctions = !state.focusedID;
    button.classList.toggle("active", allFunctions);
    button.setAttribute("aria-pressed", String(allFunctions));
  }

  function closeFunctionOptions() {
    const input = $("#function-search");
    const options = $("#function-options");
    if (!input || !options) return;
    options.hidden = true;
    input.setAttribute("aria-expanded", "false");
    state.activeFunctionSuggestion = -1;
  }

  function renderFunctionOptions(query = $("#function-search").value) {
    const options = $("#function-options");
    const normalized = query.trim().toLowerCase();
    let matches;
    if (!normalized) {
      matches = state.functionSuggestions;
    } else {
      const best = [];
      for (const entry of state.functionSearchEntries) {
        const { node, name, path, id } = entry;
        const score = name.startsWith(normalized) ? 0
          : name.includes(normalized) ? 1
            : path.includes(normalized) ? 2
              : id.includes(normalized) ? 3 : -1;
        if (score < 0) continue;
        best.push({ node, score });
        if (best.length > 16) {
          best.sort((a, b) => a.score - b.score || shortName(a.node).localeCompare(shortName(b.node)) || a.node.id.localeCompare(b.node.id));
          best.length = 12;
        }
      }
      matches = best.sort((a, b) => a.score - b.score || shortName(a.node).localeCompare(shortName(b.node)) || a.node.id.localeCompare(b.node.id))
        .slice(0, 12).map((entry) => entry.node);
    }
    state.activeFunctionSuggestion = -1;
    options.innerHTML = matches.length ? matches.map((node, index) => {
      const line = node.line ? `:${node.line}` : "";
      const label = state.functionLabelByID.get(node.id) || shortName(node);
      return `<button class="function-option" type="button" role="option" aria-selected="false" data-function-id="${esc(node.id)}" data-option-index="${index}"><span class="function-option-copy"><strong>${esc(label)}</strong><small>${esc(node.path || "")}${esc(line)}</small></span><span class="function-option-tag">FUNCTION</span></button>`;
    }).join("") : `<div class="function-options-empty">No functions match “${esc(query.trim())}”.</div>`;
    options.hidden = false;
    $("#function-search").setAttribute("aria-expanded", "true");
  }

  function selectFunctionGraph(id) {
    const node = state.nodeByID.get(id);
    if (!node || node.is_test) return;
    if (state.focusedID !== node.id) state.showAllFocusTests = false;
    state.focusedID = node.id;
    state.selectedID = node.id;
    state.hoveredID = "";
    $("#function-search").value = state.functionLabelByID.get(id) || shortName(node);
    closeFunctionOptions();
    setGraphView();
  }

  function selectAllFunctions() {
    state.focusedID = "";
    state.selectedID = "";
    $("#function-search").value = "";
    closeFunctionOptions();
    setGraphView();
  }

  function relatedTestsForFunction(id) {
    if (state.focusTestCache.has(id)) return state.focusTestCache.get(id);
    const related = [];
    for (const [testID, view] of Object.entries(state.graph.tests || {})) {
      if (Array.isArray(view?.nodes) && view.nodes.includes(id) && state.nodeByID.get(testID)?.is_test) related.push(testID);
    }
    related.sort((a, b) => shortName(state.nodeByID.get(a)).localeCompare(shortName(state.nodeByID.get(b))) || a.localeCompare(b));
    state.focusTestCache.set(id, related);
    if (state.focusTestCache.size > 64) state.focusTestCache.delete(state.focusTestCache.keys().next().value);
    return related;
  }

  function setGraphView() {
    if (!state.graph) return;
    const focus = state.nodeByID.get(state.focusedID);
    if (focus && !focus.is_test) {
      const callerIDs = new Set();
      const calleeIDs = new Set();
      for (const edge of state.graph.edges) {
        if (edge.callee === focus.id) callerIDs.add(edge.caller);
        if (edge.caller === focus.id) calleeIDs.add(edge.callee);
      }
      callerIDs.delete(focus.id);
      calleeIDs.delete(focus.id);
      state.focusTestIDs = relatedTestsForFunction(focus.id);
      const visibleTestIDs = state.showAllFocusTests || state.focusTestIDs.length <= FOCUS_TEST_PREVIEW_LIMIT
        ? state.focusTestIDs
        : state.focusTestIDs.slice(0, FOCUS_TEST_PREVIEW_LIMIT);
      const visibleTestSet = new Set(visibleTestIDs);
      const nodeIDs = new Set([focus.id, ...[...callerIDs].filter((id) => !state.nodeByID.get(id)?.is_test),
        ...[...calleeIDs].filter((id) => !state.nodeByID.get(id)?.is_test), ...visibleTestIDs]);
      state.visibleNodes = [...nodeIDs].map((id) => state.nodeByID.get(id)).filter(Boolean);
      state.visibleEdges = state.graph.edges.filter((edge) =>
        (edge.callee === focus.id && (callerIDs.has(edge.caller) && !state.nodeByID.get(edge.caller)?.is_test || visibleTestSet.has(edge.caller))) ||
        (edge.caller === focus.id && calleeIDs.has(edge.callee))
      ).map((edge) => state.nodeByID.get(edge.caller)?.is_test ? { ...edge, type: "test-coverage" } : edge);
      const edgePairs = new Set(state.visibleEdges.map((edge) => `${edge.caller}\u0000${edge.callee}`));
      for (const testID of visibleTestIDs) {
        const key = `${testID}\u0000${focus.id}`;
        if (!edgePairs.has(key)) state.visibleEdges.push({ caller: testID, callee: focus.id, type: "test-coverage" });
      }
      state.unresolved = [];
      for (const id of nodeIDs) state.unresolved.push(...(state.unresolvedByCaller.get(id) || []));
      state.positions = functionCenteredLayout(state.visibleNodes, state.visibleEdges, focus.id);
      state.selectedID = focus.id;
      renderInspector(focus);
    } else {
      state.focusedID = "";
      state.focusTestIDs = [];
      state.visibleNodes = state.graph.nodes.slice();
      state.visibleEdges = state.graph.edges;
      state.unresolved = state.graph.unresolved_calls;
      state.positions = layeredLayout(state.visibleNodes, state.visibleEdges);
      state.selectedID = "";
      $("#empty-inspector").hidden = false;
      $("#node-inspector").hidden = true;
    }
    updateAllFunctionsButton();
    const testToggle = $("#focus-tests-toggle");
    testToggle.hidden = !state.focusedID || state.focusTestIDs.length <= FOCUS_TEST_PREVIEW_LIMIT;
    if (state.showAllFocusTests) {
      testToggle.textContent = `Show first ${FOCUS_TEST_PREVIEW_LIMIT}`;
      testToggle.setAttribute("aria-label", `Show first ${FOCUS_TEST_PREVIEW_LIMIT} of ${humanNumber(state.focusTestIDs.length)} related test roots`);
    } else {
      testToggle.textContent = `Show all tests · ${humanNumber(state.focusTestIDs.length)}`;
      testToggle.setAttribute("aria-label", `Show all ${humanNumber(state.focusTestIDs.length)} related test roots`);
    }
    $("#canvas-mode").textContent = state.focusedID ? "FUNCTION VIEW" : "FULL GRAPH";
    $("#canvas-node-count").textContent = `${humanNumber(state.visibleNodes.length)} NODES`;
    $("#stat-functions").textContent = humanNumber(state.visibleNodes.filter((node) => !node.is_test).length);
    $("#stat-edges").textContent = humanNumber(state.visibleEdges.filter((edge) => edge.type !== "test-coverage").length);
    $("#stat-tests").textContent = humanNumber(state.focusedID ? state.focusTestIDs.length : Object.keys(state.graph.tests || {}).length);
    const unresolvedStat = $("#stat-unresolved");
    unresolvedStat.textContent = state.graph.unresolved_calls_omitted ? "Omitted" : humanNumber(state.unresolved.length);
    unresolvedStat.title = state.graph.unresolved_calls_omitted
      ? `${humanNumber(state.graph.unresolved_call_count)} graph-wide unresolved-call details were omitted from this compact snapshot.`
      : "";
    $("#fingerprint").textContent = (state.graph.analysis_fingerprint || state.graph.source_fingerprint || "not recorded").slice(0, 15);
    renderRepoList();
    requestAnimationFrame(() => { fitGraph(); drawGraph(); });
  }

  async function activateDataset(dataset) {
    const request = ++state.loadSequence;
    if (state.loadController) state.loadController.abort();
    const controller = new AbortController();
    state.loadController = controller;
    const loading = $("#loading-overlay");
    loading.querySelector("span:last-child").textContent = `Loading ${dataset.name}`;
    loading.hidden = false;
    try {
      if (!dataset.graph) {
        const response = await fetch(dataset.file, { cache: "no-store", signal: controller.signal });
        if (!response.ok) throw new Error(`Could not load ${dataset.name} (${response.status}).`);
        if (dataset.file.toLowerCase().endsWith(".gz")) {
          if (typeof DecompressionStream === "undefined") throw new Error("This browser cannot open compressed graph snapshots.");
          const decompressed = response.body.pipeThrough(new DecompressionStream("gzip"));
          dataset.graph = normalizeGraph(await new Response(decompressed).json());
        } else {
          dataset.graph = normalizeGraph(await response.json());
        }
      }
      if (request !== state.loadSequence) return;
      state.active = dataset;
      state.graph = dataset.graph;
      state.nodeByID = new Map(state.graph.nodes.map((node) => [node.id, node]));
      state.fileByPath = new Map(state.graph.files.map((file) => [file.path, file]));
      state.unresolvedByCaller = new Map();
      for (const call of state.graph.unresolved_calls) {
        if (!state.unresolvedByCaller.has(call.caller)) state.unresolvedByCaller.set(call.caller, []);
        state.unresolvedByCaller.get(call.caller).push(call);
      }
      buildFunctionPicker();
      state.focusedID = defaultFunctionGraphID();
      state.showAllFocusTests = false;
      state.view = { x: 0, y: 0, zoom: 1 };
      const initials = dataset.name.split(/\s+/).slice(0, 2).map((part) => part[0]).join("").toUpperCase();
      $("#header-repo").textContent = dataset.repository;
      $("#graph-title").textContent = dataset.name;
      $("#graph-subtitle").textContent = dataset.repository;
      $("#repo-glyph").textContent = initials;
      $("#repo-glyph").className = `repo-glyph ${esc(dataset.accent || "mint")}`;
      $("#graph-language").textContent = (state.graph.language || "graph").toUpperCase();
      $("#snapshot-label").textContent = dataset.commit || "Local snapshot";
      $("#snapshot-date").textContent = dataset.updated || "Graph v2";
      $("#function-search").value = state.focusedID ? state.functionLabelByID.get(state.focusedID) || "" : "";
      setGraphView();
    } catch (error) {
      if (request === state.loadSequence && error?.name !== "AbortError") {
        showToast(error instanceof Error ? error.message : "Could not load this graph.", true);
      }
    } finally {
      if (request === state.loadSequence) {
        state.loadController = null;
        loading.hidden = true;
      }
    }
  }

  function fitGraph() {
    if (!state.visibleNodes.length || !state.width || !state.height) return;
    let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
    for (const node of state.visibleNodes) {
      const point = state.positions.get(node.id);
      if (!point) continue;
      minX = Math.min(minX, point.x); maxX = Math.max(maxX, point.x);
      minY = Math.min(minY, point.y); maxY = Math.max(maxY, point.y);
    }
    const graphWidth = Math.max(180, maxX - minX + 130);
    const graphHeight = Math.max(160, maxY - minY + 130);
    state.view.zoom = Math.max(0.008, Math.min(1.7, Math.min((state.width - 80) / graphWidth, (state.height - 70) / graphHeight)));
    state.view.x = -((minX + maxX) / 2) * state.view.zoom;
    state.view.y = -((minY + maxY) / 2) * state.view.zoom;
    drawGraph();
  }

  function matchesSearch(node) {
    if (!state.search) return false;
    return `${node.id} ${node.qualname || ""} ${node.path || ""}`.toLowerCase().includes(state.search);
  }

  function typeVisible(node) {
    return node.is_test ? state.showTests : state.showFunctions;
  }

  function drawGraph() {
    if (!context || !state.width || !state.height) return;
    const dpr = state.pixelRatio;
    context.setTransform(dpr, 0, 0, dpr, 0, 0);
    context.clearRect(0, 0, state.width, state.height);
    context.save();
    context.translate(state.width / 2 + state.view.x, state.height / 2 + state.view.y);
    context.scale(state.view.zoom, state.view.zoom);

    const visible = new Set(state.visibleNodes.filter(typeVisible).map((node) => node.id));
    const degreeByID = new Map();
    const selectedNeighbors = new Set();
    for (const edge of state.visibleEdges) {
      degreeByID.set(edge.caller, (degreeByID.get(edge.caller) || 0) + 1);
      degreeByID.set(edge.callee, (degreeByID.get(edge.callee) || 0) + 1);
      if (state.selectedID && edge.caller === state.selectedID) selectedNeighbors.add(edge.callee);
      if (state.selectedID && edge.callee === state.selectedID) selectedNeighbors.add(edge.caller);
    }
    for (const edge of state.visibleEdges) {
      if (!visible.has(edge.caller) || !visible.has(edge.callee)) continue;
      const a = state.positions.get(edge.caller), b = state.positions.get(edge.callee);
      if (!a || !b) continue;
      const adjacent = state.selectedID && (edge.caller === state.selectedID || edge.callee === state.selectedID);
      const searchMatch = state.search && (matchesSearch(state.nodeByID.get(edge.caller)) || matchesSearch(state.nodeByID.get(edge.callee)));
      const coverage = edge.type === "test-coverage";
      context.globalAlpha = coverage ? 0.72 : state.search ? (searchMatch ? 0.82 : 0.07) : state.selectedID ? (adjacent ? 0.86 : 0.1) : 0.38;
      context.strokeStyle = coverage ? "#ffae78" : adjacent ? "#b2a5ff" : searchMatch ? "#57d9c7" : "#546274";
      context.lineWidth = coverage || adjacent ? 1.55 : 0.85;
      context.setLineDash(coverage ? [5, 5] : []);
      context.beginPath(); context.moveTo(a.x, a.y); context.lineTo(b.x, b.y); context.stroke();
      context.setLineDash([]);
      if (adjacent) {
        const angle = Math.atan2(b.y - a.y, b.x - a.x);
        const tipX = b.x, tipY = b.y, size = 5.2;
        context.fillStyle = "#b2a5ff"; context.globalAlpha = 0.9;
        context.beginPath(); context.moveTo(tipX, tipY);
        context.lineTo(tipX - size * Math.cos(angle - 0.45), tipY - size * Math.sin(angle - 0.45));
        context.lineTo(tipX - size * Math.cos(angle + 0.45), tipY - size * Math.sin(angle + 0.45));
        context.closePath(); context.fill();
      }
    }

    const labelBudget = state.visibleNodes.length < 85 ? Infinity : state.visibleNodes.length < 260 ? 18 : 12;
    const testLabelStride = Math.max(1, Math.ceil(112 / Math.max(0.1, state.testGap * state.view.zoom)));
    let labelCount = 0;
    for (const node of state.visibleNodes) {
      if (!typeVisible(node)) continue;
      const point = state.positions.get(node.id);
      if (!point) continue;
      const selected = node.id === state.selectedID;
      const hovered = node.id === state.hoveredID;
      const searched = matchesSearch(node);
      const related = selectedNeighbors.has(node.id);
      const degree = degreeByID.get(node.id) || 0;
      const radius = Math.min(7.4, (node.is_test ? 4.8 : 2.8) + Math.log2(degree + 1) * 0.58);
      const color = colorFor(node);
      context.globalAlpha = state.search ? (searched ? 1 : 0.2) : state.selectedID ? (selected || hovered || related ? 1 : 0.35) : 0.92;
      context.fillStyle = color;
      context.shadowColor = selected || hovered || searched ? color : "transparent";
      context.shadowBlur = selected || hovered || searched ? 18 : 0;
      context.beginPath(); context.arc(point.x, point.y, selected || hovered ? radius + 1.7 : radius, 0, Math.PI * 2); context.fill();
      context.shadowBlur = 0;
      if (node.is_test || selected || hovered || searched) {
        context.strokeStyle = color; context.globalAlpha = selected || hovered ? 0.7 : 0.24;
        context.lineWidth = selected || hovered ? 1.2 : 0.7;
        context.beginPath(); context.arc(point.x, point.y, radius + (selected ? 8 : 4), 0, Math.PI * 2); context.stroke();
      }
      const testSlot = Math.round((point.x + state.layoutWidth / 2) / state.testGap);
      const spacedTestLabel = node.is_test && Math.abs(testSlot) % testLabelStride === 0;
      const alwaysLabel = selected || hovered || searched || spacedTestLabel;
      const canLabel = alwaysLabel || (labelCount < labelBudget && state.view.zoom > 0.76 && degree >= 3);
      if (canLabel) {
        labelCount += 1;
        context.globalAlpha = alwaysLabel ? 0.96 : 0.61;
        context.fillStyle = selected || hovered ? "#f3f4fb" : "#bdc5d3";
        context.font = `${selected || node.is_test ? 11 : 10}px ui-monospace, SFMono-Regular, Menlo, monospace`;
        context.textAlign = "left"; context.textBaseline = "middle";
        const text = shortName(node);
        context.fillText(text.length > 35 ? `${text.slice(0, 32)}…` : text, point.x + radius + 8, point.y);
      }
    }
    context.restore();
  }

  function screenPoint(event) {
    const rect = canvas.getBoundingClientRect();
    return { x: event.clientX - rect.left, y: event.clientY - rect.top };
  }

  function worldPoint(screen) {
    return {
      x: (screen.x - state.width / 2 - state.view.x) / state.view.zoom,
      y: (screen.y - state.height / 2 - state.view.y) / state.view.zoom,
    };
  }

  function nodeAt(screen) {
    const world = worldPoint(screen);
    let nearest = null, nearestDistance = Infinity;
    for (const node of state.visibleNodes) {
      if (!typeVisible(node)) continue;
      const point = state.positions.get(node.id);
      if (!point) continue;
      const distance = Math.hypot(point.x - world.x, point.y - world.y);
      const hitArea = Math.max(11, 9 / state.view.zoom);
      if (distance < hitArea && distance < nearestDistance) { nearest = node; nearestDistance = distance; }
    }
    return nearest;
  }

  function selectNode(node) {
    if (!node) return;
    if (!node.is_test) {
      selectFunctionGraph(node.id);
      return;
    }
    state.selectedID = node.id;
    state.hoveredID = "";
    renderInspector(node);
    drawGraph();
  }

  function renderInspector(node) {
    $("#empty-inspector").hidden = true;
    const panel = $("#node-inspector");
    panel.hidden = false;
    const callEdges = state.visibleEdges.filter((edge) => edge.type !== "test-coverage");
    const outgoing = callEdges.filter((edge) => edge.caller === node.id).map((edge) => edge.callee);
    const incoming = callEdges.filter((edge) => edge.callee === node.id).map((edge) => edge.caller);
    const unresolved = state.unresolved.filter((call) => call.caller === node.id);
    const relatedTests = node.is_test
      ? (state.graph.tests[node.id] ? [node.id] : [])
      : node.id === state.focusedID ? state.focusTestIDs : relatedTestsForFunction(node.id);
    const file = state.fileByPath.get(node.path);
    const line = node.line ? `:${node.line}` : "";
    const nodeKind = node.is_test ? "TEST ROOT" : "FUNCTION";
    const unresolvedDetails = state.graph.unresolved_calls_omitted
      ? `<section class="detail-section unresolved-section"><div class="detail-section-title"><span>UNRESOLVED CALLS</span><span>OMITTED</span></div><p class="empty-related">${humanNumber(state.graph.unresolved_call_count)} graph-wide records were omitted from this compact browser snapshot.</p></section>`
      : unresolved.length ? `<section class="detail-section unresolved-section"><div class="detail-section-title"><span>UNRESOLVED CALLS</span><span>${unresolved.length}</span></div><ul class="unresolved-list">${unresolved.slice(0, 8).map((call) => `<li><code>${esc(call.expression)}</code><span>${esc(call.path)}:${call.line}</span></li>`).join("")}</ul></section>` : "";
    const makeLinks = (ids, empty, limit = 12) => ids.length ? `<div class="related-list">${ids.slice(0, limit).map((id) => {
      const related = state.nodeByID.get(id);
      return `<button class="related-node" type="button" data-node-id="${esc(id)}"><span class="related-node-dot ${related?.is_test ? "test" : ""}"></span><span>${esc(shortName(related) || id)}</span><span class="related-arrow">↗</span></button>`;
    }).join("")}${ids.length > limit ? `<span class="more-items">+${ids.length - limit} more</span>` : ""}</div>` : `<p class="empty-related">${empty}</p>`;
    panel.innerHTML = `
      <div class="node-kind-row"><span class="node-kind ${node.is_test ? "test" : "function"}"><i></i>${nodeKind}</span><span class="node-line">${esc(node.path || "")}${esc(line)}</span></div>
      <h3 class="node-name">${esc(shortName(node))}</h3>
      <p class="node-id">${esc(node.id)}</p>
      <div class="detail-grid"><div><span>PACKAGE</span><strong>${esc(file?.package || "—")}</strong></div><div><span>CALLEES</span><strong>${outgoing.length}</strong></div><div><span>CALLERS</span><strong>${incoming.length}</strong></div><div><span>END LINE</span><strong>${node.end_line || "—"}</strong></div></div>
      <section class="detail-section"><div class="detail-section-title"><span>DIRECT CALLEES</span><span>${outgoing.length}</span></div>${makeLinks(outgoing, "No direct child functions in this view.")}</section>
      <section class="detail-section"><div class="detail-section-title"><span>DIRECT CALLERS</span><span>${incoming.length}</span></div>${makeLinks(incoming, "No direct parent functions in this view.")}</section>
      <section class="detail-section"><div class="detail-section-title"><span>${node.is_test ? "RELATED TEST ROOTS" : "TESTS USING THIS FUNCTION"}</span><span>${relatedTests.length}</span></div>${makeLinks(relatedTests, "No related tests were found for this function.", 20)}</section>
      ${unresolvedDetails}
    `;
    panel.querySelectorAll(".related-node").forEach((button) => button.addEventListener("click", () => {
      const id = button.dataset.nodeId;
      const target = state.nodeByID.get(id);
      if (target?.is_test) selectNode(target);
      else if (target) selectFunctionGraph(target.id);
    }));
  }

  async function importFile(file) {
    if (!file) return;
    try {
      const graph = normalizeGraph(JSON.parse(await file.text()));
      const baseName = file.name.replace(/\.json$/i, "").replace(/[-_]+/g, " ");
      const dataset = {
        id: `local-${Date.now()}`, name: baseName || "Local graph", repository: "opened from this computer",
        file: file.name, commit: "local file", updated: "Current browser session", summary: "Imported graph-v2 snapshot",
        accent: "violet", graph,
      };
      state.datasets.unshift(dataset);
      await activateDataset(dataset);
      showToast(`Opened ${file.name} locally`);
    } catch (error) {
      showToast(error instanceof Error ? error.message : "Could not read that graph file.", true);
    }
  }

  async function loadLibrary() {
    const loading = $("#loading-overlay");
    try {
      const response = await fetch("./data/catalog.json", { cache: "no-store" });
      if (!response.ok) throw new Error(`Could not load the graph library (${response.status}).`);
      const catalog = await response.json();
      const datasets = (catalog.datasets || []).map((entry) => ({ ...entry, graph: null }));
      state.datasets = datasets;
      renderRepoList();
      if (!datasets.length) throw new Error("The graph library is empty. Open a graph-v2 JSON file to begin.");
      await activateDataset(datasets[0]);
    } catch (error) {
      showToast(error instanceof Error ? error.message : "Could not load the graph library.", true);
      $("#graph-title").textContent = "Graph library unavailable";
      $("#graph-subtitle").textContent = "Start the local server from site/test-graphs";
      loading.hidden = true;
    }
  }

  function resizeCanvas() {
    const rect = canvas.getBoundingClientRect();
    state.width = rect.width;
    state.height = rect.height;
    state.pixelRatio = Math.min(window.devicePixelRatio || 1, 2);
    canvas.width = Math.max(1, Math.floor(rect.width * state.pixelRatio));
    canvas.height = Math.max(1, Math.floor(rect.height * state.pixelRatio));
    drawGraph();
  }

  function zoomAt(screen, factor) {
    const before = worldPoint(screen);
    state.view.zoom = Math.max(0.008, Math.min(3.5, state.view.zoom * factor));
    state.view.x = screen.x - state.width / 2 - before.x * state.view.zoom;
    state.view.y = screen.y - state.height / 2 - before.y * state.view.zoom;
    drawGraph();
  }

  $("#repo-list").addEventListener("click", (event) => {
    const button = event.target.closest("[data-dataset]");
    if (!button) return;
    const dataset = state.datasets.find((entry) => entry.id === button.dataset.dataset);
    if (dataset && dataset !== state.active) activateDataset(dataset);
  });
  $("#repo-search").addEventListener("input", renderRepoList);
  $("#refresh-button").addEventListener("click", loadLibrary);
  const functionSearch = $("#function-search");
  const functionOptions = $("#function-options");
  functionSearch.addEventListener("focus", () => {
    functionSearch.select();
    renderFunctionOptions("");
  });
  functionSearch.addEventListener("input", () => renderFunctionOptions());
  $("#focus-tests-toggle").addEventListener("click", () => {
    state.showAllFocusTests = !state.showAllFocusTests;
    setGraphView();
  });
  functionSearch.addEventListener("keydown", (event) => {
    const options = [...functionOptions.querySelectorAll("[data-function-id]")];
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      if (functionOptions.hidden) renderFunctionOptions();
      const count = functionOptions.querySelectorAll("[data-function-id]").length;
      if (!count) return;
      event.preventDefault();
      const step = event.key === "ArrowDown" ? 1 : -1;
      state.activeFunctionSuggestion = (state.activeFunctionSuggestion + step + count) % count;
      functionOptions.querySelectorAll("[data-function-id]").forEach((button, index) => {
        const active = index === state.activeFunctionSuggestion;
        button.classList.toggle("active", active);
        button.setAttribute("aria-selected", String(active));
      });
      functionOptions.querySelector(".active")?.scrollIntoView({ block: "nearest" });
    } else if (event.key === "Enter") {
      const active = functionOptions.querySelector("[aria-selected='true'][data-function-id]");
      const only = options.length === 1 ? options[0] : null;
      const target = active || only;
      if (target) {
        event.preventDefault();
        selectFunctionGraph(target.dataset.functionId);
      }
    } else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      closeFunctionOptions();
    }
  });
  functionOptions.addEventListener("click", (event) => {
    const option = event.target.closest("[data-function-id]");
    if (option) selectFunctionGraph(option.dataset.functionId);
  });
  document.addEventListener("pointerdown", (event) => {
    if (!event.target.closest(".function-picker")) closeFunctionOptions();
  });
  $("#all-functions").addEventListener("click", selectAllFunctions);
  $("#filter-tests").addEventListener("click", (event) => {
    state.showTests = !state.showTests;
    event.currentTarget.classList.toggle("active", state.showTests);
    event.currentTarget.setAttribute("aria-pressed", String(state.showTests)); drawGraph();
  });
  $("#filter-functions").addEventListener("click", (event) => {
    state.showFunctions = !state.showFunctions;
    event.currentTarget.classList.toggle("active", state.showFunctions);
    event.currentTarget.setAttribute("aria-pressed", String(state.showFunctions)); drawGraph();
  });
  $("#node-search").addEventListener("input", (event) => { state.search = event.target.value.trim().toLowerCase(); drawGraph(); });
  $("#fit-graph").addEventListener("click", fitGraph);
  $("#zoom-in").addEventListener("click", () => zoomAt({ x: state.width / 2, y: state.height / 2 }, 1.22));
  $("#zoom-out").addEventListener("click", () => zoomAt({ x: state.width / 2, y: state.height / 2 }, 1 / 1.22));
  $("#clear-selection").addEventListener("click", () => {
    state.selectedID = ""; $("#empty-inspector").hidden = false; $("#node-inspector").hidden = true; drawGraph();
  });
  $("#import-button").addEventListener("click", () => $("#file-input").click());
  $("#file-input").addEventListener("change", (event) => { importFile(event.target.files[0]); event.target.value = ""; });
  $("#canvas-frame").addEventListener("dragover", (event) => { event.preventDefault(); $("#drop-overlay").hidden = false; });
  $("#canvas-frame").addEventListener("dragleave", (event) => { if (!event.relatedTarget || !event.currentTarget.contains(event.relatedTarget)) $("#drop-overlay").hidden = true; });
  $("#canvas-frame").addEventListener("drop", (event) => {
    event.preventDefault(); $("#drop-overlay").hidden = true;
    const file = [...event.dataTransfer.files].find((item) => item.name.toLowerCase().endsWith(".json"));
    if (file) importFile(file); else showToast("Drop a graph-v2 .json file to open it.", true);
  });
  canvas.addEventListener("pointerdown", (event) => {
    canvas.setPointerCapture(event.pointerId);
    state.pointer = screenPoint(event); state.moved = false; canvas.classList.add("dragging");
  });
  canvas.addEventListener("pointermove", (event) => {
    const point = screenPoint(event);
    if (state.pointer) {
      const dx = point.x - state.pointer.x, dy = point.y - state.pointer.y;
      if (Math.abs(dx) + Math.abs(dy) > 2) state.moved = true;
      state.view.x += dx; state.view.y += dy; state.pointer = point; drawGraph();
    } else {
      const node = nodeAt(point);
      const next = node?.id || "";
      if (next !== state.hoveredID) { state.hoveredID = next; canvas.style.cursor = next ? "pointer" : "grab"; drawGraph(); }
    }
  });
  const endPointer = (event) => {
    if (!state.pointer) return;
    if (!state.moved) selectNode(nodeAt(screenPoint(event)));
    state.pointer = null; canvas.classList.remove("dragging");
  };
  canvas.addEventListener("pointerup", endPointer);
  canvas.addEventListener("pointercancel", () => { state.pointer = null; canvas.classList.remove("dragging"); });
  canvas.addEventListener("pointerleave", () => { if (!state.pointer) { state.hoveredID = ""; drawGraph(); } });
  canvas.addEventListener("wheel", (event) => {
    event.preventDefault(); zoomAt(screenPoint(event), Math.exp(-event.deltaY * 0.0012));
  }, { passive: false });
  canvas.addEventListener("dblclick", fitGraph);
  canvas.addEventListener("keydown", (event) => {
    const step = event.shiftKey ? 90 : 42;
    if (event.key === "0") fitGraph();
    else if (event.key === "+" || event.key === "=") zoomAt({ x: state.width / 2, y: state.height / 2 }, 1.2);
    else if (event.key === "-") zoomAt({ x: state.width / 2, y: state.height / 2 }, 1 / 1.2);
    else if (event.key === "ArrowLeft") state.view.x += step;
    else if (event.key === "ArrowRight") state.view.x -= step;
    else if (event.key === "ArrowUp") state.view.y += step;
    else if (event.key === "ArrowDown") state.view.y -= step;
    else return;
    event.preventDefault(); drawGraph();
  });
  document.addEventListener("keydown", (event) => {
    const typing = ["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName);
    if (event.key === "/" && !typing) { event.preventDefault(); $("#repo-search").focus(); }
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") { event.preventDefault(); $("#node-search").focus(); }
    if (event.key === "Escape") {
      if (!functionOptions.hidden) { closeFunctionOptions(); return; }
      if ($("#node-search").value) { $("#node-search").value = ""; state.search = ""; drawGraph(); }
      else $("#clear-selection").click();
    }
  });

  new ResizeObserver(resizeCanvas).observe(canvas);
  loadLibrary();
})();
