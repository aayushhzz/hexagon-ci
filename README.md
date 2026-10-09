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

Each invocation scans all supported source files (excluding common generated/dependency directories), then traverses all functions reachable from each discovered test. Python resolves simple same-module, imported-function, module-qualified, and `self`/`cls` method calls. Go resolves direct same-package and repository-local imported functions, and some receiver calls where a parameter, local initializer, or repository-local constructor identifies the concrete type. Go tests are `Test...` functions in `_test.go` with a `*testing.T` argument. Format version 3 contains function nodes with source hashes, resolved edges, per-test subgraphs, unresolved calls, and per-file fingerprints for non-function inputs; Go graphs also include selected-file package/import metadata. Node IDs use repository-relative file paths and qualified function names. `source_fingerprint` hashes scanned repository bytes; `analysis_fingerprint` also includes the analyzer and Go build context. An output ending in `.gz` is deterministically gzip-compressed and atomically replaced.

Adding, removing, or changing source or tests takes effect on the next run. The output is replaced only after a complete rebuild; parse or source-change errors leave the previous JSON file intact. The caller should treat that previous file as stale after a failed run. A full rebuild does not require a persistent graph index.

This is static reachability, not a runtime execution trace. Python dynamic dispatch, local imports, framework-invoked fixtures, decorators, and calls through variables may remain unresolved. Go interface dispatch, function values, external modules, unknown receiver types, callbacks, and package initialization can remain unresolved. Shadowed names without a unique target remain unresolved. The reported Go file count is the number discovered; the `files` list contains those selected by the Go build context. Pass custom build tags with `--go-tags` (comma-separated). The analyzer rejects nonempty effective `GOFLAGS` rather than silently ignoring build flags. Calls inside Go function literals are attributed to the enclosing named function. Non-call dependencies such as constants in another file have no function node, but selected-file package imports allow a broader candidate list. These limits mean a large-repository graph is a partial static approximation rather than proof of runtime coverage.

The change-impact API compares base and head snapshots to find added, removed, and modified functions, including body-only edits that do not change call edges:

```python
from test_call_graph import plan_file_changes

plan = plan_file_changes(current_graph, ["lib/leaf.go"], previous_graph=previous_graph)
print(plan.changed_functions)
print(plan.candidate_tests)
print(plan.run_full_suite)  # true means the consumer must run the complete suite
```

Given base and head graph files plus a newline-separated changed-path file, the planner CLI emits a versioned JSON plan grouped by repository-relative test file. It can also append the same maps to a GitHub Actions step summary:

```sh
python3 -m test_call_graph.plan_cli \
  --current-graph /tmp/head-graph.json \
  --previous-graph /tmp/base-graph.json \
  --changed-files /tmp/changed-files.txt \
  --summary-file /tmp/test-impact-summary.md
```

The plan contains `tests_to_run` and `tests_to_skip` objects whose keys are test-file paths and whose values are arrays of test names. `candidate_tests_by_file` preserves the analyzer's affected-test candidates for diagnosis. When `run_full_suite` is true, every recognized test root is placed under `tests_to_run`, `tests_to_skip` is empty, and consumers must execute the full suite. The `CI` workflow builds base and head snapshots for each pull request, publishes this JSON as the `test-impact-plan` artifact, and displays both maps in the Actions summary. Its unit-test job downloads the artifact and passes it to `test_call_graph.plan_runner`.

The runner currently executes selective Python `unittest` plans only. Before selecting cases, it verifies that every discovered unittest appears exactly once across the run and skip maps and that each file and test name can safely resolve. Missing, malformed, unsafe, incomplete, unsupported, or full-suite plans all execute the full suite. Run it locally with `python3 -m test_call_graph.plan_runner --root /path/to/repository --plan /path/to/test-impact-plan.json`. Python graph selection currently requires the full-suite fallback because framework fixtures and import-time execution are not modeled; the JSON still shows the recognized test roots grouped by file, while the separate candidate map remains diagnostic.

For Go, a selective plan is allowed only when both snapshots use format version 3 and the same analyzer/build context, the supplied changed paths account for the graph delta, and non-function source fingerprints are unchanged. Test roots with unresolved reachable calls are included conservatively. Changes to non-function inputs, package initialization or variable initialization, bodyless declarations, `linkname`/`export` directives, `TestMain`, unsupported Go test roots, or changes without a prior snapshot fall back to the full suite. Python currently always falls back because framework fixtures and import-time execution are not modeled. The older file/package candidate analysis remains available in fallback plans, including reverse repository-local Go imports for dependencies with no function node. The planner emits plans; consumers must honor `run_full_suite` and execute the full suite whenever it is true.

See [the Prometheus benchmark and sanity audit](docs/prometheus-go-benchmark.md) for observed edges, deterministic rebuilds, and concrete missed-dependency cases.

Run the focused tests with `python3 -m unittest discover -s tests -v`.
