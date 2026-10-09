import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from test_call_graph.plan_runner import _select_cases


class _NamedCase:
    def __init__(self, test_id):
        self._test_id = test_id

    def id(self):
        return self._test_id


class PlanRunnerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.test_file = self.root / "tests" / "test_example.py"
        self.test_file.parent.mkdir()
        self.test_file.write_text("# discovered by unittest\n", encoding="utf-8")
        self.selected = _NamedCase("test_example.ExampleTests.test_selected")
        self.skipped = _NamedCase("test_example.ExampleTests.test_skipped")
        self.cases = [self.selected, self.skipped]

    def test_file_keyed_plan_selects_only_the_listed_unittest_case(self):
        plan = self.plan(
            tests_to_run={"tests/test_example.py": ["ExampleTests.test_selected"]},
            tests_to_skip={"tests/test_example.py": ["ExampleTests.test_skipped"]},
        )

        selected, reason = _select_cases(plan, self.cases, self.root, "tests")

        self.assertIsNone(reason)
        self.assertEqual([case.id() for case in selected], [self.selected.id()])

    def test_cli_executes_only_the_test_named_in_a_valid_file_keyed_plan(self):
        test_dir = self.root / "tests"
        test_dir.mkdir(exist_ok=True)
        (test_dir / "test_sample.py").write_text(
            "import unittest\n"
            "class SampleTests(unittest.TestCase):\n"
            "    def test_selected(self):\n        pass\n"
            "    def test_skipped(self):\n        raise AssertionError('skipped test ran')\n",
            encoding="utf-8",
        )
        plan_path = self.root / "plan.json"
        plan_path.write_text(json.dumps(self.plan(
            tests_to_run={"tests/test_sample.py": ["SampleTests.test_selected"]},
            tests_to_skip={"tests/test_sample.py": ["SampleTests.test_skipped"]},
        )), encoding="utf-8")
        repository = Path(__file__).resolve().parents[1]

        result = subprocess.run(
            [
                sys.executable,
                "-m",
                "test_call_graph.plan_runner",
                "--root",
                str(self.root),
                "--plan",
                str(plan_path),
            ],
            cwd=str(repository),
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout.strip().splitlines()[-1])
        self.assertEqual(report["execution_mode"], "selective")
        self.assertEqual(report["test_count"], 1)
        self.assertEqual(report["tests"], ["test_sample.SampleTests.test_selected"])

    def test_incomplete_plan_falls_back_instead_of_omitting_a_discovered_test(self):
        plan = self.plan(
            tests_to_run={"tests/test_example.py": ["ExampleTests.test_selected"]},
            tests_to_skip={},
        )

        selected, reason = _select_cases(plan, self.cases, self.root, "tests")

        self.assertIsNone(selected)
        self.assertIn("do not account for every discovered test", reason)

    def test_unsafe_path_falls_back_to_the_full_suite(self):
        plan = self.plan(
            tests_to_run={"../outside.py": ["ExampleTests.test_selected"]},
            tests_to_skip={"tests/test_example.py": ["ExampleTests.test_skipped"]},
        )

        selected, reason = _select_cases(plan, self.cases, self.root, "tests")

        self.assertIsNone(selected)
        self.assertIn("unsafe test file path", reason)

    def test_go_plan_is_not_interpreted_by_the_python_unittest_runner(self):
        plan = self.plan(
            language="go",
            tests_to_run={"tests/test_example.py": ["ExampleTests.test_selected"]},
            tests_to_skip={"tests/test_example.py": ["ExampleTests.test_skipped"]},
        )

        selected, reason = _select_cases(plan, self.cases, self.root, "tests")

        self.assertIsNone(selected)
        self.assertIn("unsupported plan language", reason)

    def plan(self, **overrides):
        plan = {
            "schema_version": 1,
            "language": "python",
            "run_full_suite": False,
            "tests_to_run": {},
            "tests_to_skip": {},
        }
        plan.update(overrides)
        return plan


if __name__ == "__main__":
    unittest.main()
