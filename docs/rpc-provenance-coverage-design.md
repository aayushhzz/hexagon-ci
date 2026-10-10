# Per-RPC provenance and input-path analysis for lifejacket coverage

Design spec. Hackathon (AWS + DevRev). This covers the HARD part only:
deterministic, field-sensitive provenance extraction inside a backend service
(example: `janus`) per RPC, and how it produces coverage analysis (Goal A)
against the lifejacket suite. Goals B (agent gap records) and C (deterministic
test skipping) are derived from the same provenance and sketched at the end.

The easy part (lifejacket Ginkgo -> gateway OpenAPI operation -> service, and
the one-time manual endpoint->service table) is assumed solved and is NOT
re-specified here.

## 0. Scope and non-goals

In scope:
- Load one backend service with full type information.
- Enumerate RPC entrypoints.
- For each entrypoint, extract a deterministic, field-sensitive provenance map
  from the request message: input access-paths, the branches they gate, the
  behavioral equivalence classes, and the sinks/outcomes each class reaches.
- Map lifejacket test facts onto that map to compute coverage.

Non-goals:
- No ML, no fuzzing, no runtime trace requirement for the core (a trace overlay
  is optional and additive, not part of the deterministic core).
- Not a full points-to/alias-sound verifier. We are precise where SSA + VTA are
  precise and conservative (explicitly marked UNRESOLVED) elsewhere.
- Not a test generator. We emit obligations, generation is Goal B tooling.

## 1. Why typed SSA, not AST

Grounding from janus (verified):
- `GetDevOrg` (internal/service/devorg.go:1782) reads `in.DevOid.GetValue()`,
  derives `devOID`, gates `if devOID == ""` -> `errInvalidJWTToken`, flows into
  `NewDevOrgDON` -> `readDevOrgWithCache` (DB), then branches on
  `userContext.DevOID != devOID` to strip response fields.
- `CreateAccountValidator.Validate` (internal/service/account_validations.go:31)
  reads `req.GetAccount().DisplayName/.GetWebsites()/.GetDomains()/
  .GetExternalRefs()`, `req.UniqueDisplayNameSuffix`, runs a DB uniqueness query,
  and is reached through the `Validator` interface plus a `type assertion`
  `in.(*api.CreateAccountRequest)`.

These require: proto getter chains, receiver flow, cross-package calls,
interface dispatch, and type assertions. The existing AST reachability builder
leaves all of these unresolved by design. Therefore the provenance pass is
built on:

- `golang.org/x/tools/go/packages` with `NeedTypes | NeedSyntax |
  NeedTypesInfo | NeedDeps` to load `janus` with complete type info.
- `golang.org/x/tools/go/ssa` + `ssautil.AllFunctions` to get SSA for the
  service and its dependencies in scope.
- `golang.org/x/tools/go/callgraph/vta` (Variable Type Analysis) over an
  RTA/CHA base for deterministic interface-dispatch resolution.

janus is `go 1.27.1` and already has `golang.org/x/tools v0.49.0`, so all of the
above is available. SSA and VTA are deterministic for a fixed input and build
context.

## 2. Pipeline overview

```
load (go/packages, typed)
  -> build SSA (whole program in scope)
  -> build callgraph (VTA)
  -> enumerate RPC entrypoints
  -> for each entrypoint: field-sensitive taint from request param
       using compositional per-function summaries
  -> emit per-RPC provenance JSON  (the "useful information")
  -> map lifejacket test facts -> coverage JSON   (Goal A)
```

Everything after `load` is pure analysis over immutable SSA, hence
deterministic. The output is fingerprinted by (service source revision, module
graph, analyzer version, build context), mirroring the existing
`analysis_fingerprint` discipline.

## 3. RPC entrypoint enumeration

An entrypoint is a method selected by the service's gRPC registration, not just
any exported method. Resolution order:

1. Primary: find the `RegisterXxxServer(grpcServer, janus)` calls (e.g.
   `api.RegisterJanusServer`, internal/service/service.go) and read the
   registered server interface's method set. This is the authoritative RPC set
   and matches the proto service.
2. Each interface method resolves to the concrete `*Janus` method via SSA
   (the dynamic type registered is `*Janus`).
3. Signature contract: `func (janus *Janus) M(ctx context.Context,
   in *<pkg>.<Msg>) (*<pkg>.<Resp>, error)`. The `in` parameter is the taint
   seed. The result message is the response-field sink target.

This yields the ~193 real entrypoints without guessing from naming.

## 4. Access paths and the taint lattice

### 4.1 Access path
An access path is a request-rooted field chain, canonicalized across proto
getters and pointer derefs:

```
in.DevOid                      (wrapper getters collapsed: .GetValue() == value)
in.Account.DisplayName
in.Account.Domains[*]          ([*] = element of repeated/slice, index-insensitive)
in.<oneof>.<variant>           (oneof variants are distinct paths)
```

Canonicalization rules:
- `GetX()` proto getter == field `X` read.
- Wrapper types (`wrapperspb.StringValue` etc.) collapse `.Value`/`.GetValue()`.
- Slice/map/repeated element access is index-insensitive, marked `[*]`.
- oneof wrapper structs expand to one path per variant.

