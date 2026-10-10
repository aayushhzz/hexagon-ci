# Deterministic call graphs for lifejacket coverage + agent-driven test PRs

Design note. Hackathon (AWS + DevRev). Audience: engineers extending `hexagon-ci`.

## 1. Problem restated

We want two outcomes for a backend repo (example: `janus`), using the
existing E2E/API suite in `lifejacket` that drives `gateway` endpoints:

1. **Coverage analysis** of the repo *as exercised by lifejacket tests* -
   which API operations, request/response fields, handler branches, and
   status codes are hit, and which are not.
2. **Agent-consumable extraction** - a structured "coverage gap" artifact an
   autonomous coding agent can turn into new lifejacket test PRs.

The naive instinct is "one big static call graph." The research and our own
recon say the honest design is a **layered graph with a deterministic spine
plus a runtime/trace overlay**, not a single whole-program static call graph.

## 2. The deterministic spine we actually have (recon result)

The three repos are joined by stable, machine-derivable keys. Verified chain
for `experience.toggles.get`:

```
lifejacket test func
  (testcases/api/v2_internal/experience/experience_toggles_get_test.go)
→ generated client method  authClient.ExperienceTogglesGetPost
  (gen/v2_internal/client.go: operationPath "/internal/experience.toggles.get", POST)
→ HTTP operation key:  POST /internal/experience.toggles.get
→ OpenAPI operationId:  experience-toggles-get-post
  (gateway/apiv2/specs/openapi-internal.yaml)
→ proto RPC + endpoint annotation
  (gateway/apiv2/experience/experience.proto: rpc ExperienceTogglesGet,
   option method_options{ endpoint:"experience.toggles.get", http_method:GET })
→ gateway handler  Server.ExperienceTogglesGet
  (gateway/experience.go:877)
→ request/response proto messages (fields + status surface)
  (apiv2/experience/experience.pb.go: Toggles []string, Actor *DONProto; resp JsonObject)
→ backend call  s.gateway.<client>.<RPC>  (here archonClient.GetInputs;
   for other ops, s.gateway.janus2.GetDevOrg -> janus-client module)
→ backend handler in janus
  (janus/internal/service/*.go, registered via api.RegisterJanusServer)
```

Key facts that make this deterministic (no ML needed for the spine):

- **The join key is the HTTP operation path + verb**, generated identically on
  both sides from the same proto. lifejacket's client path string ==
  gateway's `endpoint` annotation with `/internal/` (or `/`) prefix.
- **Field + status surface is authoritative in gateway `apiv2/*.proto`** and
  its generated `.pb.go`; the lifejacket `gen` types and the
  `SetJSON200/400/401/403/413/429/500/503` setters mirror it. So the
  "fields and paths to test" are enumerable statically from the proto/OpenAPI.
- **Backend edge is a proto client call**: `s.gateway.<svc>.<Rpc>(...)`. The
  callee resolves to an RPC FQN; the janus server implements the same method
  name and registers it with `RegisterJanusServer`. That gives a static
  gateway-RPC -> janus-handler link by method name, even across modules.

Boundary caveat (from hexagon-ci analysis): the current tool is **single-repo,
syntactic, no external modules**. Go import resolution is restricted to the
current module prefix and it never reads `janus-client`/`gateway` module
source. So today it cannot by itself walk lifejacket -> gateway -> janus.

## 3. What industry/research says (so we don't reinvent badly)

Condensed from the literature review (full citations at bottom):

- **Google TAP / "Taming Google-Scale Continuous Testing"**: production TIA is
  built on a **build-target dependency graph**, not a whole-program method
  call graph. Call-graph distance + history are *ranking* signals.
- **Google "Regression Test Selection at Scale" (2026)**: closest to us -
  a **hybrid call graph** selects a budgeted subset, then **ML ranks** with
  graph-derived features. Takeaway: use the deterministic graph for the
  candidate set, add ranking on top; don't expect the graph to prove safety.
- **Meta Predictive Test Selection**: build-dependency candidate set + ML to
  predict likely-failing tests; >95% failure recall at ~1/3 the tests. Flaky
  labels poison the model - handle flakiness explicitly.
