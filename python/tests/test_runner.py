import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from test_call_graph.runner import main


class ScopedRunnerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.plan_file = self.root / "plan.json"
        self.log_file = self.root / "go-arguments.txt"
        self.context_log = self.root / "go-context-directories.txt"
        self.env_log = self.root / "go-test-env.txt"
        self.go = self.root / "bin" / "go"
        self.go.parent.mkdir()
        self.go.write_text(
            "#!/bin/sh\n"
            "if [ \"$1\" = version ]; then pwd >> \"$GO_CONTEXT_LOG\"; printf 'go version go1.24.3 linux/amd64\\n'; exit 0; fi\n"
            "if [ \"$1\" = env ]; then pwd >> \"$GO_CONTEXT_LOG\"; printf 'linux\\namd64\\n1\\n%s\\n' \"$GOFLAGS\"; exit 0; fi\n"
            "printf '%s\\n' \"$*\" >> \"$GO_TEST_LOG\"\n"
            "printf '%s\\n' \"$GOFLAGS\" >> \"$GO_TEST_ENV_LOG\"\n",
            encoding="utf-8",
        )
        self.go.chmod(0o755)
        for directory, name in (("pkg", "suite_test.go"), ("other", "other_test.go")):
            package = self.root / directory
            package.mkdir()
            (package / name).write_text("package tests\n", encoding="utf-8")

    def test_runs_only_selected_names_grouped_by_package_from_file_keyed_plan(self):
        self.write_plan({
            "tests_to_run": {
                "pkg/suite_test.go": ["TestWanted", "TestAlsoWanted"],
                "other/other_test.go": ["TestOther"],
            },
            "tests_to_skip": {"pkg/suite_test.go": ["TestNotSelected"]},
            "all_tests_by_file": {
                "pkg/suite_test.go": ["TestAlsoWanted", "TestNotSelected", "TestWanted"],
                "other/other_test.go": ["TestOther"],
            },
            "candidate_tests_by_file": {
                "pkg/suite_test.go": ["TestAlsoWanted", "TestWanted"],
                "other/other_test.go": ["TestOther"],
            },
            "run_full_suite": False,
        })

        with self.go_log():
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            [
                "test -count=1 -run ^(TestOther)$ ./other",
                "test -count=1 -run ^(TestAlsoWanted|TestWanted)$ ./pkg",
            ],
        )

    def test_accepts_go_test_names_starting_with_digits_or_underscores(self):
        self.write_plan({
            "tests_to_run": {"pkg/suite_test.go": ["Test2", "Test_Thing"]},
            "tests_to_skip": {"other/other_test.go": ["TestSkipped"]},
            "all_tests_by_file": {
                "pkg/suite_test.go": ["Test2", "Test_Thing"],
                "other/other_test.go": ["TestSkipped"],
            },
            "candidate_tests_by_file": {"pkg/suite_test.go": ["Test2", "Test_Thing"]},
            "run_full_suite": False,
        })

        with self.go_log():
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 -run ^(Test2|Test_Thing)$ ./pkg"],
        )

    def test_runs_selected_tests_with_the_analyzed_build_tags(self):
        self.write_plan({
            "tests_to_run": {"pkg/suite_test.go": ["TestWanted"]},
            "tests_to_skip": {"pkg/suite_test.go": ["TestNotSelected"]},
            "all_tests_by_file": {
                "pkg/suite_test.go": ["TestNotSelected", "TestWanted"],
            },
            "candidate_tests_by_file": {"pkg/suite_test.go": ["TestWanted"]},
            "run_full_suite": False,
            "analysis_context": {"build_tags": ["alt"]},
        })

        with self.go_log():
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 -tags=alt -run ^(TestWanted)$ ./pkg"],
        )

    def test_reads_go_context_from_the_target_repository(self):
        self.write_plan(self.selective_plan())

        with self.go_log():
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertEqual(
            self.context_log.read_text(encoding="utf-8").splitlines(),
            [str(self.root.resolve()), str(self.root.resolve())],
        )

    def test_rejects_a_different_go_version_by_running_the_full_suite(self):
        plan = self.selective_plan()
        plan["analysis_context"]["go_version"] = "go1.25.0"
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()) as stderr:
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("Go toolchain does not match", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def test_effective_goflags_disable_selection_and_are_cleared_for_full_suite(self):
        self.write_plan(self.selective_plan())

        with (
            self.go_log(),
            patch.dict(os.environ, {"GOFLAGS": "-run=TestUnexpected"}),
            contextlib.redirect_stderr(io.StringIO()) as stderr,
        ):
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("GOFLAGS", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )
        self.assertEqual(self.env_log.read_text(encoding="utf-8").splitlines(), [""])

    def test_full_suite_plan_runs_every_go_test_package(self):
        plan = self.selective_plan()
        plan["run_full_suite"] = True
        plan["reason"] = "analysis is incomplete"
        plan["analysis_context"]["build_tags"] = ["alt"]
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()) as stderr:
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("planner requires the full suite", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 -tags=alt ./..."],
        )

    def test_path_traversal_invalidates_selection_and_runs_full_suite(self):
        plan = self.selective_plan()
        plan["tests_to_run"] = {"../outside_test.go": ["TestInjected"]}
        plan["candidate_tests_by_file"] = {"../outside_test.go": ["TestInjected"]}
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()) as stderr:
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("unsafe Go test-file path", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def test_invalid_plan_fallback_preserves_valid_analyzed_build_tags(self):
        plan = self.selective_plan()
        plan["analysis_context"]["build_tags"] = ["integration"]
        plan["tests_to_skip"] = {"../outside_test.go": ["TestSkipped"]}
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()):
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 -tags=integration ./..."],
        )

    def test_go_invalid_identifier_invalidates_selection(self):
        invalid_name = "Test\u0301"
        plan = self.selective_plan()
        plan["tests_to_run"] = {"pkg/suite_test.go": [invalid_name]}
        plan["candidate_tests_by_file"] = {"pkg/suite_test.go": [invalid_name]}
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()) as stderr:
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("invalid Go test name", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def test_symlink_test_file_invalidates_selection_and_runs_full_suite(self):
        alias = self.root / "pkg" / "alias_test.go"
        alias.symlink_to(self.root / "other" / "other_test.go")
        plan = self.selective_plan()
        plan["tests_to_skip"]["pkg/alias_test.go"] = ["TestAliased"]
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()) as stderr:
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("symbolic link", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def test_overlapping_run_and_skip_maps_invalidate_selection(self):
        plan = self.selective_plan()
        plan["tests_to_skip"]["pkg/suite_test.go"] = ["TestWanted"]
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()) as stderr:
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("both runs and skips", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def test_incomplete_run_and_skip_maps_invalidate_selection(self):
        plan = self.selective_plan()
        plan["tests_to_skip"] = {}
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()) as stderr:
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("do not account for every test root", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def test_inconsistent_candidate_map_invalidates_selection(self):
        plan = self.selective_plan()
        plan["candidate_tests_by_file"] = {"pkg/suite_test.go": ["TestDifferent"]}
        self.write_plan(plan)

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()):
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def test_empty_safe_selection_runs_the_full_suite(self):
        self.write_plan({
            "tests_to_run": {},
            "tests_to_skip": {},
            "all_tests_by_file": {},
            "candidate_tests_by_file": {},
            "run_full_suite": False,
        })

        with self.go_log():
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def test_unreadable_or_invalid_plan_runs_the_full_suite(self):
        self.plan_file.write_text("not JSON", encoding="utf-8")

        with self.go_log(), contextlib.redirect_stderr(io.StringIO()) as stderr:
            result = self.invoke()

        self.assertEqual(result, 0)
        self.assertIn("full suite", stderr.getvalue())
        self.assertEqual(
            self.log_file.read_text(encoding="utf-8").splitlines(),
            ["test -count=1 ./..."],
        )

    def selective_plan(self):
        return {
            "schema_version": 1,
            "language": "go",
            "tests_to_run": {"pkg/suite_test.go": ["TestWanted"]},
            "tests_to_skip": {"pkg/suite_test.go": ["TestNotSelected"]},
            "all_tests_by_file": {
                "pkg/suite_test.go": ["TestNotSelected", "TestWanted"],
            },
            "candidate_tests_by_file": {"pkg/suite_test.go": ["TestWanted"]},
            "run_full_suite": False,
            "analysis_context": {
                "goos": "linux",
                "goarch": "amd64",
                "go_version": "go1.24.3",
                "cgo_enabled": True,
                "build_tags": [],
            },
        }

    def write_plan(self, plan):
        if plan.get("run_full_suite") is False:
            defaults = dict(self.selective_plan()["analysis_context"])
            defaults.update(plan.get("analysis_context", {}))
            plan["analysis_context"] = defaults
            plan.setdefault("schema_version", 1)
            plan.setdefault("language", "go")
        self.plan_file.write_text(json.dumps(plan), encoding="utf-8")

    def invoke(self):
        return main([
            "--plan", str(self.plan_file),
            "--repository", str(self.root),
            "--go-command", str(self.go),
        ])

    @contextlib.contextmanager
    def go_log(self):
        previous = {
            "GO_TEST_LOG": os.environ.get("GO_TEST_LOG"),
            "GO_CONTEXT_LOG": os.environ.get("GO_CONTEXT_LOG"),
            "GO_TEST_ENV_LOG": os.environ.get("GO_TEST_ENV_LOG"),
        }
        os.environ["GO_TEST_LOG"] = str(self.log_file)
        os.environ["GO_CONTEXT_LOG"] = str(self.context_log)
        os.environ["GO_TEST_ENV_LOG"] = str(self.env_log)
        try:
            yield
        finally:
            for name, value in previous.items():
                if value is None:
                    os.environ.pop(name, None)
                else:
                    os.environ[name] = value


if __name__ == "__main__":
    unittest.main()
