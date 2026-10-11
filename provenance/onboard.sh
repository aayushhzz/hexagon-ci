#!/usr/bin/env bash
# Onboard a service with no hand-written config. Given the service repo and the
# gateway repo, this:
#   1. auto-discovers the gRPC server interface + impl (-discover),
#   2. enumerates the service's RPCs,
#   3. auto-derives the operation->RPC map from the gateway (-gateway-map),
#      using the discovered interface package as the backend client package,
#   4. prints a ready services.json entry.
#
# Usage: onboard.sh <service-dir> <gateway-dir> [InterfaceName]
# If InterfaceName is omitted and several are found, the one with the most RPCs
# is chosen. The generated opmap is a bootstrap: review the tail for operations
# that touch several backends (reported under "unresolved").
set -euo pipefail

SVC_DIR="${1:?service dir}"
GW_DIR="${2:?gateway dir}"
WANT_IFACE="${3:-}"
HERE="$(cd "$(dirname "$0")" && pwd)"

cd "$HERE"
go build -o /tmp/prov .

/tmp/prov -dir "$SVC_DIR" -discover -out /tmp/discover.json >/dev/null 2>&1

python3 - "$SVC_DIR" "$GW_DIR" "$WANT_IFACE" "$HERE" <<'PY'
import json, os, subprocess, sys

svc_dir, gw_dir, want_iface, here = sys.argv[1:5]
specs = json.load(open("/tmp/discover.json"))["services"]
if not specs:
    sys.exit("no gRPC server interfaces discovered in " + svc_dir)

def rpc_names(spec):
    out = subprocess.run(
        ["/tmp/prov", "-dir", svc_dir, "-patterns", spec["patterns"],
         "-iface-pkg", spec["iface_pkg"], "-iface", spec["iface"],
         "-impl-pkg", spec["impl_pkg"], "-impl", spec["impl"], "-out", "/dev/stdout"],
        capture_output=True, text=True)
    try:
        data = json.loads(out.stdout)
    except json.JSONDecodeError:
        return []
    return sorted(p["rpc"] for p in data.get("provenance", []))

# Choose the interface: explicit name, else the one exposing the most RPCs.
chosen, chosen_rpcs = None, []
for spec in specs:
    if want_iface and spec["iface"] != want_iface:
        continue
    rpcs = rpc_names(spec)
    if len(rpcs) > len(chosen_rpcs):
        chosen, chosen_rpcs = spec, rpcs
if chosen is None:
    sys.exit("interface %r not found; discovered: %s" % (want_iface, [s["iface"] for s in specs]))

name = chosen["name"]
slug = name.lower()
opmap_path = os.path.join(here, f"opmap.{slug}.json")

# The gateway calls the same client package the service implements, so the
# backend client package is exactly the discovered interface package.
subprocess.run(
    ["/tmp/prov", "-dir", gw_dir, "-gateway-map", "-service", slug,
     "-backend-pkgs", chosen["iface_pkg"], "-rpc-whitelist", ",".join(chosen_rpcs),
     "-out", opmap_path], check=True)
m = json.load(open(opmap_path))

entry = {
    "name": name,
    "dir_env": name.upper(),
    "dir_default": os.path.abspath(svc_dir),
    "patterns": chosen["patterns"],
    "iface_pkg": chosen["iface_pkg"],
    "iface": chosen["iface"],
    "impl_pkg": chosen["impl_pkg"],
    "impl": chosen["impl"],
    "op_map_file": f"opmap.{slug}.json",
}
print("\n=== add this to provenance/services.json ===")
print(json.dumps(entry, indent=2))
print(f"\nopmap: {opmap_path}  ({len(m['by_operation'])} operations, "
      f"{len(m.get('unresolved') or [])} unresolved to review)")
PY