- **Uber BITS ("Shifting E2E Testing Left")**: for microservices the practical
  impact relation is **trace-derived service/endpoint coverage** - "which
  tests actually hit which endpoint", recorded from force-sampled traces. This
  is the single most relevant precedent for our E2E setup: a dynamic overlay
  beats pure static reachability across service hops.
- **Meta Infer**: compositional, summary-based interprocedural analysis - the
  right shape if we ever need scalable static field/branch summaries.
- **REST API coverage criteria (Martin-Lopez et al., A-TEST 2019)**: the exact
  metric vocabulary we should emit - *path, operation, parameter,
  parameter-value, operation-flow, status/output coverage*. Keep this separate
  from implementation (line/branch) coverage.
- **RESTler (ICSE'19)**: infer producer-consumer dependencies from OpenAPI to
  build **stateful request sequences** (POST id -> GET/PUT/DELETE). We can
  derive operation-flow prerequisites the same way for test setup.
- **EvoMaster**: search-based white-box generation toward code coverage - a
  fallback generator when the agent needs inputs.
- **LLM/agentic REST testing (RESTGPT, KAT, AutoRestTest ICSE'25, APIPilot
  2026)**: the winning pattern is **LLM proposes, execution validates**:
  dependency/constraint hypotheses from the spec, grounded by real responses,
  refined in a loop. This is exactly the shape of Goal B.

**Net design guidance:**
1. Deterministic graph = candidate generator + coverage denominator, not a
   safety proof.
2. Add a **runtime/trace overlay** (Uber-style) to know what lifejacket really
   hits across the gateway->backend hop.
3. Emit coverage against **REST API coverage criteria**, split from code
   coverage.
4. For generation, **agent proposes tests, lifejacket run validates**, loop.

## 4. Proposed architecture

### Layer 0 - Operation catalog (source of truth, deterministic)
Parse gateway `apiv2/*.proto` (+ generated `openapi-internal.yaml`) into an
`operations` table keyed by `(verb, path)` / `operationId`:
- request message -> field tree (name, type, required, enum domain, format)
- response message -> field tree
- declared status codes
- RPC FQN + gateway handler symbol + endpoint annotation
This is the **denominator** for operation/parameter/status coverage. No call
graph needed; it's a proto walk. Cheap and fully deterministic.

### Layer 1 - lifejacket test -> operation map (deterministic)
Extend the existing graph builder minimally: when a lifejacket test (Ginkgo
`It`/`Describe` + `MakeRequest(ctx, authClient.Xxx, ...)`) references a
generated client method, resolve that method -> its `operationPath`+verb (the
builder already finds the call; add a resolver from client method name to the
operation key via the `gen` package). Output: `test -> operation(s)` edges,
plus which request fields the test sets and which response fields/status it
asserts (AST of the test body: struct literal keys, `Expect(res.X)`,
`Expect(response.StatusCode).To(Equal(...))`).
This directly yields **operation / parameter / parameter-value / status
coverage** of lifejacket over the API surface - Goal A's first cut, with zero
runtime dependency.

### Layer 2 - gateway handler -> backend graph (deterministic, cross-repo)
Teach the Go frontend a **multi-module mode**: given lifejacket + gateway +
janus checkouts, allow import resolution across the configured module prefixes
(`github.com/devrev/gateway`, `.../janus`, `.../janus-client`) and namespace
node IDs by module to avoid path collisions. Resolve:
- gateway handler -> `s.gateway.<svc>.<Rpc>` proto client call -> RPC FQN
- RPC FQN -> janus server method (match by method name + RegisterXxxServer)
This gives the static spine lifejacket-op -> gateway-handler -> janus-handler.
It will be *partial* (interface dispatch, dynamic, etc. remain unresolved) -
accept that; it's the candidate graph, not proof.

### Layer 3 - runtime/trace overlay (Uber BITS pattern) [high-value]
Run the lifejacket suite once against a traced environment (gateway + janus
with request/response logging or OpenTelemetry). Index:
`test -> operation -> {services, RPCs, handler funcs, status code observed}`.
This closes the static gaps across module/interface boundaries and tells us
*actual* hit branches/status. Correlate traces back to Layer 2 node IDs.
Even coarse access logs (operation + status per test) massively improve Goal A
fidelity. On AWS this maps to running the suite in a sandbox and shipping
traces to a store for correlation.

### Layer 4 - coverage model (the artifact)
Combine denominator (Layer 0) with hits (Layers 1+3) into a schema keyed by
REST coverage criteria, per operation:
```
operation: {verb, path, operationId, rpcFQN, gatewayHandler, backendHandler}
request_fields:  [{path, type, required, enum?, covered_by:[test...], values_seen:[...]}]
response_fields: [{path, covered_by:[test...]}]
status_codes:    [{code, covered_by:[test...]}]   # declared vs observed
operation_flow:  [prereq ops from producer-consumer inference]
coverage: {operation:bool, params:0..1, param_values:0..1, statuses:0..1}
gaps:     [ "status 403 never asserted", "field actor never set",
            "enum value X untested", ... ]
```

### Layer 5 - agent handoff (Goal B)
For each gap, emit an actionable record the coding agent consumes:
```
gap_id, operation, kind(status|param|param_value|flow|field),
target (e.g. assert 403 when actor != caller),
request_schema + example payload (synthesized from proto + values_seen),
expected_outcome,
suggested_test_file (sibling of existing op test in testcases/api/...),
repo_conventions {client helper: httputils.MakeRequest, auth: authClient,
  ginkgo idioms, labels/owners header block},
covering_tests_today, confidence, provenance(trace/static)
```
Agent loop (RESTGPT/AutoRestTest/APIPilot pattern): propose test -> run via
lifejacket `make`/ginkgo focus -> observe result/trace -> refine -> open PR
using the repo PR template, filling the work-item field. The deterministic
layers give grounded targets so the LLM isn't guessing the API shape.

## 5. Why this is the "optimised" shape

- **Deterministic where determinism is free** (proto/OpenAPI/operation keys):
  no ML, no flakiness, exact denominator. This is the hackathon's "deterministic
  flow."
- **Static cross-repo graph only as a candidate generator**, mirroring Google's
  hybrid-graph-then-rank and the honest `run_full_suite=True` stance already in
  `impact.py`. We never claim static proof of coverage.
- **Trace overlay** borrows Uber's microservice-correct impact relation and
  fixes the exact gaps hexagon-ci's analysis flagged (external modules,
  interface dispatch, branch/status reality).
