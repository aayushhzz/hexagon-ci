#!/usr/bin/env python3
"""Generate the static-site data from the provenance/coverage artifacts.

Reads provenance/results/janus-coverage-all.json and janus-provenance-all.json
and emits light JSON the frontend loads lazily:

  site/data/services.json          list of services + aggregate metrics
  site/data/<svc>/index.json       service totals + per-RPC summary rows
  site/data/<svc>/<RPC>.json       per-RPC detail + a copyable agent prompt

Deterministic: inputs are sorted, output key order is stable.
"""

import json
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
RESULTS = ROOT / "provenance" / "results"
MANIFEST = ROOT / "provenance" / "services.json"
OUT = Path(__file__).resolve().parent / "data"


def pct(num, den):
    return round(100.0 * num / den, 1) if den else 0.0


def load(name):
    with (RESULTS / name).open(encoding="utf-8") as fh:
        return json.load(fh)


def build_prompt(entry, prov):
    """A ready-to-paste instruction for an agent to close this RPC's gaps."""
    rpc = entry["rpc"]
    op = entry.get("operation", "")
    fields = entry.get("fields") or {}
    branches = entry.get("branches") or {}
    status = entry.get("status") or {}
    anomalies = (entry.get("anomalies") or {}).get("unmatched_test_fields") or []
    gaps = fields.get("gaps") or []
    branch_gaps = [b.get("cond", "") for b in (branches.get("gaps") or [])]
    missing = status.get("missing") or []
    errors = sorted({e.get("symbol", "") for e in (prov.get("errors") or []) if e.get("symbol")})

    lines = [
        f"Add lifejacket (Ginkgo) tests for the janus RPC `{rpc}` (HTTP operation `{op}`).",
        "",
        "Context: this is driven by static provenance+coverage analysis. Goal is to",
        "close the specific gaps below. Follow existing patterns in the matching",
        "testcases/ package (MakeRequest + Expect on status/error).",
        "",
        f"Untested HTTP statuses to assert (documented but never asserted): {missing or 'none'}",
        "",
        "Request leaf fields the RPC reads but no test sets (add positive cases that",
        "populate these, grouped by depth so nested filters are reached):",
    ]
    lines += [f"  - {g}" for g in gaps[:60]] or ["  - none"]
    if len(gaps) > 60:
        lines.append(f"  - (+{len(gaps) - 60} more)")
    lines += [
        "",
        "Validation branches whose present (failure) arm is never exercised. Add",
        "negative cases that send invalid values for these and assert the 4xx:",
    ]
    lines += [f"  - {c}" for c in branch_gaps[:40]] or ["  - none"]
    if len(branch_gaps) > 40:
        lines.append(f"  - (+{len(branch_gaps) - 40} more)")
    if errors:
        lines += ["", "Error symbols this RPC can return (candidate negative assertions):"]
        lines += [f"  - {e}" for e in errors[:40]]
    if anomalies:
        lines += [
            "",
            "Field name mismatches (HTTP vs RPC) to confirm before writing tests:",
        ]
        lines += [
            f"  - test field `{a.get('field')}` -> suggestions {a.get('fuzzy_suggestions') or []}"
            for a in anomalies
        ]
    lines += [
        "",
        "Deliverable: new It() blocks covering the above, each asserting the right",
        "status code, kept deterministic and idempotent.",
    ]
    return "\n".join(lines)


