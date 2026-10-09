"""CLI for producing machine-readable and GitHub Actions test-impact reports."""

import argparse
import json
import sys
from pathlib import Path

from .impact import plan_file_changes


def _load_graph(path: str) -> dict:
    with Path(path).open("r", encoding="utf-8") as graph_file:
        return json.load(graph_file)


def _report(plan) -> dict:
    tests = list(plan.candidate_tests)
    return {
        "tests": tests,
        "tests_csv": ", ".join(tests),
        "changed_functions": list(plan.changed_functions),
        "run_full_suite": plan.run_full_suite,
        "reason": plan.reason,
    }


def _markdown(report: dict) -> str:
    if report["run_full_suite"]:
        mode = "Full suite required"
        guidance = "The test array is diagnostic only; do not use it to skip the full suite."
    else:
        mode = "Selected test roots"
        guidance = "Run only the test roots in this array."

    tests_json = json.dumps(report["tests"], ensure_ascii=False)
    tests_csv = report["tests_csv"] or "(empty)"
    return (
        "## Test impact plan\n\n"
        "- **Execution recommendation:** {}\n"
        "- **Reason:** {}\n"
        "- **Test IDs (JSON array):** `{}`\n"
        "- **Test IDs (comma-separated):** `{}`\n\n"
        "{}\n"
    ).format(mode, report["reason"], tests_json, tests_csv, guidance)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="Plan tests affected by repository changes")
    parser.add_argument("--current-graph", required=True, help="Current/head graph JSON")
    parser.add_argument("--previous-graph", help="Previous/base graph JSON")
    parser.add_argument(
        "--changed-files", required=True,
        help="Text file containing one changed repository-relative path per line",
    )
    parser.add_argument(
        "--summary-file",
        help="Append a Markdown report to this file, such as GITHUB_STEP_SUMMARY",
    )
    args = parser.parse_args(argv)

    try:
        current_graph = _load_graph(args.current_graph)
        previous_graph = _load_graph(args.previous_graph) if args.previous_graph else None
        changed_files = Path(args.changed_files).read_text(encoding="utf-8").splitlines()
        plan = plan_file_changes(current_graph, changed_files, previous_graph=previous_graph)
        report = _report(plan)
        if args.summary_file:
            with Path(args.summary_file).open("a", encoding="utf-8") as summary_file:
                summary_file.write(_markdown(report))
        print(json.dumps(report, ensure_ascii=False, sort_keys=True))
    except (OSError, UnicodeError, ValueError, KeyError, TypeError) as error:
        print("test impact planning failed: {}".format(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