- **Agent uses grounded gaps**, matching the strongest current REST-testing
  research (propose-then-validate), which is far more reliable than letting an
  LLM invent API calls.

## 6. Concrete next steps for hexagon-ci

1. Layer 0: proto/OpenAPI -> operation catalog JSON (standalone, fast win).
2. Layer 1: resolver from lifejacket generated client method -> operation key;
   extract set request fields + asserted response fields/status from test AST.
3. Emit the Layer 4 coverage JSON for one service (experience or a janus-backed
   op) as the demo.
4. Layer 2 multi-module Go resolution (module-namespaced node IDs) - stretch.
5. Layer 3 trace overlay via a single instrumented suite run - highest-fidelity
   stretch, strongly recommended for the demo narrative.
6. Layer 5 gap records + wire one autonomous PR end-to-end on lifejacket.

## 7. References
- Memon et al., *Taming Google-Scale Continuous Testing*, ICSE-SEIP 2017.
- Google Research, *Regression Test Selection at Scale*, 2026 (hybrid call graph + ML).
- Machalica et al., *Predictive Test Selection* (Meta), arXiv:1810.05286.
- Calcagno et al., *Moving Fast with Software Verification* (Infer).
- Uber Eng, *Shifting E2E Testing Left* (BITS); *SLATE*.
- Netflix, *Sequential testing / canaries* (post-deploy, complementary).
- Martin-Lopez, Segura, Ruiz-Cortes, *Test Coverage Criteria for RESTful Web APIs*, A-TEST 2019.
- Atlidakis, Godefroid, Polishchuk, *RESTler: Stateful REST API Fuzzing*, ICSE 2019.
- Arcuri, *EvoMaster* (search-based white-box REST generation), 2019.
- Kim et al., *RESTGPT* (ICSE-NIER 2024); *KAT* (ICST 2024);
  *AutoRestTest* multi-agent (ICSE 2025); *APIPilot* (arXiv 2026).