def summary_row(entry):
    f = entry.get("fields") or {}
    b = entry.get("branches") or {}
    s = entry.get("status") or {}
    a = (entry.get("anomalies") or {}).get("unmatched_test_fields") or []
    svc = (b.get("by_origin") or {}).get("service", {})
    return {
        "rpc": entry["rpc"],
        "operation": entry.get("operation", ""),
        "unmapped": bool(entry.get("unmapped", False)),
        "tests": entry.get("tests", 0) or 0,
        "field_pct": f.get("coverage_pct", 0.0) or 0.0,
        "field_covered": f.get("covered_count", 0) or 0,
        "field_total": f.get("leaf_fields_read_by_rpc", 0) or 0,
        "rep_pct": (f.get("representative") or {}).get("pct", 0.0) or 0.0,
        "rep_covered": (f.get("representative") or {}).get("covered", 0) or 0,
        "rep_total": (f.get("representative") or {}).get("units", 0) or 0,
        "svc_present_pct": svc.get("present_arm_pct", 0.0) or 0.0,
        "svc_present": svc.get("present_arm_exercised", 0) or 0,
        "svc_negative_pct": svc.get("negative_arm_pct", 0.0) or 0.0,
        "svc_branch_total": svc.get("total", 0) or 0,
        "present_arm": b.get("present_arm_exercised", 0) or 0,
        "branch_total": b.get("total", 0) or 0,
        "present_pct": b.get("present_arm_pct", 0.0) or 0.0,
        "negative_pct": b.get("negative_arm_pct", 0.0) or 0.0,
        "status_pct": s.get("coverage_pct", 0.0) or 0.0,
        "missing_count": len(s.get("missing") or []),
        "anomaly_count": len(a),
        "gap_count": len(f.get("gaps") or []),
        "dep_services": (entry.get("dependencies") or {}).get("service_count", 0) or 0,
        "dep_reached": bool((entry.get("dependencies") or {}).get("reached_by_test", False)),
        "dep_used": (entry.get("dependencies") or {}).get("deps_result_used_in_response", 0) or 0,
        "dep_checked": (entry.get("dependencies") or {}).get("deps_result_checked_by_test", 0) or 0,
    }


