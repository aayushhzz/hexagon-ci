#!/usr/bin/env bash
# Reproduce the full-janus provenance and coverage artifacts in this folder.
# Deterministic: re-running overwrites the JSON with byte-identical output.
set -euo pipefail

JANUS="${JANUS:-/Users/devrev/janus}"
LIFEJACKET="${LIFEJACKET:-/Users/devrev/lifejacket}"
HERE="$(cd "$(dirname "$0")" && pwd)"
PROV="$HERE/.."

cd "$PROV"
go build -o /tmp/prov .

# 1. Full per-RPC provenance for every janus RPC.
/tmp/prov -dir "$JANUS" -out "$HERE/janus-provenance-all.json"

# 2. Full coverage: every RPC intersected with the whole lifejacket test suite.
/tmp/prov -dir "$JANUS" -cover "$LIFEJACKET/testcases" -out "$HERE/janus-coverage-all.json"

# 3. Ten representative per-RPC provenance files (mix of verbs/resources).
reps="CreateAccount,GetDevOrg,UpdateDevUser,CreateRevOrg,MergeAccounts,CreateTeam,UpdateRevOrg,CreateRevUser,GetAccount,ListDevUsers"
mkdir -p "$HERE/per-rpc"
/tmp/prov -dir "$JANUS" -only "$reps" -out /tmp/reps.json
python3 - "$HERE" <<'PY'
import json,sys,os
here=sys.argv[1]
for p in json.load(open('/tmp/reps.json'))['provenance']:
    open(os.path.join(here,'per-rpc','%s.json'%p['rpc']),'w').write(json.dumps(p,indent=2))
PY

echo "done: artifacts in $HERE"
