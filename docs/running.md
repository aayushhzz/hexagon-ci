# Running the provenance + coverage tooling

End-to-end guide for the per-RPC provenance analyzer and the lifejacket coverage
mapper in `provenance/`. All commands are deterministic for a fixed source tree.

## Prerequisites

- Go 1.27+ on `PATH` (`go version`).
- Local checkouts of the target service and tests. Defaults assume:
  - service: `/Users/devrev/janus`
  - tests:   `/Users/devrev/lifejacket`
- First run downloads `golang.org/x/tools`:
  ```sh
  cd hexagon-ci/provenance && go mod download
  ```

## 1. Provenance for one or more RPCs

Extract which request fields each RPC reads, the branches they gate, the DB/RPC
sinks they reach, and the error exits they drive.

```sh
cd hexagon-ci/provenance

# One RPC
go run . -dir /Users/devrev/janus -only GetDevOrg

# Several RPCs, written to a file
go run . -dir /Users/devrev/janus -only GetDevOrg,CreateAccount -out /tmp/prov.json

# All RPCs on the server interface
go run . -dir /Users/devrev/janus
```

Inspect a single RPC quickly:

```sh
go run . -dir /Users/devrev/janus -only GetDevOrg | \
  python3 -c "import sys,json;p=json.load(sys.stdin)['provenance'][0];print(p['rpc']);[print(' ',x['path'],x['kinds']) for x in p['input_paths']]"
```

Debug trace (functions followed, sink candidates) on stderr:

```sh
PROV_DEBUG=1 go run . -dir /Users/devrev/janus -only GetDevOrg 2>&1 >/dev/null | head
```

## 2. Coverage against lifejacket tests

Add `-cover <lifejacket testcases dir>` to switch from raw provenance to a
coverage report. It parses the Ginkgo tests, maps each API operation to its RPC,
and intersects test-set request fields with the RPC provenance.

```sh
go run . -dir /Users/devrev/janus -only CreateAccount \
  -cover /Users/devrev/lifejacket/testcases/api/v2_internal/accounts
```

Headline numbers:

```sh
go run . -dir /Users/devrev/janus -only CreateAccount \
  -cover /Users/devrev/lifejacket/testcases/api/v2_internal/accounts 2>/dev/null | \
python3 -c "import sys,json;r=json.load(sys.stdin)['coverage'][0];f=r['fields'];b=r['branches'];\
print('field %.1f%% (%d/%d), branch %.1f%% (%d/%d), statuses %s'%(f['coverage_pct'],f['covered_count'],f['leaf_fields_read_by_rpc'],b['coverage_pct'],b['input_exercised'],b['total'],r['statuses_asserted']))"
```

## Full-repo run (all janus RPCs)

janus exposes ~160 gRPC RPCs on the `JanusServer` interface. The analyzer builds
SSA once and analyzes every RPC in a single process, so the whole repo runs in
seconds, not one-process-per-RPC.

```sh
cd hexagon-ci/provenance
go build -o /tmp/prov .

# All RPCs, full provenance JSON (~2MB, ~20s)
/tmp/prov -dir /Users/devrev/janus -out results/janus-provenance-all.json

# All RPCs, coverage vs the entire lifejacket suite (~30s)
/tmp/prov -dir /Users/devrev/janus -cover /Users/devrev/lifejacket/testcases \
  -out results/janus-coverage-all.json
```

A ready-made script regenerates every artifact in `provenance/results/`
(deterministic, overwrites in place):

```sh
JANUS=/Users/devrev/janus LIFEJACKET=/Users/devrev/lifejacket \
  ./results/reproduce.sh
```

It writes:

- `results/janus-provenance-all.json` full provenance for all 160 RPCs
- `results/janus-coverage-all.json` full coverage report for all 160 RPCs
- `results/per-rpc/<RPC>.json` 10 representative single-RPC provenance files
- `results/SUMMARY.md` human-readable coverage table + totals

Summarize the full coverage JSON:

```sh
python3 -c "import json;c=json.load(open('results/janus-coverage-all.json'))['coverage'];\
m=[r for r in c if not r.get('unmapped')];\
tl=sum(r['fields']['leaf_fields_read_by_rpc'] for r in m);tc=sum(r['fields']['covered_count'] for r in m);\
print('mapped %d/%d, aggregate field cov %.1f%%'%(len(m),len(c),100*tc/tl))"
```

### Reading the full-repo result

- Only RPCs whose operation appears in the lifejacket suite are `mapped`; the
  rest are `unmapped: true` (internal/admin RPCs not exposed through the tested
  gateway surface, or operation names the verb+resource derivation does not
  cover). The op->RPC derivation is RPC-set-aware and tries both singular and
  plural resource forms (janus uses `CreateAccount` but `ListAccounts`).
- Identifier RPCs (Get/Delete/Count) take a structured identifier (e.g.
  `GetAccountRequest.AccountIdentifier.AccountDon`) while the HTTP test sets a
  flat `Id`. Map it once in `fieldmap.json`
  (`"Id": "AccountIdentifier.AccountDon"`) and the whole subtree counts; until
  then it shows under `anomalies.unmatched_test_fields`.