def build_service(service, cov, prov):
    """Emit index.json + per-RPC detail for one service, return its services.json row."""
    prov_by_rpc = {p["rpc"]: p for p in prov.get("provenance", [])}
    entries = sorted(cov.get("coverage", []), key=lambda e: e["rpc"])
    mapped = [e for e in entries if not e.get("unmapped", False)]

    def agg(key, sub):
        return sum((e.get(key) or {}).get(sub, 0) or 0 for e in mapped)

    def agg_rep(sub):
        return sum(((e.get("fields") or {}).get("representative") or {}).get(sub, 0) or 0 for e in mapped)

    def agg_svc(sub):
        return sum((((e.get("branches") or {}).get("by_origin") or {}).get("service") or {}).get(sub, 0) or 0 for e in mapped)

    status_doc = sum(len((e.get("status") or {}).get("documented") or []) for e in mapped)
    status_hit = sum(
        len(set((e.get("status") or {}).get("asserted") or []) & set((e.get("status") or {}).get("documented") or []))
        for e in mapped
    )
    anomalies_total = sum(
        len((e.get("anomalies") or {}).get("unmatched_test_fields") or []) for e in mapped
    )
    dep_rpcs = [e for e in mapped if (e.get("dependencies") or {}).get("service_count", 0)]
    dep_reached = [e for e in dep_rpcs if (e.get("dependencies") or {}).get("reached_by_test")]
    dep_used = sum((e.get("dependencies") or {}).get("deps_result_used_in_response", 0) or 0 for e in mapped)
    dep_checked = sum((e.get("dependencies") or {}).get("deps_result_checked_by_test", 0) or 0 for e in mapped)
    totals = {
        "rpcs": len(entries),
        "mapped": len(mapped),
        "unmapped": len(entries) - len(mapped),
        "field_covered": agg("fields", "covered_count"),
        "field_total": agg("fields", "leaf_fields_read_by_rpc"),
        "rep_covered": agg_rep("covered"),
        "rep_total": agg_rep("units"),
        "present_arm": agg("branches", "present_arm_exercised"),
        "negative_arm": agg("branches", "negative_arm_exercised"),
        "absent_arm": agg("branches", "absent_arm_exercised"),
        "reachable_on_happy": agg("branches", "reachable_on_happy_path"),
        "branch_total": agg("branches", "total"),
        "svc_present": agg_svc("present_arm_exercised"),
        "svc_negative": agg_svc("negative_arm_exercised"),
        "svc_branch_total": agg_svc("total"),
        "status_documented": status_doc,
        "status_asserted": status_hit,
        "anomalies": anomalies_total,
        "dep_rpcs": len(dep_rpcs),
        "dep_rpcs_reached": len(dep_reached),
        "dep_used": dep_used,
        "dep_checked": dep_checked,
    }
    totals["field_pct"] = pct(totals["field_covered"], totals["field_total"])
    totals["rep_pct"] = pct(totals["rep_covered"], totals["rep_total"])
    totals["present_pct"] = pct(totals["present_arm"], totals["branch_total"])
    totals["svc_present_pct"] = pct(totals["svc_present"], totals["svc_branch_total"])
    totals["svc_negative_pct"] = pct(totals["svc_negative"], totals["svc_branch_total"])
    totals["negative_pct"] = pct(totals["negative_arm"], totals["branch_total"])
    totals["absent_pct"] = pct(totals["absent_arm"], totals["reachable_on_happy"])
    totals["status_pct"] = pct(totals["status_asserted"], totals["status_documented"])

    svc_dir = OUT / service
    svc_dir.mkdir(parents=True, exist_ok=True)

    index = {"service": service, "totals": totals, "rpcs": [summary_row(e) for e in entries]}
    (svc_dir / "index.json").write_text(json.dumps(index, indent=2), encoding="utf-8")

    for e in entries:
        p = prov_by_rpc.get(e["rpc"], {})
        detail = {
            "service": service,
            "coverage": e,
            "provenance": {
                "func": p.get("func", ""),
                "request_type": p.get("request_type", ""),
                "response_type": p.get("response_type", ""),
                "sinks": p.get("sinks") or [],
                "errors": p.get("errors") or [],
                "unresolved": p.get("unresolved") or [],
            },
            "agent_prompt": build_prompt(e, p),
        }
        (svc_dir / f"{e['rpc']}.json").write_text(json.dumps(detail, indent=2), encoding="utf-8")

    print(f"wrote {len(entries)} RPC files for service '{service}' to {svc_dir}")
    return {
        "service": service,
        "rpcs": totals["rpcs"],
        "mapped": totals["mapped"],
        "field_pct": totals["field_pct"],
        "rep_pct": totals["rep_pct"],
        "present_pct": totals["present_pct"],
        "svc_present_pct": totals["svc_present_pct"],
        "negative_pct": totals["negative_pct"],
        "status_pct": totals["status_pct"],
        "anomalies": totals["anomalies"],
        "dep_rpcs": totals.get("dep_rpcs", 0),
        "dep_used": totals.get("dep_used", 0),
        "dep_checked": totals.get("dep_checked", 0),
    }


def main():
    manifest = json.loads(MANIFEST.read_text(encoding="utf-8"))
    OUT.mkdir(parents=True, exist_ok=True)
    services = []
    for svc in manifest["services"]:
        slug = svc["name"].lower()
        cov_path = RESULTS / f"{slug}-coverage-all.json"
        prov_path = RESULTS / f"{slug}-provenance-all.json"
        if not cov_path.exists() or not prov_path.exists():
            print(f"skip {svc['name']}: missing {cov_path.name} or {prov_path.name} (run results/reproduce.sh first)")
            continue
        cov = json.loads(cov_path.read_text(encoding="utf-8"))
        prov = json.loads(prov_path.read_text(encoding="utf-8"))
        services.append(build_service(svc["name"], cov, prov))
    services.sort(key=lambda s: s["service"].lower())
    (OUT / "services.json").write_text(json.dumps({"services": services}, indent=2), encoding="utf-8")
    print(f"wrote services.json with {len(services)} services")


if __name__ == "__main__":
    main()
