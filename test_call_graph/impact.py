"""Conservative file-change planning over source-derived test graphs."""

from collections import defaultdict, deque
from dataclasses import dataclass
from pathlib import PurePosixPath
from typing import Iterable, Optional


@dataclass(frozen=True)
class ImpactPlan:
    candidate_tests: tuple
    run_full_suite: bool
    reason: str


def plan_file_changes(graph: dict, changed_files: Iterable[str], previous_graph: Optional[dict] = None) -> ImpactPlan:
    """Report known ancestor tests, but never authorize a skip from an AST graph alone.

    Include old and new package import paths to account for deleted or renamed
    source. Candidate tests are diagnostic; run_full_suite is authoritative.
    """
    changed = set(changed_files)
    if not changed:
        raise ValueError("At least one changed file is required")
    current_roots = set(graph["tests"])
    candidates = set()
    for snapshot in (graph, previous_graph):
        if snapshot is None:
            continue
        files = {file["path"]: file for file in snapshot.get("files", [])}
        node_paths = {node["id"]: node["path"] for node in snapshot["nodes"]}
        reverse_imports = defaultdict(set)
        for file in files.values():
            for imported in file["imports"]:
                reverse_imports[imported].add(file["package"])
        affected = {files[path]["package"] for path in changed if path in files}
        queue = deque(affected)
        while queue:
            for importer in reverse_imports[queue.popleft()] - affected:
                affected.add(importer)
                queue.append(importer)
        for root, view in snapshot["tests"].items():
            if root not in current_roots:
                continue
            test_file = files.get(node_paths[root])
            if (test_file is not None and test_file["package"] in affected) or any(
                node_paths[node_id] in changed for node_id in view["nodes"]
            ):
                candidates.add(root)
    return ImpactPlan(
        candidate_tests=tuple(sorted(candidates)),
        run_full_suite=True,
        reason="AST call and package graphs cannot prove all runtime or non-call dependencies",
    )
