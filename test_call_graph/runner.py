"""Execute Go tests selected by a validated, file-keyed impact plan."""

import argparse
import json
import os
import re
import subprocess
import sys
import unicodedata
from collections import defaultdict
from pathlib import Path, PurePosixPath


def _is_go_test_name(name):
    if not isinstance(name, str) or not name.startswith("Test") or len(name) <= len("Test"):
        return False
    suffix = name[len("Test"):]
    return (
        not suffix[0].islower()
        and all(
            char == "_"
            or unicodedata.category(char).startswith("L")
            or unicodedata.category(char) == "Nd"
            for char in suffix
        )
    )


def _go_environment(go_command, repository):
    version_result = subprocess.run(
        [go_command, "version"],
        capture_output=True,
        check=False,
        cwd=str(repository),
        text=True,
    )
    if version_result.returncode:
        raise RuntimeError("could not read Go version: {}".format(version_result.stderr.strip()))
    version_fields = version_result.stdout.split()
    if len(version_fields) < 4 or version_fields[:2] != ["go", "version"]:
        raise RuntimeError("could not parse Go version output")
    if version_fields[2] == "devel":
        if len(version_fields) < 5:
            raise RuntimeError("could not parse development Go version output")
        version = "devel " + version_fields[3]
        platform = version_fields[4]
    else:
        version = version_fields[2]
        platform = version_fields[3]
    if "/" not in platform:
        raise RuntimeError("could not parse Go platform from version output")
    goos, goarch = platform.split("/", 1)

    env_result = subprocess.run(
        [go_command, "env", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS"],
        capture_output=True,
        check=False,
        cwd=str(repository),
        text=True,
    )
    if env_result.returncode:
        raise RuntimeError(
            "could not read Go build environment: {}".format(env_result.stderr.strip())
        )
    env_values = env_result.stdout.splitlines()
    if len(env_values) != 4 or env_values[2] not in ("0", "1"):
        raise RuntimeError("could not parse Go build environment")
    if env_values[3].strip():
        raise ValueError("GOFLAGS must be empty for scoped test execution")
    return {
        "go_version": version,
        "goos": env_values[0],
        "goarch": env_values[1],
        "cgo_enabled": env_values[2] == "1",
    }


def _build_tags(analysis_context):
    tags = analysis_context.get("build_tags")
    if not isinstance(tags, list) or any(
        not isinstance(tag, str)
        or not tag
        or not all(char.isascii() and (char.isalnum() or char in "_.") for char in tag)
        for tag in tags
    ):
        raise ValueError("plan analysis_context must contain valid Go build_tags")
    return sorted(set(tags))


def _safe_test_map(test_map, repository, field):
    if not isinstance(test_map, dict):
        raise ValueError("{} must be an object keyed by test-file path".format(field))

    result = {}
    repository = repository.resolve()
    for raw_path, names in test_map.items():
        if not isinstance(raw_path, str) or not raw_path:
            raise ValueError("{} contains an invalid test-file path".format(field))
        path = PurePosixPath(raw_path)
        if (
            path.is_absolute()
            or ".." in path.parts
            or path.as_posix() != raw_path
            or "\\" in raw_path
            or not raw_path.endswith("_test.go")
        ):
            raise ValueError("unsafe Go test-file path: {}".format(raw_path))
        if not isinstance(names, list) or not names:
            raise ValueError("test names must be a non-empty array for {}".format(raw_path))
        if any(not _is_go_test_name(name) for name in names):
            raise ValueError("invalid Go test name in {}".format(raw_path))
        if len(names) != len(set(names)):
            raise ValueError("duplicate Go test names for {}".format(raw_path))

        candidate = repository
        for part in path.parts:
            candidate = candidate / part
            if candidate.is_symlink():
                raise ValueError("Go test path contains a symbolic link: {}".format(raw_path))
        test_file = candidate.resolve()
        try:
            test_file.relative_to(repository)
        except ValueError:
            raise ValueError("Go test file resolves outside the repository: {}".format(raw_path))
        if not test_file.is_file():
            raise ValueError("Go test file does not exist: {}".format(raw_path))
        result[raw_path] = set(names)
    return result


def _test_ids(test_map):
    return {
        "{}::{}".format(path, name)
        for path, names in test_map.items()
        for name in names
    }


def _selected_packages(test_map):
    packages = defaultdict(set)
    for path, names in test_map.items():
        package = PurePosixPath(path).parent.as_posix()
        package = "." if package == "." else "./" + package
        packages[package].update(names)
    return packages


def _validated_build_tags(plan):
    context = plan.get("analysis_context")
    if not isinstance(context, dict):
        return []
    try:
        return _build_tags(context)
    except ValueError:
        return []


def _run_full_suite(go_command, repository, reason, build_tags=()):
    print("running the full Go suite: {}".format(reason), file=sys.stderr)
    command = [go_command, "test", "-count=1"]
    if build_tags:
        command.append("-tags=" + ",".join(build_tags))
    command.append("./...")

    # GOFLAGS can contain a hidden -run selector, which would make a supposed
    # full-suite fallback selective. Keep fallback behavior unconditional.
    environment = os.environ.copy()
    environment.pop("GOFLAGS", None)
    return subprocess.run(command, cwd=str(repository), env=environment, check=False).returncode


def _selective_plan(plan, repository, go_command):
    if not isinstance(plan, dict) or type(plan.get("schema_version")) is not int:
        raise ValueError("missing or unsupported structured plan")
    if plan["schema_version"] != 1:
        raise ValueError("unsupported structured plan schema")
    if plan.get("language") != "go":
        raise ValueError("selective execution is supported only for Go plans")
    if type(plan.get("run_full_suite")) is not bool:
        raise ValueError("plan must include a boolean run_full_suite field")
    if plan["run_full_suite"]:
        raise ValueError("planner requires the full suite: {}".format(
            plan.get("reason") or "no reason provided"
        ))

    run_map = _safe_test_map(plan.get("tests_to_run"), repository, "tests_to_run")
    skip_map = _safe_test_map(plan.get("tests_to_skip"), repository, "tests_to_skip")
    all_map = _safe_test_map(plan.get("all_tests_by_file"), repository, "all_tests_by_file")
    candidate_map = _safe_test_map(
        plan.get("candidate_tests_by_file"), repository, "candidate_tests_by_file"
    )

    run_ids = _test_ids(run_map)
    skip_ids = _test_ids(skip_map)
    all_ids = _test_ids(all_map)
    if run_ids & skip_ids:
        raise ValueError("test plan both runs and skips the same test")
    if (run_ids | skip_ids) != all_ids:
        raise ValueError("run and skip maps do not account for every test root")
    if not run_ids:
        raise ValueError("test plan selected no tests")
    if _test_ids(candidate_map) != run_ids:
        raise ValueError("candidate tests do not match tests_to_run")

    context = plan.get("analysis_context")
    if not isinstance(context, dict):
        raise ValueError("plan must include the Go analysis_context")
    expected_context = {
        "goos": context.get("goos"),
        "goarch": context.get("goarch"),
        "go_version": context.get("go_version"),
        "cgo_enabled": context.get("cgo_enabled"),
    }
    if (
        any(
            not isinstance(expected_context[key], str) or not expected_context[key]
            for key in ("goos", "goarch", "go_version")
        )
        or type(expected_context["cgo_enabled"]) is not bool
    ):
        raise ValueError("plan analysis_context is missing the Go toolchain context")
    build_tags = _build_tags(context)

    actual_context = _go_environment(go_command, repository)
    if any(actual_context[key] != expected_context[key] for key in expected_context):
        raise ValueError(
            "Go toolchain does not match the analyzed build context: expected {}, got {}".format(
                expected_context, actual_context
            )
        )
    return _selected_packages(run_map), build_tags


def run_plan(plan, repository, go_command="go"):
    repository = Path(repository).resolve()
    if not repository.is_dir():
        raise ValueError("repository is not a directory: {}".format(repository))

    if isinstance(plan, dict) and plan.get("run_full_suite") is True:
        build_tags = []
        if type(plan.get("schema_version")) is int and plan.get("schema_version") == 1:
            if plan.get("language") == "go":
                build_tags = _validated_build_tags(plan)
        return _run_full_suite(
            go_command,
            repository,
            "planner requires the full suite: {}".format(
                plan.get("reason") or "no reason provided"
            ),
            build_tags=build_tags,
        )

    try:
        packages, build_tags = _selective_plan(plan, repository, go_command)
    except (OSError, RuntimeError, TypeError, ValueError) as error:
        print("scoped selection is unsafe; falling back to the full suite: {}".format(error),
              file=sys.stderr)
        fallback_tags = []
        if (
            isinstance(plan, dict)
            and type(plan.get("schema_version")) is int
            and plan.get("schema_version") == 1
            and plan.get("language") == "go"
        ):
            fallback_tags = _validated_build_tags(plan)
        return _run_full_suite(go_command, repository, str(error), build_tags=fallback_tags)

    for package in sorted(packages):
        names = sorted(packages[package])
        test_pattern = "^({})$".format("|".join(re.escape(name) for name in names))
        command = [go_command, "test", "-count=1"]
        if build_tags:
            command.append("-tags=" + ",".join(build_tags))
        command.extend(["-run", test_pattern, package])
        print("Running {} in {}".format(", ".join(names), package))
        result = subprocess.run(command, cwd=str(repository), check=False)
        if result.returncode:
            return result.returncode
    return 0


def _read_plan(path):
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON object key: {}".format(key))
            result[key] = value
        return result

    try:
        with Path(path).open("r", encoding="utf-8") as plan_file:
            return json.load(plan_file, object_pairs_hook=unique_object), None
    except (OSError, UnicodeError, ValueError) as error:
        return None, "test plan is unavailable or invalid: {}".format(error)


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Run Go tests selected by a structured test-impact plan"
    )
    parser.add_argument("--plan", required=True, help="File-keyed JSON plan from plan_cli")
    parser.add_argument("--repository", default=".", help="Repository containing Go tests")
    parser.add_argument("--go-command", default="go", help="Go executable")
    args = parser.parse_args(argv)

    try:
        repository = Path(args.repository).resolve()
        plan, plan_error = _read_plan(args.plan)
        if plan_error:
            print("falling back to the full suite: {}".format(plan_error), file=sys.stderr)
            return _run_full_suite(args.go_command, repository, plan_error)
        return run_plan(plan, repository, args.go_command)
    except (OSError, RuntimeError, TypeError, ValueError) as error:
        print("scoped test run failed: {}".format(error), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
