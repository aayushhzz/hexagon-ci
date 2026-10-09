"""Run Python unittest cases selected by a validated file-keyed impact plan."""

import argparse
import json
import subprocess
import sys
import unittest
from pathlib import Path, PurePosixPath


def _flatten_suite(suite):
    cases = []
    for item in suite:
        if isinstance(item, unittest.TestSuite):
            cases.extend(_flatten_suite(item))
        else:
            cases.append(item)
    return cases


def _safe_test_map(test_map, root: Path, test_dir: str):
    if not isinstance(test_map, dict):
        raise ValueError("test map must be an object keyed by repository-relative file path")
    result = {}
    test_root = PurePosixPath(test_dir)
    if test_root.is_absolute() or ".." in test_root.parts:
        raise ValueError("configured unittest directory is unsafe")

    for raw_path, names in test_map.items():
        if not isinstance(raw_path, str) or not raw_path:
            raise ValueError("test map contains an invalid file path")
        path = PurePosixPath(raw_path)
        if path.is_absolute() or ".." in path.parts or path.as_posix() != raw_path or path.suffix != ".py":
            raise ValueError("unsafe test file path: {}".format(raw_path))
        try:
            relative_to_test_root = path.relative_to(test_root)
        except ValueError:
            raise ValueError("test file is outside the configured unittest directory: {}".format(raw_path))
        if not relative_to_test_root.parts:
            raise ValueError("test file path does not name a Python file: {}".format(raw_path))

        file_path = root.joinpath(*path.parts)
        resolved_path = file_path.resolve()
        try:
            resolved_path.relative_to(root.resolve())
        except ValueError:
            raise ValueError("test file resolves outside the repository: {}".format(raw_path))
        if not resolved_path.is_file():
            raise ValueError("test file does not exist: {}".format(raw_path))

        if not isinstance(names, list) or not names:
            raise ValueError("test names must be a non-empty array for {}".format(raw_path))
        module_parts = list(relative_to_test_root.with_suffix("").parts)
        if module_parts[-1] == "__init__":
            module_parts.pop()
        if not module_parts or not all(part.isidentifier() for part in module_parts):
            raise ValueError("test file cannot be imported by unittest: {}".format(raw_path))
        module = ".".join(module_parts)

        ids = []
        for name in names:
            if not isinstance(name, str) or not name or not all(
                part.isidentifier() for part in name.split(".")
            ):
                raise ValueError("test name cannot be imported by unittest: {}".format(name))
            ids.append(module + "." + name)
        if len(ids) != len(set(ids)):
            raise ValueError("test map contains duplicate test names for {}".format(raw_path))
        if set(ids) & set(result):
            raise ValueError("test map refers to the same unittest from multiple files")
        result.update((test_id, raw_path) for test_id in ids)
    return result


def _select_cases(plan, cases, root: Path, test_dir: str):
    """Return selected cases, or None and a reason to run the full suite."""
    if not isinstance(plan, dict) or plan.get("schema_version") != 1:
        return None, "missing or unsupported test plan schema"
    if plan.get("language") != "python":
        return None, "unsupported plan language for the Python unittest runner"
    if plan.get("run_full_suite") is True:
        return None, "planner requires the full suite: {}".format(plan.get("reason", "no reason provided"))
    if plan.get("run_full_suite") is not False:
        return None, "test plan has no valid run_full_suite decision"

    try:
        run_map = _safe_test_map(plan.get("tests_to_run"), root, test_dir)
        skip_map = _safe_test_map(plan.get("tests_to_skip"), root, test_dir)
    except (TypeError, ValueError, OSError) as error:
        return None, "invalid test plan: {}".format(error)

    overlap = set(run_map) & set(skip_map)
    if overlap:
        return None, "test plan both runs and skips the same test"
    if not run_map:
        return None, "test plan selected no tests; use the full suite"

    case_ids = [case.id() for case in cases]
    discovered = set(case_ids)
    if len(discovered) != len(case_ids):
        return None, "unittest discovery returned duplicate test IDs"
    planned = set(run_map) | set(skip_map)
    if planned != discovered:
        return None, "test plan and unittest discovery do not account for every discovered test"

    cases_by_id = {case.id(): case for case in cases}
    return [cases_by_id[test_id] for test_id in sorted(run_map)], None


def _read_plan(path):
    if not path:
        return None, "test plan was not provided"
    try:
        with Path(path).open("r", encoding="utf-8") as plan_file:
            return json.load(plan_file), None
    except (OSError, UnicodeError, ValueError) as error:
        return None, "test plan is unavailable or invalid: {}".format(error)


def _append_summary(path, mode, reason, test_count):
    if not path:
        return
    content = (
        "\n## Test runner\n\n"
        "- **Execution mode:** {}\n"
        "- **Tests executed:** {}\n"
        "- **Reason:** {}\n"
    ).format(mode, test_count, reason)
    with Path(path).open("a", encoding="utf-8") as summary_file:
        summary_file.write(content)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="Run tests from a validated impact plan")
    parser.add_argument("--root", default=".", help="Repository root")
    parser.add_argument("--plan", help="File-keyed plan JSON; missing or invalid plans run the full suite")
    parser.add_argument("--test-dir", default="tests", help="Directory used by unittest discovery")
    parser.add_argument("--summary-file", help="Append the execution result to a Markdown summary")
    args = parser.parse_args(argv)

    root = Path(args.root).resolve()
    test_start = root / args.test_dir
    try:
        discovered_suite = unittest.TestLoader().discover(
            start_dir=str(test_start), top_level_dir=str(test_start)
        )
        all_cases = _flatten_suite(discovered_suite)
    except (ImportError, OSError, RuntimeError, TypeError, ValueError) as error:
        print("unittest discovery failed; running the full suite: {}".format(error), file=sys.stderr)
        return subprocess.run(
            [sys.executable, "-m", "unittest", "discover", "-s", args.test_dir, "-v"],
            cwd=str(root),
        ).returncode

    plan, plan_error = _read_plan(args.plan)
    if plan is None:
        selected_cases, reason = None, plan_error
    else:
        selected_cases, reason = _select_cases(plan, all_cases, root, args.test_dir)

    if selected_cases is None:
        cases_to_run = all_cases
        mode = "full suite"
    else:
        cases_to_run = selected_cases
        mode = "selective"

    result = unittest.TextTestRunner(verbosity=2).run(unittest.TestSuite(cases_to_run))
    report = {
        "execution_mode": mode,
        "reason": reason or "validated file-keyed plan",
        "test_count": len(cases_to_run),
        "tests": [case.id() for case in cases_to_run],
    }
    print(json.dumps(report, ensure_ascii=False, sort_keys=True))
    try:
        _append_summary(args.summary_file, mode, report["reason"], len(cases_to_run))
    except OSError as error:
        print("could not update the Actions summary: {}".format(error), file=sys.stderr)
        return 1
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    sys.exit(main())
