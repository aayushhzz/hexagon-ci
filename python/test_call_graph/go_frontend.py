"""Go source adapter backed by the standard-library Go AST parser."""

import base64
import hashlib
import json
import os
import subprocess
from pathlib import Path
from typing import Dict, Mapping, Optional, Union

from .model import Edge, GraphFacts, Node, UnresolvedCall


IGNORED_DIRS = {".git", "vendor", "node_modules", "build", "dist", "__pycache__"}


class GoFrontend:
    """Parse Go source without running Go tests or downloading module dependencies."""

    language = "go"
    source_extension = ".go"

    def __init__(self, helper: Optional[Union[str, Path]] = None, tags=()):
        self.helper = helper or os.environ.get("CALL_GRAPH_GO_HELPER")
        self.tags = sorted(set(tags))
        self.root = None

    def collect_sources(self, root: Path) -> Dict[str, bytes]:
        self.root = root
        sources = {}
        for directory, subdirs, files in os.walk(str(root)):
            subdirs[:] = sorted(
                name for name in subdirs
                if name not in IGNORED_DIRS and not name.startswith(".")
                and not Path(directory, name).is_symlink()
            )
            for name in sorted(files):
                source = Path(directory, name)
                if name.endswith(".go") and not source.is_symlink():
                    sources[source.relative_to(root).as_posix()] = source.read_bytes()
        module_file = root / "go.mod"
        if module_file.is_file():
            sources["go.mod"] = module_file.read_bytes()
        return sources

    def analyze(self, sources: Mapping[str, bytes]) -> GraphFacts:
        flags = subprocess.run(["go", "env", "GOFLAGS"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if flags.returncode:
            raise ValueError("Cannot determine effective GOFLAGS: {}".format(
                flags.stderr.decode("utf-8", "replace").strip()
            ))
        if flags.stdout.strip():
            raise ValueError("GOFLAGS are not applied by this analyzer; unset them and pass build tags with --go-tags")
        command = [str(self.helper)] if self.helper else [
            "go", "run", str(Path(__file__).with_name("go_source") / "main.go")
        ]
        encoded = json.dumps({
            "root": str(self.root) if self.root is not None else "",
            "tags": self.tags,
            "sources": {
                path: base64.b64encode(content).decode("ascii")
                for path, content in sources.items()
            }
        }).encode("utf-8")
        result = subprocess.run(command, input=encoded, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            raise ValueError("Go parser failed: {}".format(result.stderr.decode("utf-8", "replace").strip()))
        data = json.loads(result.stdout)
        helper_path = Path(self.helper) if self.helper else Path(__file__).with_name("go_source") / "main.go"
        analyzer = hashlib.sha256()
        for path in (helper_path, Path(__file__), Path(__file__).with_name("rebuild.py"),
                     Path(__file__).with_name("model.py")):
            analyzer.update(path.read_bytes())
        context = data["build_context"]
        context["analyzer_sha256"] = analyzer.hexdigest()
        context["selection_unsafe_reasons"] = data.get("selection_unsafe_reasons", [])
        source_files = {
            item["path"]: item["non_function_hash"]
            for item in data.get("source_files", [])
        }
        for path, content in sources.items():
            if path not in source_files:
                # Unselected build variants and module metadata cannot be
                # safely decomposed into function-only changes here.
                source_files[path] = hashlib.sha256(content).hexdigest()
        return GraphFacts(
            nodes=[Node(**node) for node in data["nodes"]],
            edges=[Edge(**edge) for edge in data["edges"]],
            unresolved_calls=[UnresolvedCall(**call) for call in data["unresolved_calls"]],
            analysis_context=context,
            files=data["files"],
            source_files=[
                {"path": path, "non_function_hash": digest}
                for path, digest in sorted(source_files.items())
            ],
        )
