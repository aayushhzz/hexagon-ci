"""Language-independent facts consumed by the graph builder."""

from dataclasses import dataclass, field
from typing import Dict, List, Optional


@dataclass(frozen=True)
class Node:
    id: str
    path: str
    qualname: str
    line: int
    is_test: bool
    source_hash: Optional[str] = None


@dataclass(frozen=True)
class Edge:
    caller: str
    callee: str
    path: str
    line: int


@dataclass(frozen=True)
class UnresolvedCall:
    caller: str
    expression: str
    path: str
    line: int


@dataclass
class GraphFacts:
    nodes: List[Node]
    edges: List[Edge]
    unresolved_calls: List[UnresolvedCall]
    analysis_context: Dict[str, object] = field(default_factory=dict)
    files: List[dict] = field(default_factory=list)
    source_files: List[dict] = field(default_factory=list)