- Create/Update RPCs that stuff the body into a DB object only *read*/validate a
  few fields directly (the rest flow to the DB sink via container taint), so the
  leaf-read denominator reflects validated fields, not every stored field.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-dir` | `.` | target service module to analyze |
| `-patterns` | `./internal/service/...` | package patterns to build SSA for |
| `-iface-pkg` / `-iface` | janus-client `api` / `JanusServer` | gRPC server interface |
| `-impl-pkg` / `-impl` | janus service / `Janus` | concrete server type |
| `-only` | all | comma-separated RPC names |
| `-out` | stdout | output file |
| `-cover` | off | lifejacket testcases dir -> coverage mode |
| `-status-spec` | auto under `-cover` | generated oapi-codegen `client.go` for documented HTTP statuses |
| `-op-map` | derived | operation->RPC overrides, e.g. `AccountsCreate=CreateAccount` |
| `-field-map` | `fieldmap.json` | reviewable HTTP->RPC field map file (missing file ignored) |
| `-field-alias` | none | inline HTTP->RPC field renames (quick form of the map) |
| `-emit-fieldmap-scaffold` | off | write a field-map scaffold from anomalies and exit |

For a non-janus service, point the interface/impl flags at that server type.

## Interpreting coverage output

Per RPC the report contains:

- `fields.coverage_pct` / `covered_count` / `leaf_fields_read_by_rpc`
  Percentage of *leaf* request fields the RPC reads that at least one test sets.
  Leaf = a read path with no deeper read path beneath it (intermediate message
  nodes like `in.Account` are excluded from the denominator).
- `fields.covered[]` each covered leaf path, `match` (`exact` or `alias`), and
  the tests that set it.
- `fields.gaps[]` leaf fields the RPC reads that no test exercises. This is the
  actionable list for adding tests.
- `branches.coverage_pct` / `input_exercised` / `total` and `branches.gaps[]`
  validation branches whose gating request fields no test sets.
- `status.documented` / `asserted` / `missing` / `coverage_pct` HTTP statuses
  documented for the operation (from the generated client spec) vs those the
  tests assert. `missing` is the actionable list (e.g. an untested `403`).
- `test_facts[]` each test's name, file, labels, fields set, statuses, and
  `asserts_success`.

This is static reachability intersected with test *inputs*, not executed-branch
coverage: setting a field proves the input is present, not that both branch
outcomes ran. The field is named `input_exercised`, not `covered`, for that
reason.

## HTTP-body vs RPC-request field mismatches (anomalies)

The HTTP request body (oapi-codegen) and the proto RPC request are usually a 1-1
leaf mapping (`display_name` <-> `in.Account.DisplayName`), handled automatically
by normalizing snake_case/PascalCase. Some fields are genuinely renamed by the
gateway and are reported under `anomalies.unmatched_test_fields`:

- `Artifacts` (HTTP) vs `ArtifactIds` (RPC)
- `SchemaFragmentIds` (HTTP) vs `CustomSchemaFragmentIds` (RPC)

Each unmatched field includes `fuzzy_suggestions` (RPC paths that look related by
shared stem or substring) to help resolve it. A field that maps to nothing (e.g.
a body field the RPC ignores) is reported with no suggestion.

### Resolving mappings (human-in-loop)

Confirmed mappings live in a reviewable file, `provenance/fieldmap.json`, loaded
by default (`-field-map`):

```json
{
  "global": { "Artifacts": "ArtifactIds" },
  "by_rpc": { "GetAccount": { "Id": "AccountIdentifier.AccountDon" } }
}
```

- Keys are HTTP body field names, values are RPC field names, matched
  case/style-insensitively.
- A value may be a dotted **path fragment**: `Id -> AccountIdentifier.AccountDon`
  credits every RPC leaf beneath that subtree (the gateway parses the flat `id`
  into a structured DON).
- `global` applies to every RPC; `by_rpc` overrides/adds per RPC; `-field-alias`
  on the CLI overrides both for quick experiments.

To bootstrap the file for a new service/operation, emit a scaffold from the
anomalies and review it:

```sh
go run . -dir /Users/devrev/janus -cover /Users/devrev/lifejacket/testcases \
  -emit-fieldmap-scaffold /tmp/scaffold.json
```

The scaffold has a `by_rpc` block pre-filled with a best-guess leaf per unmatched
field and a `_review` block listing all fuzzy suggestions. Confirm the guesses,
delete `_review`, and save as `fieldmap.json`. An empty anomaly list means the
leaf mapping held and no entry is needed.

## HTTP statuses come from the generated spec

The documented status universe per operation (200/201 success plus
400/401/403/404/409/413/429/500/503, etc.) is parsed from the generated
oapi-codegen client (`gen/v2_internal/client.go`), auto-located under `-cover` or
set with `-status-spec`. The report shows `documented` vs `asserted` and the
`missing` codes, so an untested `403`/`404` is visible. Test success assertions
(`Expect(err).To(BeNil())`) resolve to the operation's documented 2xx code (201
for creates), not a hardcoded 200.


## Determinism

Two runs over the same source produce byte-identical output:

```sh
go run . -dir /Users/devrev/janus -only CreateAccount > /tmp/a.json
go run . -dir /Users/devrev/janus -only CreateAccount > /tmp/b.json
diff /tmp/a.json /tmp/b.json && echo identical
```

## Captured samples

- `provenance/examples/janus-getdevorg-createaccount.json` raw provenance
- `provenance/examples/janus-createaccount-coverage.json` coverage report
