# Go and Python graph-build benchmark (2026-10-10)

## Result

On this Apple Silicon Mac (12 logical CPUs, 16 GiB RAM), the native Go CLI built each graph faster than the Python graph-v2 reference. The measured Go speedup ranged from **2.43× to 2.57×** across the three paired repository measurements.

| Repository and revision | Go files found / selected | Test graphs | Go time | Python time | Go speedup | JSON size, Go / Python |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| [Prometheus](https://github.com/prometheus/prometheus), `2213c3c192574b8a339acd336ea6385974a7567b` | 736 / 696 | 2,074 | 5.29 s | 13.11 s | 2.48× | 205.74 / 205.74 MB |
| [Grafana](https://github.com/grafana/grafana), `df53a3d5f66602b584c4dbfae012608d34f35a21` | 6,679 / 6,647 | 9,986 | 74.59 s | 181.20 s | 2.43× | 1,366.26 / 1,366.11 MB |
| [Kubernetes](https://github.com/kubernetes/kubernetes), `a5081d965140b4ee8311bea358f1afd79b840677` | 13,555 / 13,189 | 12,735 | 143.20 s | 368.12 s | 2.57× | 876.87 / 876.81 MB |

The Go/Python outputs had matching counts for top-level function nodes and resolved edges: Prometheus 9,786 / 17,876; Grafana 52,066 / 75,303; Kubernetes 112,862 / 116,354. Both commands reported the same discovered Go-file and test-graph counts for each checkout. The JSON byte sizes differ slightly; these checks establish matching aggregate counts, not byte-for-byte graph equivalence.

## Method

- Each repository was a shallow public GitHub checkout pinned to the revision in the table. The measured command rebuilt a plain, uncompressed graph from that local checkout; clone time was excluded.
- Timings are one complete run per implementation and repository, measured with `/usr/bin/time -lp`. They include source collection, parsing, graph construction, and JSON writing. The CLI/helper compilation was done once before timing and excluded. Builds ran serially on Go 1.24.3 and Python 3.14.7; Hexagon CI was at `b6edd1a`.
- The Python run used `python/graph_v2_reference` and a precompiled Go AST helper. That reference passes Go sources to the helper for AST parsing, then constructs and serializes the per-test graph in Python. This isolates graph-build work from repeated helper compilation; it is not a pure-Python Go parser.
- The Go builder found all `.go` files except its ignored directories, then selected the files matching the host build context (`darwin/arm64`, default tags). No repository tests were run.

Example commands from the repository root:

```sh
go build -o /tmp/hexagon-ci .
/tmp/hexagon-ci graph /path/to/repository --output /tmp/graph.json

go build -o /tmp/go-ast-helper python/graph_v2_reference/test_call_graph/go_source/main.go
PYTHONPATH=python/graph_v2_reference python3 -m test_call_graph /path/to/repository \
  --language go --go-helper /tmp/go-ast-helper --output /tmp/python-graph.json
```

These are local single-run measurements, not a CI prediction or a cold-cache guarantee. They compare graph-building speed, not graph completeness: the analyzers use Go AST parsing without type-checking or executing repository code, so unresolved calls and build-context limits remain.

## Latest worktree implementation rerun (2026-10-10)

The worktree changed while the first rerun was in progress, so the final comparison below uses a fresh binary built after those changes. The baseline is the committed source at `b6edd1aa856a3345400b329f4486f088df08c988`; the latest binary is from the then-current uncommitted worktree snapshot and has SHA-256 `92ab6d09784baa93cfdf1a16ed6f8fc751e13ba865e4ed3ee4a1ae95140ac3b1`. The baseline binary SHA-256 is `6e07f466392fbc98e54c2c499f1745e08dc5f4a4ef32842b67dbc87a485a8c81`.

| Repository and revision | Discovered Go files / test graphs | Baseline Go median (3 runs) | Latest Go median (3 runs) | Median paired speedup | Python reference (1 run) | Latest Go faster than Python |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| [Prometheus](https://github.com/prometheus/prometheus), `2213c3c192574b8a339acd336ea6385974a7567b` | 736 / 2,074 | 4.44 s | 1.88 s | 2.36× | 14.36 s | 7.64× |
| [Grafana](https://github.com/grafana/grafana), `df53a3d5f66602b584c4dbfae012608d34f35a21` | 6,679 / 9,986 | 74.08 s | 12.56 s | 6.02× | 190.32 s | 15.15× |
| [Kubernetes](https://github.com/kubernetes/kubernetes), `a5081d965140b4ee8311bea358f1afd79b840677` | 13,555 / 12,735 | 144.81 s | 16.15 s | 9.08× | 384.81 s | 23.83× |

The Go speedup is the median of the three paired speed ratios; the Python comparison uses one fresh run per repository. Peak RSS from `/usr/bin/time -lp` was variable, so the table below includes the three-run median and observed range for Go. Values are MiB.

| Repository | Baseline Go median RSS | Latest Go median RSS (range) | Python RSS (one run) |
| --- | ---: | ---: | ---: |
| Prometheus | 1,043.7 | 1,047.4 (1,033.1–1,067.4) | 275.9 |
| Grafana | 2,667.7 | 4,577.2 (2,055.3–5,350.0) | 1,191.5 |
| Kubernetes | 3,879.2 | 4,519.8 (2,779.0–4,541.0) | 2,172.2 |

For every baseline/latest Go pair, the complete graph-v2 JSON had the same SHA-256 after replacing only `analyzer_sha256` and `analysis_fingerprint`, which are expected to change with analyzer source. The matching normalized hashes were Prometheus `923fb143cd2528510a8f1ccced19a7f5eb2ec511ce6629a3ae9fe9198f4d815b`, Grafana `89f92236b589146986f44393535095b160bae931241e4d616f726081a008a0c9`, and Kubernetes `b1dc9637528a740476331f30bc985c41a18253b384f80597cf832b4fc7457b9c`. Baseline and latest Go outputs had identical byte sizes per repository: 205,743,915; 1,366,258,942; and 876,874,133 bytes, respectively. The Python output sizes were 205,735,525; 1,366,106,465; and 876,810,953 bytes; matching sizes or aggregate counts are not a byte-equivalence claim for Python.

The baseline/latest runs used serial paired order `baseline → latest`, `latest → baseline`, then `baseline → latest`. Python used `python/graph_v2_reference` with its Go AST helper precompiled. All binaries and the helper were built before timing. Each measured graph command includes source scanning, AST parsing, graph construction, and output writing; repository clones and compilation are excluded. Runs used Go 1.24.3 and Python 3.14.7 on Darwin/arm64 with 12 logical CPUs and 16 GiB RAM. Filesystem cache was local and warm; these are not cold-cache CI estimates. `go test ./...` passed on the latest source snapshot.

The graph-v2 outputs remaining identical confirms that the latest internal numeric traversal and writer changes preserve the measured graph data for these three snapshots. It does not increase the analyzer's resolution coverage or prove completeness beyond its existing static-analysis limits.
