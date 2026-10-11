"""A deliberately conservative Python frontend for direct source calls."""

import ast
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Mapping, Optional, Tuple

from .model import Edge, GraphFacts, Node, UnresolvedCall


IGNORED_DIRS = {".git", ".venv", "venv", "__pycache__", ".tox", "node_modules", "build", "dist"}


@dataclass(frozen=True)
class _Symbol:
    node: Node
    module: str
    function_scopes: Tuple[str, ...]
    class_qualname: Optional[str]


@dataclass(frozen=True)
class _CallSite:
    caller: str
    expression: str
    parts: Optional[Tuple[str, ...]]
    path: str
    line: int


@dataclass
class _ParsedFile:
    symbols: List[_Symbol]
    calls: List[_CallSite]
    imported_modules: Dict[str, str]
    imported_names: Dict[str, Tuple[str, str]]


def _module_name(path: str) -> str:
    parts = list(Path(path).with_suffix("").parts)
    if parts[-1] == "__init__" and len(parts) > 1:
        parts.pop()
    return ".".join(parts)


def _attribute_parts(node: ast.AST) -> Optional[Tuple[str, ...]]:
    if isinstance(node, ast.Name):
        return (node.id,)
    if isinstance(node, ast.Attribute):
        parent = _attribute_parts(node.value)
        if parent is not None:
            return parent + (node.attr,)
    return None


class _Visitor(ast.NodeVisitor):
    def __init__(self, path: str):
        self.path = path
        self.module = _module_name(path)
        self.scope: List[Tuple[str, str]] = []
        self.functions: List[str] = []
        self.symbols: List[_Symbol] = []
        self.calls: List[_CallSite] = []
        self.imported_modules: Dict[str, str] = {}
        self.imported_names: Dict[str, Tuple[str, str]] = {}

    def visit_ClassDef(self, node: ast.ClassDef) -> None:
        self.scope.append((node.name, "class"))
        for statement in node.body:
            self.visit(statement)
        self.scope.pop()

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        self._visit_function(node)

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
        self._visit_function(node)

    def _visit_function(self, node: ast.AST) -> None:
        name = node.name
        qualname = ".".join([part for part, _ in self.scope] + [name])
        class_parts = [index for index, (_, kind) in enumerate(self.scope) if kind == "class"]
        class_qualname = None
        if class_parts:
            class_qualname = ".".join(part for part, _ in self.scope[: class_parts[-1] + 1])
        symbol = _Symbol(
            node=Node(
                id="{}::{}".format(self.path, qualname),
                path=self.path,
                qualname=qualname,
                line=node.lineno,
                is_test=name.startswith("test_") and not self.functions,
            ),
            module=self.module,
            function_scopes=tuple(self.functions),
            class_qualname=class_qualname,
        )
        self.symbols.append(symbol)
        self.scope.append((name, "function"))
        self.functions.append(qualname)
        # Decorators and default arguments run when a function is defined, not when it is called.
        for statement in node.body:
            self.visit(statement)
        self.functions.pop()
        self.scope.pop()

    def visit_Import(self, node: ast.Import) -> None:
        if self.scope:
            return
        for alias in node.names:
            local = alias.asname or alias.name.split(".")[0]
            self.imported_modules[local] = alias.name if alias.asname else local

    def visit_ImportFrom(self, node: ast.ImportFrom) -> None:
        if self.scope:
            return
        module = node.module or ""
        if node.level:
            package = self.module.split(".")
            if Path(self.path).name != "__init__.py":
                package.pop()
            package = package[: max(0, len(package) - node.level + 1)]
            module = ".".join(part for part in [".".join(package), module] if part)
        for alias in node.names:
            if alias.name != "*":
                self.imported_names[alias.asname or alias.name] = (module, alias.name)

    def visit_Call(self, node: ast.Call) -> None:
        if self.functions:
            self.calls.append(
                _CallSite(
                    caller="{}::{}".format(self.path, self.functions[-1]),
                    expression=ast.unparse(node.func),
                    parts=_attribute_parts(node.func),
                    path=self.path,
                    line=node.lineno,
                )
            )
        self.generic_visit(node)


