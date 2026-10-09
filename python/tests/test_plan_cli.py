import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

from test_call_graph.plan_cli import main


class PlanCliTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.previous = self.root / "previous.json"
        self.current = self.root / "current.json"
        self.changed = self.root / "changed.txt"
        self.summary = self.root / "summary.md"

    def test_outputs_file_keyed_run_and_skip_maps_and_selective_summary(self):
        self.previous.write_text(json.dumps(self.graph("before")), encoding="utf-8")
        self.current.write_text(json.dumps(self.graph("after")), encoding="utf-8")
        self.changed.write_text("lib.go\n", encoding="utf-8")

        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            result = main([
                "--current-graph", str(self.current),
                "--previous-graph", str(self.previous),
                "--changed-files", str(self.changed),
                "--summary-file", str(self.summary),
            ])

        report = json.loads(stdout.getvalue())
        self.assertEqual(result, 0)
        self.assertEqual(report["schema_version"], 1)
        self.assertEqual(report["language"], "go")
        self.assertEqual(
            report["analysis_context"],
            {
                "goos": "linux",
                "goarch": "amd64",
                "go_version": "go1.24.3",
                "cgo_enabled": True,
                "build_tags": ["integration"],
            },
        )
        self.assertFalse(report["run_full_suite"])
        self.assertEqual(report["tests_to_run"], {"app_test.go": ["TestAffected"]})
        self.assertEqual(report["tests_to_skip"], {"app_test.go": ["TestUnrelated"]})
        self.assertEqual(
            report["all_tests_by_file"],
            {"app_test.go": ["TestAffected", "TestUnrelated"]},
        )
        self.assertEqual(report["candidate_tests_by_file"], {"app_test.go": ["TestAffected"]})
        summary = self.summary.read_text(encoding="utf-8")
        self.assertIn("Tests to run by file", summary)
        self.assertIn("Tests to skip by file", summary)
        self.assertIn("TestAffected", summary)
        self.assertIn("TestUnrelated", summary)

    def test_full_suite_fallback_is_explicit_in_machine_and_markdown_reports(self):
        self.current.write_text(json.dumps(self.graph("after")), encoding="utf-8")
        self.changed.write_text("lib.go\n", encoding="utf-8")

        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            result = main([
                "--current-graph", str(self.current),
                "--changed-files", str(self.changed),
                "--summary-file", str(self.summary),
            ])

        report = json.loads(stdout.getvalue())
        summary = self.summary.read_text(encoding="utf-8")
        self.assertEqual(result, 0)
        self.assertTrue(report["run_full_suite"])
        self.assertIn("full suite required", summary.lower())
        self.assertIn("do not skip tests", summary.lower())
        self.assertEqual(
            report["tests_to_run"],
            {"app_test.go": ["TestAffected", "TestUnrelated"]},
        )
        self.assertEqual(report["tests_to_skip"], {})
        self.assertEqual(
            report["all_tests_by_file"],
            {"app_test.go": ["TestAffected", "TestUnrelated"]},
        )
        self.assertEqual(
            report["candidate_tests_by_file"],
            {"app_test.go": ["TestAffected", "TestUnrelated"]},
        )

    @staticmethod
    def graph(function_hash):
        root = "app_test.go::TestAffected"
        other = "app_test.go::TestUnrelated"
        return {
            "format_version": 3,
            "language": "go",
            "source_fingerprint": "snapshot-" + function_hash,
            "analysis_context": {
                "goos": "linux",
                "goarch": "amd64",
                "go_version": "go1.24.3",
                "cgo_enabled": True,
                "build_tags": ["integration"],
            },
            "files": [
                {"path": "lib.go", "package": "example", "imports": []},
                {"path": "app_test.go", "package": "example", "imports": []},
            ],
            "source_files": [
                {"path": "lib.go", "non_function_hash": "stable-lib"},
                {"path": "app_test.go", "non_function_hash": "stable-test"},
            ],
            "nodes": [
                {
                    "id": "lib.go::Changed",
                    "path": "lib.go",
                    "qualname": "Changed",
                    "source_hash": function_hash,
                },
                {
                    "id": root,
                    "path": "app_test.go",
                    "qualname": "TestAffected",
                    "source_hash": "stable-root",
                    "is_test": True,
                },
                {
                    "id": other,
                    "path": "app_test.go",
                    "qualname": "TestUnrelated",
                    "source_hash": "stable-other",
                    "is_test": True,
                },
            ],
            "tests": {
                root: {"nodes": [root, "lib.go::Changed"], "unresolved_calls": []},
                other: {"nodes": [other], "unresolved_calls": []},
            },
        }


if __name__ == "__main__":
    unittest.main()
