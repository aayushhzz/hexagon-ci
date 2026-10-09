"""Conservative test selection for source changes."""

from collections import defaultdict, deque
from dataclasses import dataclass
from pathlib import PurePosixPath
from typing import Iterable, Optional


@dataclass(frozen=True)
class ImpactPlan:
    candidate_tests: tuple
    run_full_suite: bool
    reason: str
    changed_functions: tuple = ()


def _file_candidates(snapshots, changed, current_roots):
    """Return broad file/package candidates for diagnostic fallback plans."""
    candidates = set()
    for snapshot in snapshots:
        if snapshot is None:
            continue
        files = {file["path"]: file for file in snapshot.get("files", [])}
        node_paths = {node["id"]: node["path"] for node in snapshot.get("nodes", [])}
        reverse_imports = defaultdict(set)
        for file in files.values():
            for imported in file.get("imports", []):
                reverse_imports[imported].add(file["package"])
        affected = {files[path]["package"] for path in changed if path in files}
        queue = deque(affected)
        while queue:
            for importer in reverse_imports[queue.popleft()] - affected:
                affected.add(importer)
                queue.append(importer)
        for root, view in snapshot.get("tests", {}).items():
            if root not in current_roots:
                continue
            test_file = files.get(node_paths.get(root))
            if (test_file is not None and test_file["package"] in affected) or any(
                node_paths.get(node_id) in changed for node_id in view.get("nodes", [])
            ):
                candidates.add(root)
    return candidates


def _source_file_map(graph):
    source_files = graph.get("source_files")
    if not isinstance(source_files, list):
        return None
    result = {}
    for source_file in source_files:
        path = source_file.get("path")
        fingerprint = source_file.get("non_function_hash")
        if not path or not fingerprint or path in result:
            return None
        result[path] = fingerprint
    return result


def _node_map(graph):
    return {node["id"]: node for node in graph.get("nodes", [])}


def _unsupported_go_roots(graph):
    reasons = set()
    for node in graph.get("nodes", []):
        path = node.get("path", "")
        name = node.get("qualname", "").split(".")[-1].split("#")[0]
        if name == "init":
            reasons.add("Go package init functions are not connected to test roots")
        if path.endswith("_test.go") and name == "TestMain":
            reasons.add("Go TestMain setup is not connected to test roots")
        if path.endswith("_test.go") and name.startswith(("Example", "Benchmark", "Fuzz")):
            reasons.add("Example, benchmark, and fuzz roots are not represented")
    return reasons


def plan_file_changes(graph: dict, changed_files: Iterable[str], previous_graph: Optional[dict] = None) -> ImpactPlan:
    """Select tests for changed functions, falling back when ancestry is uncertain.

    Graph snapshots must include function and non-function fingerprints. Tests
    are selected from both snapshots so deleted functions and changed edges keep
    their old test ancestors. This planner returns a plan; it does not run tests.
    """
    changed = {PurePosixPath(str(path).replace("\\", "/")).as_posix() for path in changed_files}
    if not changed:
        raise ValueError("At least one changed file is required")

    current_roots = set(graph.get("tests", {}))
    snapshots = (graph, previous_graph)
    file_candidates = _file_candidates(snapshots, changed, current_roots)
    reasons = set()
    changed_functions = set()
    changed_function_paths = set()
    exact_candidates = set()

    if previous_graph is None:
        reasons.add("A previous graph is required to identify changed functions")
    else:
        if not current_roots or not previous_graph.get("tests"):
            reasons.add("No recognized Go test roots are available in both snapshots")
        if graph.get("format_version") != 3 or previous_graph.get("format_version") != 3:
            reasons.add("Graph snapshots lack function-level fingerprints")
        if graph.get("language") != "go" or previous_graph.get("language") != "go":
            reasons.add("Selective planning is currently supported only for Go graphs")
        if graph.get("language") != previous_graph.get("language"):
            reasons.add("Graph languages differ")
        if graph.get("analysis_context") != previous_graph.get("analysis_context"):
            reasons.add("Graph analysis or Go build contexts differ")

        old_nodes = _node_map(previous_graph)
        new_nodes = _node_map(graph)
        for node_id in sorted(set(old_nodes) | set(new_nodes)):
            old = old_nodes.get(node_id)
            new = new_nodes.get(node_id)
            if (old is not None and not old.get("source_hash")) or (
                new is not None and not new.get("source_hash")
            ):
                reasons.add("A function fingerprint is missing")
                continue
            if old is None or new is None or old["source_hash"] != new["source_hash"]:
                changed_functions.add(node_id)
                changed_function_paths.add((new or old)["path"])

        old_source_files = _source_file_map(previous_graph)
        new_source_files = _source_file_map(graph)
        changed_source_paths = set()
        if old_source_files is None or new_source_files is None:
            reasons.add("A source-file fingerprint is missing")
        else:
            for path in sorted(set(old_source_files) | set(new_source_files)):
                if old_source_files.get(path) != new_source_files.get(path):
                    changed_source_paths.add(path)
                    reasons.add("Non-function or build-input source changed: {}".format(path))
            known_source_paths = set(old_source_files) | set(new_source_files)
            for path in changed - known_source_paths:
                reasons.add("Changed path is outside analyzed source files: {}".format(path))

        if graph.get("source_fingerprint") != previous_graph.get("source_fingerprint"):
            explained_paths = changed_function_paths | changed_source_paths
            if not explained_paths:
                reasons.add("Source fingerprints differ without explainable source changes")
            for path in explained_paths - changed:
                reasons.add("Graph changed outside the supplied changed-file set: {}".format(path))
        elif changed_functions or changed_source_paths:
            reasons.add("Graph contents differ despite matching source fingerprints")

        reasons.update(graph.get("analysis_context", {}).get("selection_unsafe_reasons", []))
        reasons.update(previous_graph.get("analysis_context", {}).get("selection_unsafe_reasons", []))
        reasons.update(_unsupported_go_roots(graph))
        reasons.update(_unsupported_go_roots(previous_graph))

        for snapshot in snapshots:
            if snapshot is None:
                continue
            for root, view in snapshot.get("tests", {}).items():
                if root not in current_roots:
                    continue
                reached = set(view.get("nodes", []))
                if reached.intersection(changed_functions):
                    exact_candidates.add(root)
                # Any unresolved call in a test's reachable code could target
                # a changed local function, so retain that test conservatively.
                if view.get("unresolved_calls"):
                    exact_candidates.add(root)

    if graph.get("language") != "go":
        reasons.add("Python framework fixtures and import-time execution are not modeled")

    if reasons:
        return ImpactPlan(
            candidate_tests=tuple(sorted(file_candidates | exact_candidates)),
            run_full_suite=True,
            reason="; ".join(sorted(reasons)),
            changed_functions=tuple(sorted(changed_functions)),
        )

    return ImpactPlan(
        candidate_tests=tuple(sorted(exact_candidates)),
        run_full_suite=False,
        reason="Go changes are confined to represented function bodies; uncertain test roots are included",
        changed_functions=tuple(sorted(changed_functions)),
    )
