"""Rebuild the entire call graph and publish only completed snapshots."""

import hashlib
import gzip
import io
import json
import os
import tempfile
from collections import defaultdict, deque
from pathlib import Path
from typing import Dict, Mapping, Optional, Protocol, Set, Tuple, Union

from .model import GraphFacts
from .python_frontend import PythonFrontend


class SourceFrontend(Protocol):
    language: str
    source_extension: str

    def collect_sources(self, root: Path) -> Mapping[str, bytes]:
        ...

    def analyze(self, sources: Mapping[str, bytes]) -> GraphFacts:
        ...


def _fingerprint(sources: Mapping[str, bytes]) -> str:
    digest = hashlib.sha256()
    for path, content in sorted(sources.items()):
        name = path.encode("utf-8")
        digest.update(len(name).to_bytes(8, "big"))
        digest.update(name)
        digest.update(len(content).to_bytes(8, "big"))
        digest.update(content)
    return digest.hexdigest()


def _graph_from_facts(facts: GraphFacts, fingerprint: str, file_count: int, language: str) -> dict:
    nodes = {}
    for node in facts.nodes:
        if node.id in nodes:
            raise ValueError("Duplicate graph node: {}".format(node.id))
        nodes[node.id] = {
            "id": node.id,
            "path": node.path,
            "qualname": node.qualname,
            "line": node.line,
            "is_test": node.is_test,
            "source_hash": node.source_hash,
        }

    callsites: Dict[Tuple[str, str], list] = defaultdict(list)
    adjacency: Dict[str, Set[str]] = defaultdict(set)
    for edge in facts.edges:
        if edge.caller not in nodes or edge.callee not in nodes:
            raise ValueError("Call edge references a missing graph node")
        adjacency[edge.caller].add(edge.callee)
        callsites[(edge.caller, edge.callee)].append({"path": edge.path, "line": edge.line})

    edges = [
        {
            "caller": caller,
            "callee": callee,
            "callsites": sorted(locations, key=lambda location: (location["path"], location["line"])),
        }
        for (caller, callee), locations in sorted(callsites.items())
    ]
    unresolved = []
    for call in facts.unresolved_calls:
        if call.caller not in nodes:
            raise ValueError("Unresolved call references a missing graph node")
        unresolved.append(
            {
                "caller": call.caller,
                "expression": call.expression,
                "path": call.path,
                "line": call.line,
            }
        )
    unresolved.sort(key=lambda call: (call["caller"], call["path"], call["line"], call["expression"]))

    tests = {}
    for root in sorted(node_id for node_id, node in nodes.items() if node["is_test"]):
        visited = {root}
        pending = deque([root])
        reached_edges = set()
        while pending:
            caller = pending.popleft()
            for callee in sorted(adjacency[caller]):
                reached_edges.add((caller, callee))
                if callee not in visited:
                    visited.add(callee)
                    pending.append(callee)
        tests[root] = {
            "nodes": sorted(visited),
            "edges": [
                {"caller": caller, "callee": callee}
                for caller, callee in sorted(reached_edges)
            ],
            "unresolved_calls": [call for call in unresolved if call["caller"] in visited],
        }

    return {
        "format_version": 3,
        "language": language,
        "source_fingerprint": fingerprint,
        "analysis_context": facts.analysis_context,
        "analysis_fingerprint": hashlib.sha256(json.dumps(
            [fingerprint, facts.analysis_context], sort_keys=True
        ).encode("utf-8")).hexdigest(),
        "file_count": file_count,
        "files": sorted(facts.files, key=lambda file: file["path"]),
        "source_files": sorted(facts.source_files, key=lambda file: file["path"]),
        "nodes": [nodes[node_id] for node_id in sorted(nodes)],
        "edges": edges,
        "unresolved_calls": unresolved,
        "tests": tests,
    }


def build_graph(root: Union[str, Path], frontend: Optional[SourceFrontend] = None) -> dict:
    """Build from all source files; abort if any file changes during the rebuild."""
    root = Path(root).resolve()
    if not root.is_dir():
        raise ValueError("Repository directory does not exist: {}".format(root))
    frontend = frontend or PythonFrontend()
    sources = frontend.collect_sources(root)
    facts = frontend.analyze(sources)
    file_count = sum(path.endswith(frontend.source_extension) for path in sources)
    graph = _graph_from_facts(facts, _fingerprint(sources), file_count, frontend.language)
    if sources != frontend.collect_sources(root):
        raise RuntimeError("Repository sources changed during the rebuild; retry")
    return graph


def rebuild_to_file(
    root: Union[str, Path], output: Union[str, Path], frontend: Optional[SourceFrontend] = None
) -> dict:
    """Atomically replace output after a successful complete rebuild."""
    graph = build_graph(root, frontend)
    output = Path(output)
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary_path = None
    try:
        compressed = output.suffix == ".gz"
        with tempfile.NamedTemporaryFile(
            mode="w+b" if compressed else "w", encoding=None if compressed else "utf-8",
            prefix=output.name + ".", suffix=".tmp",
            dir=str(output.parent), delete=False
        ) as temporary:
            temporary_path = Path(temporary.name)
            if compressed:
                with gzip.GzipFile(fileobj=temporary, mode="wb", filename="", mtime=0) as zipped:
                    writer = io.TextIOWrapper(zipped, encoding="utf-8")
                    json.dump(graph, writer, indent=2, sort_keys=True)
                    writer.write("\n")
                    writer.flush()
                    writer.detach()
            else:
                json.dump(graph, temporary, indent=2, sort_keys=True)
                temporary.write("\n")
            temporary.flush()
            os.fsync(temporary.fileno())
        os.replace(str(temporary_path), str(output))
    finally:
        if temporary_path is not None and temporary_path.exists():
            temporary_path.unlink()
    return graph
