# Hexagon CI

Source-derived per-test call graph builder. Python uses the standard-library AST. Go uses a small helper built with the Go standard library; both feed the same full-rebuild graph engine.

Run a full rebuild from this checkout:

```sh
python3 -m test_call_graph /path/to/python/repository --output /path/to/graph.json
python3 -m test_call_graph /path/to/go/repository --language go --output /path/to/go-graph.json
python3 -m test_call_graph /path/to/go/repository --language go --go-tags alt --output /path/to/go-graph.json.gz
```

Go mode requires a `go` executable on `PATH`. It invokes `go run` by default; for repeated rebuilds, compile the helper once and pass the binary to avoid compilation time:

```sh
go build -o /tmp/go-ast-helper test_call_graph/go_source/main.go
python3 -m test_call_graph /path/to/go/repository --language go --go-helper /tmp/go-ast-helper --output /path/to/go-graph.json
```

Each invocation scans all supported source files (excluding common generated/dependency directories), then traverses all functions reachable from each discovered test. Python resolves simple same-module, imported-function, module-qualified, and `self`/`cls` method calls. Go resolves direct same-package and repository-local imported functions, and some receiver calls where a parameter, local initializer, or repository-local constructor identifies the concrete type. Go tests are `Test...` functions in `_test.go` with a `*testing.T` argument. Format version 2 contains function nodes, resolved edges, per-test subgraphs, unresolved calls, and, for Go, selected-file package/import metadata. Node IDs use repository-relative file paths and qualified function names. `source_fingerprint` hashes scanned repository bytes; `analysis_fingerprint` also includes the analyzer and Go build context. An output ending in `.gz` is deterministically gzip-compressed and atomically replaced.

Adding, removing, or changing source or tests takes effect on the next run. The output is replaced only after a complete rebuild; parse or source-change errors leave the previous JSON file intact. The caller should treat that previous file as stale after a failed run. A full rebuild does not require a persistent graph index.

This is static reachability, not a runtime execution trace. Python dynamic dispatch, local imports, framework-invoked fixtures, decorators, and calls through variables may remain unresolved. Go interface dispatch, function values, external modules, unknown receiver types, callbacks, and package initialization can remain unresolved. Shadowed names without a unique target remain unresolved. The reported Go file count is the number discovered; the `files` list contains those selected by the Go build context. Pass custom build tags with `--go-tags` (comma-separated). The analyzer rejects nonempty effective `GOFLAGS` rather than silently ignoring build flags. Calls inside Go function literals are attributed to the enclosing named function. Non-call dependencies such as constants in another file have no function node, but selected-file package imports allow a broader candidate list. These limits mean a large-repository graph is a partial static approximation rather than proof of runtime coverage.

The safe planning API deliberately does **not** authorize selective execution:

```python
from test_call_graph import plan_file_changes

plan = plan_file_changes(current_graph, ["lib/config.go"], previous_graph=previous_graph)
print(plan.candidate_tests)  # diagnostic package/call-graph ancestry
assert plan.run_full_suite  # run the complete applicable test suite
```

The candidate list includes tests in the changed file's package and reverse repository-local importers, including dependencies with no function node; previous snapshots cover deleted files. `run_full_suite` remains true because this static graph cannot prove that other tests are unaffected. A future test runner must honor that field; this builder does not execute tests. Changes outside selected Go sources or untracked runtime dependencies also require a full-suite run.

See [the Prometheus benchmark and sanity audit](docs/prometheus-go-benchmark.md) for observed edges, deterministic rebuilds, and concrete missed-dependency cases.

Run the focused tests with `python3 -m unittest discover -s tests -v`.

## Per-RPC provenance and coverage (Go)

`provenance/` is a separate, deterministic analyzer that extracts per-RPC
request-field provenance (inputs, branches, DB/RPC sinks, error exits) from a Go
backend service via typed SSA, then intersects it with lifejacket Ginkgo tests to
report field/branch coverage percentages, gaps, and HTTP-vs-RPC field anomalies.

```sh
cd provenance
# provenance only
go run . -dir /Users/devrev/janus -only GetDevOrg,CreateAccount
# coverage vs lifejacket
go run . -dir /Users/devrev/janus -only CreateAccount \
  -cover /Users/devrev/lifejacket/testcases/api/v2_internal/accounts
```

Full run guide: [docs/running.md](docs/running.md). Design: [docs/rpc-provenance-coverage-design.md](docs/rpc-provenance-coverage-design.md).
