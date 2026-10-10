# provenance

Deterministic per-RPC input-path and provenance extractor for Go backend
services, built on `go/packages` + `go/ssa`. It is the hard core described in
`../docs/rpc-provenance-coverage-design.md`: for each RPC entrypoint it seeds
field-sensitive taint at the request message and reports the request access
paths that are read, the branches they gate, the DB/RPC sinks they reach, and
the error exits they drive.

## Run

```sh
go run . -dir /path/to/service [flags]
```

Example (janus, two RPCs):

```sh
go run . -dir /Users/devrev/janus -only GetDevOrg,CreateAccount
```

Flags:

- `-dir`         target module directory to analyze (default `.`)
- `-patterns`    comma-separated package patterns (default `./internal/service/...`)
- `-iface-pkg`   gRPC server interface package (default `github.com/devrev/janus-client/api`)
- `-iface`       server interface name (default `JanusServer`)
- `-impl-pkg`    concrete server package (default `github.com/devrev/janus/internal/service`)
- `-impl`        concrete server type (default `Janus`)
- `-only`        comma-separated RPC names to analyze (default: all)
- `-out`         output file (default stdout)
- `PROV_DEBUG=1` env: trace followed functions and sink candidates on stderr

Dependencies are loaded types-only (export data); SSA bodies are built for the
service packages. Proto getters are canonicalized as field reads rather than
followed. Output is deterministic for a fixed source + build context.

## Coverage mode (Goal A)

Intersect per-RPC provenance with lifejacket Ginkgo tests to see which request
fields and validation-branch inputs the tests actually exercise, and which the
RPC reads but no test touches (the gaps an agent should fill).

```sh
go run . -dir /Users/devrev/janus -only CreateAccount \
  -cover /Users/devrev/lifejacket/testcases/api/v2_internal/accounts
```

- `-cover <dir>`  lifejacket testcases dir; switches output to a coverage report
- `-op-map`       operation->RPC overrides, e.g. `AccountsCreate=CreateAccount`

How it works (all syntactic, deterministic):

1. Parse every `*_test.go` for Ginkgo `It` blocks that issue a request. For each
   we extract the operation (`Client*.<Op>` in `MakeRequest`), the request body
   fields set (`<Op>JSONRequestBody{...}` keys), asserted statuses
   (`StatusCode == N`, or `BeNil()` success treated as 200), and labels.
2. Map operation -> RPC (verb-last reorder + resource singularization, e.g.
   `AccountsCreate` -> `CreateAccount`), overridable with `-op-map`.
3. Join by request-field leaf name (the gateway flattens/nests bodies, so
   `DisplayName` matches `in.Account.DisplayName`), normalizing snake_case vs
   PascalCase. Emit per RPC: field coverage percentage over leaf fields (with the
   tests that set each), field gaps, branch input-exercise percentage, branch
   gaps, asserted statuses, and `anomalies` for HTTP-body fields that match no
   RPC path (gateway renames like `Artifacts` vs `ArtifactIds`, with fuzzy
   suggestions). Resolve confirmed renames with `-field-alias`.

This is static reachability intersected with test *inputs*, not executed-branch
coverage. A captured sample is in `examples/janus-createaccount-coverage.json`.
Full run/flag/output guide: `../docs/running.md`.

## What it extracts (per RPC)

- `input_paths`: request access paths read, each tagged `read` and/or
  `predicate`. Field expansion stops at the proto-message boundary, so Go/stdlib
  internals are not reported as request fields.
- `branches`: tainted `if` conditions with rendered condition and the request
  paths that gate them. These enumerate the input equivalence classes to test.
- `sinks`: `db` / `rpc` / `dispatch` / `opaque` calls reached under taint, with
  the request paths that flow to them (provenance).
- `errors`: sentinel error returns, marked `tainted` when gated by a tainted
  branch.

## Scope and limits (current)

- Interface dispatch is resolved only for static/concrete calls and recorded as
  a `dispatch` sink otherwise. VTA-based dynamic dispatch is future work.
- Container reachability (a struct built from request fields) is tracked coarsely
  with a `~` marker so query documents reach DB/RPC sinks without polluting the
  precise request access paths.
- Constructors/converters (`New*`, `To*`, single non-error result) propagate
  taint through their result even when the body summary cannot track it exactly.
- This is static reachability, not runtime coverage. See the design doc for how
  this feeds coverage analysis against lifejacket tests (Goal A).

## Validated oracles

`GetDevOrg` and `CreateAccount` in janus. The latter exercises interface-
dispatched validation, a type assertion, deep proto getter chains, cross-package
following, and DB/RPC sink detection. A captured sample is in
`examples/janus-getdevorg-createaccount.json`.
