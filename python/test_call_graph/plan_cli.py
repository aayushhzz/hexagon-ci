"""CLI for producing machine-readable and GitHub Actions test-impact reports."""

import argparse
import json
import sys
from pathlib import Path, PurePosixPath

from .impact import plan_file_changes


def _load_graph(path: str) -> dict:
    with Path(path).open("r", encoding="utf-8") as graph_file:
        return json.load(graph_file)


def _group_tests(graph: dict, test_ids) -> dict:
    nodes = {node.get("id"): node for node in graph.get("nodes", [])}
    grouped = {}
    for test_id in sorted(set(test_ids)):
        node = nodes.get(test_id)
        if not node or not node.get("is_test"):
            raise ValueError("Test root is missing from the current graph: {}".format(test_id))
        path = node.get("path")
        qualname = node.get("qualname")
        if not isinstance(path, str) or not isinstance(qualname, str) or not qualname:
            raise ValueError("Test root has an invalid file path or name: {}".format(test_id))
        normalized = PurePosixPath(path.replace("\\", "/"))
        if normalized.is_absolute() or ".." in normalized.parts or normalized.as_posix() != path:
            raise ValueError("Test root has an unsafe repository-relative path: {}".format(path))
        grouped.setdefault(path, []).append(qualname)
    return {path: sorted(names) for path, names in sorted(grouped.items())}


def _report(plan, graph: dict) -> dict:
    all_roots = set(graph.get("tests", {}))
    selected = set(plan.candidate_tests)
    if not selected.issubset(all_roots):
        raise ValueError("The plan selected test roots that are not in the current graph")
    if plan.run_full_suite:
        tests_to_run = all_roots
        tests_to_skip = set()
    else:
        tests_to_run = selected
        tests_to_skip = all_roots - selected
    return {
        "schema_version": 1,
        "language": graph.get("language"),
        "analysis_context": graph.get("analysis_context", {}),
        "all_tests_by_file": _group_tests(graph, all_roots),
        "tests_to_run": _group_tests(graph, tests_to_run),
        "tests_to_skip": _group_tests(graph, tests_to_skip),
        "candidate_tests_by_file": _group_tests(graph, selected),
        "changed_functions": list(plan.changed_functions),
        "run_full_suite": plan.run_full_suite,
        "reason": plan.reason,
    }


def _markdown(report: dict) -> str:
    if report["run_full_suite"]:
        mode = "Full suite required"
        guidance = "This fallback lists every recognized test under tests_to_run; do not skip tests."
    else:
        mode = "Selected tests"
        guidance = "The runner may execute tests_to_run and skip tests_to_skip."

    tests_to_run = json.dumps(report["tests_to_run"], ensure_ascii=False, indent=2, sort_keys=True)
    tests_to_skip = json.dumps(report["tests_to_skip"], ensure_ascii=False, indent=2, sort_keys=True)
    return (
        "## Test impact plan\n\n"
        "- **Execution recommendation:** {}\n"
        "- **Reason:** {}\n"
        "- **Language:** `{}`\n\n"
        "### Tests to run by file\n\n"
        "```json\n{}\n```\n\n"
        "### Tests to skip by file\n\n"
        "```json\n{}\n```\n\n"
        "{}\n"
    ).format(mode, report["reason"], report["language"], tests_to_run, tests_to_skip, guidance)


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
        report = _report(plan, current_graph)
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
