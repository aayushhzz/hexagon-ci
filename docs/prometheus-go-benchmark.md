# Prometheus Go call-graph rebuild (2026-10-09)

This is a benchmark and source-level spot check of the experimental Go adapter in this worktree. It is not a measurement of a type-checked Go analyzer or of runtime test coverage.

## Input and reproducibility

- Repository: `github.com/prometheus/prometheus`, shallow checkout at commit `2213c3c192574b8a339acd336ea6385974a7567b` under `/tmp/callgraph-go-PO8VYh/prometheus`.
- The checkout contains approximately 40 MiB of files. The builder discovered 736 `.go` files; it selects default-build-compatible files for parsing, so 736 is a discovery count rather than the number of files actually parsed. The repository's `go.mod` declares Go 1.26.7. The helper used for these measurements was built with the local Go 1.24.3 toolchain from this worktree. No Prometheus tests were run.
- Builder output: `/tmp/callgraph-go-PO8VYh/prometheus-graph.json`, source fingerprint `a8bb03e6a127ac24eb286b4dc8d81c652e79e512421d1b72767087a02c4c367c`. The fingerprint covers repository source bytes, not the analyzer version or Go build context.

From this worktree, the measured prebuilt-helper invocation was:

```sh
go build -o /tmp/callgraph-go-PO8VYh/go-ast-helper test_call_graph/go_source/main.go
python3 -m test_call_graph /tmp/callgraph-go-PO8VYh/prometheus \
  --language go --go-helper /tmp/callgraph-go-PO8VYh/go-ast-helper \
  --output /tmp/callgraph-go-PO8VYh/prometheus-graph.json
```

## Measurement

Three complete CLI rebuilds with the precompiled helper took **13.588, 12.371, and 12.280 seconds** wall time (median **12.371 seconds**). A separate CLI invocation using the default `go run` helper took **14.22 seconds** wall time. Each timed command includes source scanning, parsing, graph construction, and JSON serialization/write. It excludes cloning and the separate prebuilt-helper compilation. Times are local measurements, not a CI prediction.

The initial JSON was **108,728,905 bytes** (about 104 MiB; `du -h` reported 113 MiB allocated). It contained 2,074 test-rooted graphs, 9,786 function nodes, 13,464 distinct resolved edges, 23,687 resolved call sites, and 93,170 unresolved call sites. Per-test graphs repeat reachable node IDs and unresolved-call details, which contributes to output size. The unresolved count includes builtins and external calls and therefore is **not** the number of missed internal edges.

## Correctness spot checks

The graph records the following calls at the matching source lines in this checkout:

| Source call | Resolved edge |
| --- | --- |
| `promql/engine_test.go:61` | `TestQueryConcurrency → promql/query_logger.go::NewActiveQueryTracker` |
| `promql/engine_test.go:70` | `TestQueryConcurrency → promql/promqltest/test.go::NewTestEngineWithOpts` |
| `promql/promqltest/test.go:123` | `NewTestEngineWithOpts → promql/engine.go::NewEngine` |
| `model/labels/sharding_test.go:30` | `TestStableHash → model/labels/sharding_stringlabels.go::StableHash` |

The `StableHash` target is the source selected by the default `!slicelabels && !dedupelabels` build constraint. The alternative `slicelabels` implementation is not in this graph.

There are also confirmed omissions: `engine.NewTestQuery` at `promql/engine_test.go:97` and `:109` is recorded as unresolved even though `engine` is a local `*promql.Engine`; `NewSubsequenceMatcher(tt.pattern).Score` at `util/strutil/subsequence_test.go:268` is unresolved as a chained method call. The builder does not type-check ordinary variable receivers or resolve all function-value, interface, external-module, and callback calls. A few source-verified edges establish that those paths were represented, but do not establish graph-wide precision or recall. Default build constraints alone also do not represent every custom-tag configuration.

The 12 focused builder tests cover small Go and Python repositories, including an inserted/deleted dependency, default-tag selection, and preserving an existing output on parse failure. They do not prove the large-repository graph globally correct. For test-impact selection, an unresolved internal edge needs conservative fallback or further type-aware analysis before skipping tests safely.

## Follow-up sanity audit (2026-10-09)

Additional Go fixtures tested repeat-run byte identity, two tests sharing a transitive dependency, deletion of a dependency file, local/parameter shadowing, ordinary receiver calls, and a constant-only dependency file. Shadowing exposed false call edges: a `Target` function parameter or local variable was incorrectly resolved to a same-name package function. The helper now marks such ambiguous calls unresolved rather than inventing an edge. These tests also show that changing `lib/config.go` containing only `const Value` can change a reachable function's behavior without adding a call-graph node for that file. A reverse walk over function nodes alone would miss its test.

After the shadowing fix, two full Prometheus rebuilds with the precompiled helper took **10.15 and 9.67 seconds** wall time on the warmed local checkout. Both wrote the same SHA-256 JSON hash `86085d7397f0f0a19c346d718866ccda271986f37cd4870bb1d17`. The updated snapshot is **105,562,707 bytes** (about 101 MiB) with 2,074 test graphs, 9,786 nodes, 13,385 resolved edges, 23,498 resolved call sites, and 93,359 unresolved call sites. These follow-up timings are not directly comparable to the initial three-run median as a controlled speed comparison.

The source fingerprint did not change between analyzer versions although the graph did. Reproducible builds must pin the analyzer binary/version and Go build context as well as the source revision. Identical output for a fixed input is determinism; it does not prove that the graph includes every true dependency. The follow-up implementation adds a separate analysis fingerprint and a conservative change-impact plan; see below.

## Receiver resolution and safe change planning (2026-10-09)

The updated AST helper resolves the two known Prometheus omissions: `TestQueryConcurrency → Engine.NewTestQuery` at `promql/engine_test.go:97,109`, and `TestSubsequenceScore → SubsequenceMatcher.Score` for the chained constructor call at `util/strutil/subsequence_test.go:268`. This uses known parameter types, local initializers, and repository-local function return types; it is not a full Go type checker. Dynamic dispatch and unknown/external code still remain outside its reach.

The graph now records the effective Go build context, analyzer hash, an `analysis_fingerprint`, and selected-file package/import metadata. Explicit `--go-tags` selects alternate build variants; unsupported `GOFLAGS` fail the rebuild. The `plan_file_changes` API uses current and optional prior graph snapshots to report test candidates through both call ancestry and reverse package imports. It marks `run_full_suite=True` in every case: the AST graph still cannot prove that other tests are unaffected. It is a safety plan, not an implemented test runner or a claim of global graph completeness.

One updated Prometheus rebuild took **14.01 seconds**, producing 2,074 test graphs, 9,786 function nodes, 17,876 resolved edges, and 86,370 unresolved calls. Selected-file metadata lists 696 parsed Go files out of 736 discovered. The expanded reachability repeated more facts per test and wrote **205,735,525 bytes** of plain JSON. A compressed `.json.gz` rebuild took **16.33 seconds** and wrote **10,456,895 bytes**; gzip preserves the graph schema and is deterministic for fixed input. These are single local measurements after code changes, not controlled performance comparisons with the earlier versions.

After finalizing the effective-`GOFLAGS` guard, two more compressed rebuilds took **14.86 and 14.79 seconds** and wrote **10,456,884 bytes** each. Both files had SHA-256 `74722d815c8de7e8ba2a927825eef4ccbe09a9c96d0b84b7467727e05ec9a8d7`. The input Prometheus repository was still parsed with the local Go 1.24.3 helper despite its Go 1.26.7 module declaration; the full Go test suite was not executed, so no claim of all-build-context or runtime correctness follows from this benchmark.
