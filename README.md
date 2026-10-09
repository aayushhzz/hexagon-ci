# Hexagon CI

Hexagon CI is a Go CLI for building per-test source graphs, reporting tests that may be affected by file changes, and running Go tests. The graph and plan formats retain the existing graph v2 contract. The former Python implementation and its tests are preserved under [`python/`](python/) as a runnable reference.

Build the CLI and rebuild a Go repository graph:

```sh
go build -o /tmp/hexagon-ci .
/tmp/hexagon-ci graph /path/to/go/repository --output /path/to/graph.json
/tmp/hexagon-ci graph /path/to/go/repository --go-tags alt --output /path/to/graph.json.gz
```

The original positional graph invocation remains supported:

```sh
/tmp/hexagon-ci /path/to/go/repository --language go --output /path/to/graph.json
```

For an impact plan, pass the current graph, changed repository-relative paths, and (when available) the previous graph so deleted files still contribute candidates:

```sh
/tmp/hexagon-ci plan /path/to/graph.json \
  --changed-file lib/config.go \
  --previous-graph /path/to/base-graph.json \
  --output /path/to/plan.json
/tmp/hexagon-ci test /path/to/go/repository --plan /path/to/plan.json
```

The planner reports call-graph and package-import candidates, but sets `run_full_suite` to `true` and leaves `selection_complete` unset. Static analysis cannot prove that callbacks, interface dispatch, generated code, non-call dependencies, or runtime configuration have no effect on other tests. The runner uses `go test ./...` unless a plan explicitly marks selection complete and its source fingerprint, analyzer, and Go build context still match the checkout. Incomplete, stale, or mismatched plans fall back to the full suite. Build tags from graph analysis are carried into test execution. Go environment flags are rejected unless expressed with `--go-tags`, so the analyzed and executed build contexts stay explicit.

Graph v2 includes function nodes, resolved call edges, per-test subgraphs, unresolved calls, selected Go file package/import metadata, source fingerprints, and build context. Readers tolerate additional JSON fields while rejecting unsupported graph versions or languages. Plans use format version 1; the runner rejects an unknown plan version rather than guessing how to execute it. Graph writes are atomic, gzip output is deterministic, and parse/source-change failures leave the previous output intact.

The Go analyzer uses the Go standard library and does not type-check or execute repository code while building the graph. Interface dispatch, function values, callbacks, external modules, unknown receiver types, and package initialization can remain unresolved. Candidate tests are diagnostic until dependency completeness is established, so full-suite fallback remains the safe default.

The Python implementation is retained for reference and parity checks. To run it directly from the repository root:

```sh
PYTHONPATH=python python3 -m test_call_graph /path/to/repository --language go --output /tmp/python-graph.json
PYTHONPATH=python python3 -m unittest discover -s python/tests -v
```

The archived planner and runner from the earlier Python implementation remain under `python/test_call_graph` and `python/tests`. The graph v2 Python reference used for native-Go parity checks is under `python/graph_v2_reference`.

Run the Go suite and static checks with:

```sh
go test ./...
go vet ./...
```

The Go integration tests run the Python reference against shared Go fixtures and compare graph v2 and selector behavior. See [the Prometheus benchmark and sanity audit](docs/prometheus-go-benchmark.md) for observed edges, deterministic rebuilds, and concrete missed-dependency cases.