def _lookup(index: Mapping[Tuple[str, str], _Symbol], module: str, name: str) -> Optional[str]:
    symbol = index.get((module, name)) or index.get((module, name + ".__init__"))
    return symbol.node.id if symbol else None


def _lookup_dotted(
    index: Mapping[Tuple[str, str], _Symbol], parts: Tuple[str, ...]
) -> Optional[str]:
    for split in range(len(parts) - 1, 0, -1):
        result = _lookup(index, ".".join(parts[:split]), ".".join(parts[split:]))
        if result:
            return result
    return None


def _resolve(
    call: _CallSite,
    caller: _Symbol,
    parsed: _ParsedFile,
    index: Mapping[Tuple[str, str], _Symbol],
) -> Optional[str]:
    parts = call.parts
    if not parts:
        return None
    if len(parts) == 1:
        name = parts[0]
        for scope in (caller.node.qualname,) + tuple(reversed(caller.function_scopes)):
            result = _lookup(index, caller.module, scope + "." + name)
            if result:
                return result
        result = _lookup(index, caller.module, name)
        if result:
            return result
        imported = parsed.imported_names.get(name)
        return _lookup(index, *imported) if imported else None

    if parts[0] in {"self", "cls"} and len(parts) == 2 and caller.class_qualname:
        return _lookup(index, caller.module, caller.class_qualname + "." + parts[1])
    imported_module = parsed.imported_modules.get(parts[0])
    if imported_module:
        result = _lookup_dotted(index, tuple(imported_module.split(".")) + parts[1:])
        if result:
            return result
    imported_name = parsed.imported_names.get(parts[0])
    if imported_name:
        result = _lookup(index, imported_name[0], ".".join((imported_name[1],) + parts[1:]))
        if result:
            return result
        result = _lookup_dotted(index, tuple(imported_name[0].split(".")) + (imported_name[1],) + parts[1:])
        if result:
            return result
    return _lookup(index, caller.module, ".".join(parts))


class PythonFrontend:
    """Produce source facts for Python files without executing repository code."""

    language = "python"
    source_extension = ".py"

    def collect_sources(self, root: Path) -> Dict[str, bytes]:
        sources = {}
        for directory, subdirs, files in os.walk(str(root)):
            subdirs[:] = sorted(
                name
                for name in subdirs
                if name not in IGNORED_DIRS and not name.startswith(".")
                and not Path(directory, name).is_symlink()
            )
            for name in sorted(files):
                path = Path(directory, name)
                if name.endswith(".py") and not path.is_symlink():
                    sources[path.relative_to(root).as_posix()] = path.read_bytes()
        return sources

    def analyze(self, sources: Mapping[str, bytes]) -> GraphFacts:
        if not sources:
            raise ValueError("No Python source files found in the repository")
        parsed_files: Dict[str, _ParsedFile] = {}
        index: Dict[Tuple[str, str], _Symbol] = {}
        by_id: Dict[str, _Symbol] = {}
        for path, content in sorted(sources.items()):
            visitor = _Visitor(path)
            visitor.visit(ast.parse(content, filename=path))
            parsed_files[path] = _ParsedFile(
                visitor.symbols, visitor.calls, visitor.imported_modules, visitor.imported_names
            )
            for symbol in visitor.symbols:
                key = (symbol.module, symbol.node.qualname)
                if key in index or symbol.node.id in by_id:
                    raise ValueError("Duplicate function definition: {}".format(symbol.node.id))
                index[key] = symbol
                by_id[symbol.node.id] = symbol

        edges: List[Edge] = []
        unresolved: List[UnresolvedCall] = []
        for path, parsed in sorted(parsed_files.items()):
            for call in parsed.calls:
                target = _resolve(call, by_id[call.caller], parsed, index)
                if target:
                    edges.append(Edge(call.caller, target, path, call.line))
                else:
                    unresolved.append(UnresolvedCall(call.caller, call.expression, path, call.line))
        return GraphFacts([symbol.node for symbol in by_id.values()], edges, unresolved)