### 4.2 Lattice
Per SSA value we track a set of `(accessPath, kind)` labels, where `kind` is a
small enum: `read`, `derived` (value computed from tainted inputs), and
`predicate` (participates in a branch condition). The lattice is the powerset of
labels ordered by subset. Join is union. It is finite per function (paths are
bounded by the request type's field closure with `[*]` collapsing recursion), so
the fixpoint terminates.

### 4.3 Propagation rules (intra-function, over SSA)
- `Parameter(in)` -> seed label `{(in, read)}`.
- `FieldAddr`/`Field v, #k` -> extend each path of v with `.fieldName(#k)`.
- `IndexAddr`/`Index`/map lookup on tainted v -> append `[*]`.
- Proto getter `Call v.GetX()` -> treat as `Field` read of `X`.
- `UnOp *p` (deref), `ChangeType`, `Convert`, `Phi` -> propagate join of operands.
- `BinOp`, `Call` to pure helpers -> result is `derived` with union of operand
  paths.
- `If cond` where `cond` is tainted -> emit a Branch record (section 6) and tag
  the condition paths `predicate`.
- Store to a response-message field with tainted value -> emit a response-write
  edge (section 6).

## 5. Interprocedural propagation via summaries

To scale to 47K LOC x 193 entrypoints we use compositional summaries (Infer
pattern), not whole-program re-analysis per entrypoint.

A function summary is computed once per SSA function:

```
Summary(f) = {
  paramTaintToReturn:  map[paramIndex] -> {resultPaths}   // how inputs reach results
  paramTaintToSinks:   list of (sinkKind, sinkId, viaParams)  // DB/RPC/error reached
  paramTaintToBranches:list of branch obligations keyed by param
  calleeRefs:          resolved callees (VTA) for further linking
}
```

At a call site, the caller substitutes its own access-path labels for the
callee's `paramIndex` placeholders, producing caller-rooted provenance. Dispatch
targets come from VTA (deterministic). Recursion/cycles use a standard
fixpoint over the summary lattice (iterate to stable, bounded by finite path
sets). This makes the per-entrypoint walk linear in reachable summaries.

UNRESOLVED handling (determinism-preserving):
- If VTA yields more than one dynamic target for a dispatch, include all (sound
  over-approximation) and mark the branch set `dispatch=multi`.
- If a target is in an external module whose SSA is not loaded (e.g. deep in a
  driver), record an `opaque` sink with the callee symbol and stop. We never
  silently drop taint.

## 6. What a provenance record contains

Per RPC entrypoint, emit:

```json
{
  "rpc": "Janus.GetDevOrg",
  "request_type": "api.GetDevOrgRequest",
  "response_type": "api.GetDevOrgResponse",
  "input_paths": [
    {"path": "in.DevOid", "kinds": ["read","predicate"]}
  ],
  "branches": [
    {
      "id": "b1",
      "at": "internal/service/devorg.go:1793",
      "predicate_paths": ["in.DevOid (derived devOID)"],
      "classes": [
        {"cond": "devOID == \"\"",      "outcome": {"error": "errInvalidJWTToken"}},
        {"cond": "devOID != \"\"",      "outcome": {"continue": true}}
      ]
    },
    {
      "id": "b2",
      "at": "internal/service/devorg.go:1815",
      "predicate_paths": ["in.DevOid", "ctx.userContext.DevOID"],
      "classes": [
        {"cond": "userCtx.DevOID != devOID",
         "outcome": {"response_fields": ["DevSlug","DisplayName","DisplayId"]}},
        {"cond": "userCtx.DevOID == devOID",
         "outcome": {"response_fields": ["<full DevOrg>"]}}
      ]
    }
  ],
  "sinks": [
    {"kind": "db_read",  "target": "collection.DevOrg via readDevOrgWithCache",
     "via_paths": ["in.DevOid"], "at": "internal/service/devorg.go:1803"}
  ],
  "errors": [
    {"symbol": "errInvalidJWTToken", "grpc_code": "InvalidArgument?",
     "gated_by": ["b1.class[devOID==\"\"]"]}
  ],
  "unresolved": []
}
```

`branches[].classes` is the key artifact: it is the enumerated set of input
equivalence classes with their distinguishable outcomes. For `GetDevOrg` the
cross-product reduces to three realizable test scenarios:
DevOid empty -> error, DevOid == own org -> full response, DevOid == other org
-> stripped response. That is the deterministic test-obligation set for the RPC.

`sinks` + `errors` + response-field writes give the provenance (which input
reaches which effect), which is what change-impact (Goal C) consumes.

## 7. Goal A: coverage analysis (primary)

### 7.1 Test facts (from the easy layer)
For each lifejacket test mapped to this RPC's operation we already can extract,
from the Ginkgo test AST and generated client types:
- `set_fields`: request access-paths the test populates (struct literal keys,
  including values when literal, e.g. `Toggles: []string{validToggle}`).
- `set_values`: concrete/enumerable values set per path (for parameter-value and
  equivalence-class matching).
- `asserted_status`: `Expect(response.StatusCode).To(Equal(200))`.
- `asserted_response_paths`: `Expect(res.Toggles)...` -> `response.Toggles`.

### 7.2 Mapping test -> provenance
For each test and each branch of the RPC:
- A branch class is COVERED if the test's `set_values` satisfy the class
  condition (literal evaluation) OR, when values are not statically known, if
  the test's `set_fields` include the branch's `predicate_paths` AND the
  asserted outcome (status or response fields) matches exactly one class'
  outcome. The outcome match disambiguates which class was taken without needing
  to execute.
- A response field is COVERED if some covering test asserts it.
- A status/error is COVERED if some test asserts that status and reaches a class
  whose outcome is that error.

### 7.3 Coverage output (per RPC)
```json
{
  "rpc": "Janus.GetDevOrg",
  "operation": "GET /internal/dev-orgs.get",     // from the easy layer
  "metrics": {
    "operation_covered": true,
    "input_paths": {"covered": 1, "total": 1},
    "branch_classes": {"covered": 2, "total": 3},
    "response_fields": {"covered": 3, "total": 3},
    "statuses": {"covered": 2, "total": 3}
  },
  "covered": [
    {"branch": "b2", "class": "userCtx.DevOID != devOID",
     "by_tests": ["experience_toggles_get_test.go:...external org case"]}
  ],
  "uncovered": [
    {"branch": "b1", "class": "devOID == \"\"",
     "expected": {"error": "errInvalidJWTToken"},
     "reason": "no mapped test leaves in.DevOid empty with empty user-context org"}
  ]
}
```

Metrics are reported against REST coverage criteria (operation, parameter,
parameter-value, status) AND branch-class coverage (the implementation-level
view the provenance uniquely enables). The two are reported separately, per the
coverage-criteria literature.

### 7.4 Determinism and soundness for coverage
- COVERED is asserted only when provably satisfied (literal value match) or when
  predicate-paths are set and the asserted outcome uniquely identifies one
  class. Otherwise the class is UNCOVERED (conservative, never over-claims
  coverage).
- Branches behind `dispatch=multi` or `opaque` sinks are reported with a
  `low_confidence` flag so a human/agent can inspect, never silently counted as
  covered.

## 8. Goal B (sketch): agent gap records

Each `uncovered` branch class is already an actionable obligation: it carries
the input condition (which request paths and the predicate), a synthesizable
example payload (request type + the class condition + any `set_values` seen in
sibling tests), and the expected outcome (status/error/response fields). The
agent layer wraps this with repo conventions (test file location sibling to the
operation's existing test, `httputils.MakeRequest` + `authClient` idiom, Ginkgo
`It` + labels/owners header) and runs the propose -> lifejacket-run -> refine ->
PR loop. The provenance guarantees the agent targets a real, reachable code path
with a known expected result rather than guessing.

## 9. Goal C (sketch): deterministic test skipping

From the same SSA + callgraph:
- Build the reverse interprocedural callgraph (callee -> callers), VTA-resolved.
- `git diff` on the service -> changed SSA functions and changed branch sites.
- For each changed function/branch, walk reverse edges to the RPC entrypoints
  that reach it, and intersect with the specific input-paths/branch-classes
  whose provenance includes the change.
- Map affected branch-classes -> lifejacket tests covering them (section 7.2).
- SKIP = all mapped tests MINUS affected tests, but only when every path to the
  change is VTA-resolved. Where dispatch is `multi`/`opaque`, fall back to
  including those tests (the honest `run_full_suite`-style guard, but scoped to
  the unresolved subset instead of the whole suite).

This is fully deterministic: reachability plus the field-sensitive provenance,
no statistical model.

## 10. Build context and reproducibility
Pin and record: service git revision, resolved module graph (go.mod/go.sum of
service + relevant clients), analyzer version, and build context
(GOOS/GOARCH/tags). Emit a combined `analysis_fingerprint` so a given fingerprint
always implies the same provenance and coverage JSON. Alternate build tags
produce a distinct fingerprint, as in the existing tool.

## 11. Validation plan (before generalizing)
Hand-verified oracles already established:
1. `GetDevOrg`: expect 1 input path, 2 branches, 3 realizable classes, 1 db_read
   sink, 1 error. Coverage must mark the empty-DevOid error class UNCOVERED
   unless a mapped test exercises it.
2. `CreateAccount` via `CreateAccountValidator.Validate`: expect input paths
   `in.Account.{DisplayName,Websites,Domains,ExternalRefs}` and
   `in.UniqueDisplayNameSuffix`, a db_read uniqueness sink, per-field error
   outcomes, reached through interface dispatch + type assertion (proves VTA +
   assertion handling).
Only after these two match the hand oracles do we run across all entrypoints.

## 12. References
Same set as docs/coverage-via-callgraphs-design.md. Most load-bearing here:
Meta Infer (compositional summaries), VTA (Tip et al.), REST API coverage
criteria (Martin-Lopez et al. A-TEST 2019), and the honest conservative-skip
stance already encoded in the existing impact.py.
