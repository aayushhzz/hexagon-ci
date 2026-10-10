# Experimental compact Go graph artifact benchmark (2026-10-10)

This page records the initial bitset/gob prototype. The [implemented format and follow-up measurements](compact-go-graph-implementation.md) use sparse sorted IDs and a direct numeric builder.

Graph-v2 artifacts serialize named nodes, edges, and unresolved calls again for each test-rooted view. This historical benchmark compares an indented JSON gzip file with the **experimental** bitset format implemented in [`benchmarks/compactgraph/main.go`](../benchmarks/compactgraph/main.go). The later production format uses sparse sorted IDs and is documented in the implementation follow-up.

The compact format keeps one name table and one copy of the global edges and unresolved calls. An edge refers to dense `uint32` function IDs; each test stores its root ID and a bitset of reachable function IDs. On lookup, the bitset and global facts reconstruct the existing named `TestGraph`, then JSON encoding returns the same view. The experimental wire encoding is Go `gob` wrapped in gzip, not a stable or interoperable format contract.

| Pinned repository | Tests | JSON gzip / compact bytes | Size reduction | Build JSON / compact | Median load JSON / compact | Median 300-view query JSON / compact |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| [Prometheus](https://github.com/prometheus/prometheus) `2213c3c192574b8a339acd336ea6385974a7567b` | 2,074 | 11,638,249 / 933,915 | 12.5x | 3.07 / 0.96 s | 1,491 / 30 ms | 40 / 48 ms |
| [Grafana](https://github.com/grafana/grafana) `df53a3d5f66602b584c4dbfae012608d34f35a21` | 9,986 | 68,474,212 / 4,792,910 | 14.3x | 20.67 / 6.10 s | 9,100 / 193 ms | 112 / 131 ms |
| [Kubernetes](https://github.com/kubernetes/kubernetes) `a5081d965140b4ee8311bea358f1afd79b840677` | 12,735 | 40,197,263 / 8,298,066 | 4.8x | 20.22 / 12.76 s | 5,804 / 368 ms | 222 / 257 ms |

Build timings are one serial run on each pinned checkout, including source analysis and file writing. The Prometheus build figures above are from the final run; the Grafana and Kubernetes figures are from their first runs. Load and query figures are medians of three separate process runs. Queries cycle through the first, middle, and last lexically sorted test roots plus the test with the most reachable functions (75 queries per root), measuring lookup, view reconstruction where needed, and JSON encoding. The generated JSON byte totals matched for the paired reads: 33,457,875 (Prometheus), 96,010,125 (Grafana), and 193,419,000 (Kubernetes). Every test view, global edge, unresolved call, node, and file metadata was compared to the graph-v2 artifact on each repository, and all 24,795 test views matched.

Single-run peak process RSS from `/usr/bin/time -lp` (decimal MB) illustrates memory pressure:

| Repository | Build JSON / compact | Read plus query JSON / compact |
| --- | ---: | ---: |
| Prometheus | 1,093 / 309 MB | 527 / 48 MB |
| Grafana | 2,843 / 1,633 MB | 3,071 / 423 MB |
| Kubernetes | 4,063 / 3,366 MB | 2,090 / 1,081 MB |

This comparison isolates **artifact representation**, not a new graph-construction algorithm: `build-compact` still calls `BuildGraph`, which allocates all named per-test views first, then converts them. Its lower build time and RSS largely come from avoiding the large `json.MarshalIndent` buffer and repeated named JSON/gzip serialization. Saving names alone cannot explain the size delta: deduplicating the per-test edge and unresolved lists, and compressing reachability as bitsets, also matters. Load time includes decompression, decoding, and compact index construction; it excludes repository scanning and parser startup. Warm OS caches, Go garbage collection, single-run builds, and local machine load can affect timing. These data do not predict S3 transfer time or cold CI behavior. Both formats preserve the same static-analysis limitations.

To reproduce, compile the experimental tool with `go build -o /tmp/compactgraph ./benchmarks/compactgraph`. For each pinned checkout, run (serially):

```sh
/usr/bin/time -lp /tmp/compactgraph -mode build-v2 -repo /path/to/checkout -file /tmp/graph-v2.json.gz
/usr/bin/time -lp /tmp/compactgraph -mode build-compact -repo /path/to/checkout -file /tmp/graph.hgc.gz
/tmp/compactgraph -mode validate -named /tmp/graph-v2.json.gz -compact /tmp/graph.hgc.gz
/usr/bin/time -lp /tmp/compactgraph -mode read-v2 -file /tmp/graph-v2.json.gz -queries 300
/usr/bin/time -lp /tmp/compactgraph -mode read-compact -file /tmp/graph.hgc.gz -queries 300
```

The subsequent production implementation adds a versioned, deterministic compact artifact with explicit validation, source/build-context fingerprints, atomic writes, and a direct numeric builder. It retains graph-v2 JSON for existing consumers and selects sparse sorted ID lists rather than fixed bitsets; see the [follow-up measurements](compact-go-graph-implementation.md). The bitset prototype remains here to make the representation choice and its measurements reproducible.
