# Onboarding a service

Adding a service needs **no analyzer code changes** and no hand-written config.
The tool auto-discovers the server interface/impl and auto-derives the
operation->RPC map from the gateway. Point it at the service repo and the
gateway repo.

## One command

```sh
cd provenance
./onboard.sh <service-dir> <gateway-dir> [InterfaceName]
```

It prints a ready `services.json` entry and writes `opmap.<service>.json`. Add
the entry to `services.json`, review the opmap tail (see below), then run
`results/reproduce.sh` and `site/gen_site_data.py`.

## What it does (and why no code changes are needed)

1. **Discover** the gRPC server (`-discover`): scans the repo for
   `Register<X>Server(...)` calls and reports each interface + concrete impl
   whose implementation lives in the module. This yields the `iface_pkg`,
   `iface`, `impl_pkg`, `impl`, and `patterns` fields. Different repos wire
   servers differently (`internal/service` vs `internal/serverv2` vs
   `internal/server`), which is the only reason this was ever manual.

2. **Enumerate** the service's RPC names (the normal analyzer run).

3. **Map** operations to RPCs from the gateway (`-gateway-map`): a gateway
   handler method's name *is* the HTTP operation id, and any backend-client
   method it reaches (directly or through helpers) *is* the backend RPC (gRPC
   client and server method names are identical). The backend client package is
   exactly the discovered `iface_pkg` (the gateway calls the same client the
   service implements), so nothing extra is supplied.

## The two inputs, demystified

Per service there is **no code**, only two derived inputs:

- the interface/impl identification -> auto-discovered (`-discover`);
- the operation->RPC map -> auto-derived from the gateway (`-gateway-map`).

## Reviewing the generated opmap

`-gateway-map` uses static reachability, so it is a strong bootstrap, not an
oracle. Validated against the hand-reviewed Janus/codex/s3interact maps it
reproduced **100% of their mappings with zero disagreements**, and even found a
correct mapping a human missed (`WorksUpdate -> UpdateWork`). The tail needs a
look:

- Operations that touch several backends through shared helpers can produce an
  incidental mapping (e.g. `PartsContributorsList -> GroupWorks`).
- Operations that genuinely fan out to multiple RPCs are reported under
  `unresolved` rather than guessed.

The committed `opmap.*.json` files are the reviewed maps; use `onboard.sh` to
bootstrap a new one, then prune the tail.
