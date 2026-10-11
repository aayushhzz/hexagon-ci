# Local DevRev function graph explorer

This dependency-free page loads Hexagon CI graph-v2 snapshots on demand and decompresses `.json.gz` files in your browser. Graph data stays in this local workspace; the page does not upload it. The library starts with the requested repositories available as local Go checkouts, then lists graph snapshots built from the Go modules under `/Users/devrev/Desktop/DevRev`.

From the repository root, start the local site:

```sh
cd site/test-graphs
/usr/bin/python3 -m http.server 4173
```

Open <http://localhost:4173>. Pick a repository, then search for a function in **Function view**. Each view places that function between its direct callers and callees and includes related test roots above it; dashed orange links mark test coverage. To keep dense views readable, large function views start with 24 tests and let you expand to all related tests. Large repositories open on a representative function view. Click another function to center a new view, or choose **All functions** to see the full repository graph. Drag to pan and scroll to zoom.

Large snapshots also have compact browser copies in `data/web`. These preserve the main call graph and test-to-function membership while replacing each test's duplicate call list with an edge count and omitting its repeated unresolved-call list. The two Gateway snapshots also omit the huge graph-level unresolved-call table; the inspector labels that detail as omitted and retains the total count. Original builder snapshots remain alongside these compact copies.

To rebuild a local snapshot, run the graph builder from the repository root and write the compressed output into `site/test-graphs/data`:

```sh
/usr/local/go/bin/go run . graph /path/to/repository --output /tmp/repository.json
/usr/bin/gzip -c /tmp/repository.json > site/test-graphs/data/repository.json.gz
```

Then add an entry for `./data/repository.json.gz` to `data/catalog.json`, including its repository name, commit, Go-file count, and test count. The two small `inventory` and `event-router` snapshots are synthetic demo fixtures and appear after the repository snapshots.
