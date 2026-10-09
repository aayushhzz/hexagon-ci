import json
import tempfile
import unittest
from pathlib import Path

from test_call_graph import build_graph, rebuild_to_file
from test_call_graph.python_frontend import PythonFrontend


class FullRebuildTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.output = self.root / "graph.json"

    def write(self, path, content):
        destination = self.root / path
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(content, encoding="utf-8")

    def make_chain(self):
        self.write("helpers.py", "def end():\n    pass\n\ndef middle():\n    end()\n")
        self.write("app.py", "from helpers import middle as step\n\ndef start():\n    step()\n")
        self.write("tests/test_app.py", "from app import start\n\ndef test_flow():\n    start()\n")

    def test_rebuild_reflects_inserted_and_deleted_intermediate_calls(self):
        self.make_chain()
        first = rebuild_to_file(self.root, self.output)
        root = "tests/test_app.py::test_flow"
        self.assertEqual(
            self.edges(first, root),
            {
                (root, "app.py::start"),
                ("app.py::start", "helpers.py::middle"),
                ("helpers.py::middle", "helpers.py::end"),
            },
        )

        self.write(
            "helpers.py",
            "def end():\n    pass\n\ndef inserted():\n    end()\n\ndef middle():\n    inserted()\n",
        )
        second = rebuild_to_file(self.root, self.output)
        self.assertIn(("helpers.py::middle", "helpers.py::inserted"), self.edges(second, root))
        self.assertIn(("helpers.py::inserted", "helpers.py::end"), self.edges(second, root))
        self.assertNotIn(("helpers.py::middle", "helpers.py::end"), self.edges(second, root))
        self.assertNotEqual(first["source_fingerprint"], second["source_fingerprint"])
        self.assertEqual(second, json.loads(self.output.read_text(encoding="utf-8")))

        self.write("helpers.py", "def end():\n    pass\n\ndef middle():\n    end()\n")
        third = rebuild_to_file(self.root, self.output)
        self.assertNotIn("helpers.py::inserted", third["tests"][root]["nodes"])
        self.assertEqual(self.edges(third, root), self.edges(first, root))

    def test_deleted_source_and_added_test_do_not_leave_stale_nodes(self):
        self.make_chain()
        rebuild_to_file(self.root, self.output)
        self.write(
            "tests/test_app.py",
            "from app import start\n\ndef test_flow():\n    start()\n\ndef test_second():\n    start()\n",
        )
        added = rebuild_to_file(self.root, self.output)
        self.assertIn("tests/test_app.py::test_second", added["tests"])

        (self.root / "helpers.py").unlink()
        self.write("tests/test_app.py", "from app import start\n\ndef test_second():\n    start()\n")
        deleted = rebuild_to_file(self.root, self.output)
        self.assertNotIn("tests/test_app.py::test_flow", deleted["tests"])
        self.assertNotIn("helpers.py::middle", {node["id"] for node in deleted["nodes"]})
        self.assertNotIn("helpers.py::middle", deleted["tests"]["tests/test_app.py::test_second"]["nodes"])
        self.assertTrue(any(call["expression"] == "step" for call in deleted["unresolved_calls"]))

    def test_cycles_and_method_calls_are_included_once(self):
        self.write(
            "test_worker.py",
            "class Worker:\n"
            "    def test_run(self):\n        self.first()\n"
            "    def first(self):\n        self.second()\n"
            "    def second(self):\n        self.first()\n",
        )
        graph = build_graph(self.root)
        root = "test_worker.py::Worker.test_run"
        self.assertEqual(
            self.edges(graph, root),
            {
                (root, "test_worker.py::Worker.first"),
                ("test_worker.py::Worker.first", "test_worker.py::Worker.second"),
                ("test_worker.py::Worker.second", "test_worker.py::Worker.first"),
            },
        )
        self.assertEqual(len(graph["tests"][root]["nodes"]), 3)

    def test_removing_one_branch_retains_an_alternate_path(self):
        self.write(
            "flow.py",
            "def shared():\n    pass\n"
            "\ndef left():\n    shared()\n"
            "\ndef right():\n    shared()\n"
            "\ndef entry():\n    left()\n    right()\n",
        )
        self.write("test_flow.py", "from flow import entry\n\ndef test_flow():\n    entry()\n")
        root = "test_flow.py::test_flow"
        initial = build_graph(self.root)
        self.assertIn(("flow.py::left", "flow.py::shared"), self.edges(initial, root))

        self.write(
            "flow.py",
            "def shared():\n    pass\n"
            "\ndef left():\n    pass\n"
            "\ndef right():\n    shared()\n"
            "\ndef entry():\n    left()\n    right()\n",
        )
        updated = build_graph(self.root)
        self.assertNotIn(("flow.py::left", "flow.py::shared"), self.edges(updated, root))
        self.assertIn(("flow.py::right", "flow.py::shared"), self.edges(updated, root))
        self.assertIn("flow.py::shared", updated["tests"][root]["nodes"])

    def test_package_relative_import_and_module_alias(self):
        self.write("pkg/__init__.py", "")
        self.write("pkg/deps.py", "def finish():\n    pass\n")
        self.write("pkg/worker.py", "from .deps import finish\n\ndef run():\n    finish()\n")
        self.write("test_pkg.py", "import pkg.worker as worker\n\ndef test_run():\n    worker.run()\n")
        graph = build_graph(self.root)
        root = "test_pkg.py::test_run"
        self.assertEqual(
            self.edges(graph, root),
            {
                (root, "pkg/worker.py::run"),
                ("pkg/worker.py::run", "pkg/deps.py::finish"),
            },
        )

    def test_failed_parse_preserves_last_complete_output(self):
        self.make_chain()
        rebuild_to_file(self.root, self.output)
        previous = self.output.read_bytes()
        self.write("broken.py", "def broken(:\n")
        with self.assertRaises(SyntaxError):
            rebuild_to_file(self.root, self.output)
        self.assertEqual(self.output.read_bytes(), previous)

    def test_change_during_scan_does_not_publish_partial_snapshot(self):
        self.make_chain()
        rebuild_to_file(self.root, self.output)
        previous = self.output.read_bytes()

        class ChangingFrontend(PythonFrontend):
            def analyze(inner_self, sources):
                self.write("later.py", "def added():\n    pass\n")
                return super().analyze(sources)

        with self.assertRaisesRegex(RuntimeError, "changed during the rebuild"):
            rebuild_to_file(self.root, self.output, ChangingFrontend())
        self.assertEqual(self.output.read_bytes(), previous)

    @staticmethod
    def edges(graph, root):
        return {(edge["caller"], edge["callee"]) for edge in graph["tests"][root]["edges"]}


if __name__ == "__main__":
    unittest.main()
