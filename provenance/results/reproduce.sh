#!/usr/bin/env bash
# Reproduce provenance + coverage artifacts for every service in services.json.
# Deterministic: re-running overwrites the JSON with byte-identical output.
# Per-service source dirs come from env vars (see services.json dir_env),
# falling back to the committed defaults. Lifejacket test suite via LIFEJACKET.
set -euo pipefail

LIFEJACKET="${LIFEJACKET:-/Users/devrev/lifejacket}"
HERE="$(cd "$(dirname "$0")" && pwd)"
PROV="$HERE/.."

cd "$PROV"
go build -o /tmp/prov .

# Drive the per-service runs from the manifest so the site and the artifacts
# stay in lockstep. slug = lowercased service name (file prefix).
python3 - "$PROV" "$HERE" "$LIFEJACKET" <<'PY'
import json, os, subprocess, sys

prov_dir, here, lifejacket = sys.argv[1], sys.argv[2], sys.argv[3]
manifest = json.load(open(os.path.join(prov_dir, "services.json")))
testcases = os.path.join(lifejacket, "testcases")

for svc in manifest["services"]:
    slug = svc["name"].lower()
    src = os.environ.get(svc["dir_env"], svc["dir_default"])
    base = [
        "/tmp/prov",
        "-dir", src,
        "-patterns", svc["patterns"],
        "-iface-pkg", svc["iface_pkg"],
        "-iface", svc["iface"],
        "-impl-pkg", svc["impl_pkg"],
        "-impl", svc["impl"],
        "-op-map-file", svc.get("op_map_file", ""),
        "-field-map", svc.get("field_map_file", ""),
    ]
    prov_out = os.path.join(here, f"{slug}-provenance-all.json")
    cov_out = os.path.join(here, f"{slug}-coverage-all.json")
    subprocess.run(base + ["-out", prov_out], check=True)
    subprocess.run(base + ["-cover", testcases, "-out", cov_out], check=True)
    print(f"  {svc['name']}: {prov_out}, {cov_out}")
PY

echo "done: artifacts in $HERE"
