import shutil
import os
import gzip
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from test_call_graph import build_graph, rebuild_to_file, plan_file_changes
from test_call_graph.go_frontend import GoFrontend


class GoRebuildTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        go = shutil.which("go")
        if not go:
            raise unittest.SkipTest("Go toolchain is unavailable")
        cls.helper_dir = tempfile.TemporaryDirectory()
        cls.helper = Path(cls.helper_dir.name) / "go-ast-helper"
        source = Path(__file__).resolve().parents[1] / "test_call_graph/go_source/main.go"
        subprocess.run([go, "build", "-o", str(cls.helper), str(source)], check=True)

    @classmethod
    def tearDownClass(cls):
        if hasattr(cls, "helper_dir"):
            cls.helper_dir.cleanup()

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.output = self.root / "graph.json"
        self.frontend = GoFrontend(self.helper)
        self.write("go.mod", "module example.com/demo\n\ngo 1.20\n")

    def write(self, path, content):
        file = self.root / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text(content, encoding="utf-8")

    def test_imported_call_chain_and_inserted_dependency(self):
        self.write("lib/end.go", "package lib\nfunc End() {}\n")
        self.write(
            "app/app.go",
            'package app\nimport "example.com/demo/lib"\nfunc Start() { lib.End() }\n',
        )
        self.write(
            "app/app_test.go",
            'package app\nimport "testing"\nfunc TestFlow(t *testing.T) { Start() }\n',
        )
        first = rebuild_to_file(self.root, self.output, self.frontend)
        root = "app/app_test.go::TestFlow"
        self.assertEqual(first["file_count"], 3)
        self.assertEqual(
            self.edges(first, root),
            {(root, "app/app.go::Start"), ("app/app.go::Start", "lib/end.go::End")},
        )

        self.write("lib/end.go", "package lib\nfunc End() {}\nfunc Middle() { End() }\n")
        self.write(
            "app/app.go",
            'package app\nimport "example.com/demo/lib"\nfunc Start() { lib.Middle() }\n',
        )
        second = rebuild_to_file(self.root, self.output, self.frontend)
        self.assertEqual(
            self.edges(second, root),
            {
                (root, "app/app.go::Start"),
                ("app/app.go::Start", "lib/end.go::Middle"),
                ("lib/end.go::Middle", "lib/end.go::End"),
            },
        )
        self.assertNotEqual(first["source_fingerprint"], second["source_fingerprint"])

        self.write("lib/end.go", "package lib\nfunc End() {}\n")
        third = rebuild_to_file(self.root, self.output, self.frontend)
        self.assertNotIn("lib/end.go::Middle", third["tests"][root]["nodes"])
        self.assertTrue(any(call["expression"] == "lib.Middle" for call in third["unresolved_calls"]))

    def test_external_test_package_and_receiver_methods(self):
        self.write(
            "app/app.go",
            "package app\n"
            "type Worker struct{}\n"
            "func (w Worker) First() { w.Second() }\n"
            "func (w Worker) Second() {}\n"
            "func Start() { Worker.First(Worker{}) }\n",
        )
        self.write(
            "app/app_test.go",
            'package app_test\nimport "testing"\nimport "example.com/demo/app"\n'
            "func TestFlow(t *testing.T) { app.Start() }\n"
            "func TestMain(m *testing.M) {}\n",
        )
        graph = build_graph(self.root, self.frontend)
        root = "app/app_test.go::TestFlow"
        self.assertEqual(
            self.edges(graph, root),
            {
                (root, "app/app.go::Start"),
                ("app/app.go::Start", "app/app.go::Worker.First"),
                ("app/app.go::Worker.First", "app/app.go::Worker.Second"),
            },
        )
        self.assertEqual(list(graph["tests"]), [root])

    def test_go_parse_error_keeps_previous_output(self):
        self.write("app_test.go", 'package app\nimport "testing"\nfunc TestOK(t *testing.T) {}\n')
        rebuild_to_file(self.root, self.output, self.frontend)
        previous = self.output.read_bytes()
        self.write("bad.go", "package app\nfunc Broken( {\n")
        with self.assertRaisesRegex(ValueError, "Go parser failed"):
            rebuild_to_file(self.root, self.output, self.frontend)
        self.assertEqual(self.output.read_bytes(), previous)

    def test_multiple_init_functions_in_one_generated_file_have_distinct_nodes(self):
        self.write("generated.go", "package app\nfunc init() {}\nfunc init() {}\n")
        self.write("app_test.go", 'package app\nimport "testing"\nfunc TestOK(t *testing.T) {}\n')
        graph = build_graph(self.root, self.frontend)
        ids = {node["id"] for node in graph["nodes"]}
        self.assertIn("generated.go::init#1", ids)
        self.assertIn("generated.go::init#2", ids)

    def test_default_build_tags_select_only_one_definition(self):
        self.write("default.go", "//go:build !alt\n\npackage app\nfunc Selected() {}\n")
        self.write("alt.go", "//go:build alt\n\npackage app\nfunc Selected() {}\n")
        self.write(
            "app_test.go",
            'package app\nimport "testing"\nfunc TestSelected(t *testing.T) { Selected() }\n',
        )
        graph = build_graph(self.root, self.frontend)
        self.assertEqual(
            self.edges(graph, "app_test.go::TestSelected"),
            {("app_test.go::TestSelected", "default.go::Selected")},
        )
        self.assertNotIn("alt.go::Selected", {node["id"] for node in graph["nodes"]})

    def test_build_tags_change_selected_target_and_analysis_fingerprint(self):
        self.write("default.go", "//go:build !alt\n\npackage app\nfunc Selected() {}\n")
        self.write("alt.go", "//go:build alt\n\npackage app\nfunc Selected() {}\n")
        self.write(
            "app_test.go",
            'package app\nimport "testing"\nfunc TestSelected(t *testing.T) { Selected() }\n',
        )
        default = build_graph(self.root, self.frontend)
        alt = build_graph(self.root, GoFrontend(self.helper, tags=["alt"]))
        root = "app_test.go::TestSelected"
        self.assertEqual(self.edges(alt, root), {(root, "alt.go::Selected")})
        self.assertEqual(default["source_fingerprint"], alt["source_fingerprint"])
        self.assertNotEqual(default["analysis_fingerprint"], alt["analysis_fingerprint"])
        self.assertEqual(alt["analysis_context"]["build_tags"], ["alt"])

    def test_go_flags_tags_are_rejected_instead_of_silently_ignored(self):
        self.write("app.go", "package app\nfunc Selected() {}\n")
        self.write(
            "app_test.go",
            'package app\nimport "testing"\nfunc TestSelected(t *testing.T) { Selected() }\n',
        )
        with patch.dict(os.environ, {"GOFLAGS": "-tags=alt"}):
            with self.assertRaisesRegex(ValueError, "GOFLAGS"):
                build_graph(self.root, self.frontend)

    def test_function_parameter_shadowing_does_not_invent_a_call_edge(self):
        self.write(
            "app.go",
            "package app\n"
            "func Target() {}\n"
            "func Invoke(Target func()) { Target() }\n",
        )
        self.write(
            "app_test.go",
            'package app\nimport "testing"\n'
            "func TestFlow(t *testing.T) { Invoke(func() {}) }\n",
        )
        graph = build_graph(self.root, self.frontend)
        root = "app_test.go::TestFlow"
        self.assertEqual(self.edges(graph, root), {(root, "app.go::Invoke")})
        self.assertIn(
            {"caller": "app.go::Invoke", "expression": "Target", "path": "app.go", "line": 3},
            graph["tests"][root]["unresolved_calls"],
        )

    def test_local_shadowing_does_not_invent_a_call_edge(self):
        self.write(
            "app.go",
            "package app\n"
            "func Target() {}\n"
            "func Invoke() { Target := func() {}; Target() }\n",
        )
        self.write(
            "app_test.go",
            'package app\nimport "testing"\n'
            "func TestFlow(t *testing.T) { Invoke() }\n",
        )
        graph = build_graph(self.root, self.frontend)
        root = "app_test.go::TestFlow"
        self.assertEqual(self.edges(graph, root), {(root, "app.go::Invoke")})
        self.assertTrue(any(
            call["caller"] == "app.go::Invoke" and call["expression"] == "Target"
            for call in graph["tests"][root]["unresolved_calls"]
        ))

    def test_shared_dependency_file_reaches_all_ancestor_tests(self):
        self.write("lib/leaf.go", "package lib\nfunc Leaf() {}\n")
        self.write(
            "app/entry.go",
            'package app\nimport leaf "example.com/demo/lib"\n'
            "func Start() { leaf.Leaf() }\n",
        )
        self.write(
            "app/bridge.go",
            "package app\nfunc Bridge() { Start() }\n",
        )
        self.write(
            "app/entry_test.go",
            'package app\nimport "testing"\n'
            "func TestDirect(t *testing.T) { Start() }\n"
            "func TestIndirect(t *testing.T) { Bridge() }\n"
            "func TestUnrelated(t *testing.T) {}\n",
        )
        graph = build_graph(self.root, self.frontend)
        direct = "app/entry_test.go::TestDirect"
        indirect = "app/entry_test.go::TestIndirect"
        leaf = "lib/leaf.go::Leaf"
        self.assertEqual(self.edges(graph, direct), {
            (direct, "app/entry.go::Start"),
            ("app/entry.go::Start", leaf),
        })
        self.assertEqual(self.edges(graph, indirect), {
            (indirect, "app/bridge.go::Bridge"),
            ("app/bridge.go::Bridge", "app/entry.go::Start"),
            ("app/entry.go::Start", leaf),
        })
        self.assertEqual(
            {root for root, view in graph["tests"].items() if leaf in view["nodes"]},
            {direct, indirect},
        )

    def test_repeat_full_rebuild_writes_identical_bytes(self):
        self.write("app.go", "package app\nfunc End() {}\nfunc Start() { End() }\n")
        self.write(
            "app_test.go",
            'package app\nimport "testing"\n'
            "func TestB(t *testing.T) { Start() }\n"
            "func TestA(t *testing.T) { Start() }\n",
        )
        first = rebuild_to_file(self.root, self.output, self.frontend)
        first_bytes = self.output.read_bytes()
        second = rebuild_to_file(self.root, self.output, self.frontend)
        self.assertEqual(first_bytes, self.output.read_bytes())
        self.assertEqual(first["source_fingerprint"], second["source_fingerprint"])

    def test_gzip_output_round_trips_and_rebuilds_identically(self):
        self.write("app.go", "package app\nfunc Start() {}\n")
        self.write(
            "app_test.go",
            'package app\nimport "testing"\nfunc TestFlow(t *testing.T) { Start() }\n',
        )
        compressed = self.root / "graph.json.gz"
        first = rebuild_to_file(self.root, compressed, self.frontend)
        with gzip.open(compressed, "rt", encoding="utf-8") as snapshot:
            self.assertEqual(json.load(snapshot), first)
        first_bytes = compressed.read_bytes()
        rebuild_to_file(self.root, compressed, self.frontend)
        self.assertEqual(first_bytes, compressed.read_bytes())
        self.write("bad.go", "package app\nfunc Broken( {\n")
        with self.assertRaisesRegex(ValueError, "Go parser failed"):
            rebuild_to_file(self.root, compressed, self.frontend)
        self.assertEqual(first_bytes, compressed.read_bytes())

    def test_parameter_receiver_resolves_to_declared_method(self):
        self.write(
            "app.go",
            "package app\n"
            "type Worker struct{}\n"
            "func (Worker) Step() {}\n"
            "func Run(w Worker) { w.Step() }\n",
        )
        self.write(
            "app_test.go",
            'package app\nimport "testing"\n'
            "func TestFlow(t *testing.T) { Run(Worker{}) }\n",
        )
        graph = build_graph(self.root, self.frontend)
        root = "app_test.go::TestFlow"
        self.assertIn(("app.go::Run", "app.go::Worker.Step"), self.edges(graph, root))

    def test_local_and_chained_receiver_calls_resolve_to_method(self):
        self.write(
            "app.go",
            "package app\n"
            "type Worker struct{}\n"
            "func NewWorker() *Worker { return &Worker{} }\n"
            "func (w *Worker) Step() {}\n",
        )
        self.write(
            "app_test.go",
            'package app\nimport "testing"\n'
            "func TestFlow(t *testing.T) { w := NewWorker(); w.Step(); NewWorker().Step() }\n",
        )
        graph = build_graph(self.root, self.frontend)
        root = "app_test.go::TestFlow"
        self.assertEqual(self.edges(graph, root), {
            (root, "app.go::NewWorker"),
            (root, "app.go::Worker.Step"),
        })
        self.assertEqual(next(
            len(edge["callsites"]) for edge in graph["edges"]
            if edge["caller"] == root and edge["callee"] == "app.go::Worker.Step"
        ), 2)

    def test_imported_constructor_return_type_resolves_local_method_call(self):
        self.write(
            "lib/worker.go",
            "package lib\n"
            "type Worker struct{}\n"
            "func NewWorker() *Worker { return &Worker{} }\n"
            "func (w *Worker) Step() {}\n",
        )
        self.write(
            "app/app_test.go",
            'package app_test\nimport "testing"\n'
            'import "example.com/demo/lib"\n'
            "func TestFlow(t *testing.T) { w := lib.NewWorker(); w.Step() }\n",
        )
        graph = build_graph(self.root, self.frontend)
        root = "app/app_test.go::TestFlow"
        self.assertEqual(self.edges(graph, root), {
            (root, "lib/worker.go::NewWorker"),
            (root, "lib/worker.go::Worker.Step"),
        })

    def test_deleting_dependency_file_removes_stale_nodes(self):
        self.write("lib/leaf.go", "package lib\nfunc Leaf() {}\n")
        self.write(
            "app/app.go",
            'package app\nimport "example.com/demo/lib"\n'
            "func Start() { lib.Leaf() }\n",
        )
        self.write(
            "app/app_test.go",
            'package app\nimport "testing"\n'
            "func TestFlow(t *testing.T) { Start() }\n",
        )
        root = "app/app_test.go::TestFlow"
        first = rebuild_to_file(self.root, self.output, self.frontend)
        self.assertIn("lib/leaf.go::Leaf", first["tests"][root]["nodes"])

        (self.root / "lib/leaf.go").unlink()
        second = rebuild_to_file(self.root, self.output, self.frontend)
        self.assertNotIn("lib/leaf.go::Leaf", {node["id"] for node in second["nodes"]})
        self.assertNotIn("lib/leaf.go::Leaf", second["tests"][root]["nodes"])
        self.assertTrue(any(
            call["caller"] == "app/app.go::Start" and call["expression"] == "lib.Leaf"
            for call in second["tests"][root]["unresolved_calls"]
        ))
        self.assertNotEqual(first["source_fingerprint"], second["source_fingerprint"])

    def test_constant_only_dependency_file_has_no_function_node(self):
        self.write("lib/config.go", "package lib\nconst Value = 1\n")
        self.write("lib/leaf.go", "package lib\nfunc Leaf() int { return Value }\n")
        self.write(
            "app/app_test.go",
            'package app_test\nimport "testing"\nimport "example.com/demo/lib"\n'
            "func TestFlow(t *testing.T) { _ = lib.Leaf() }\n",
        )
        graph = build_graph(self.root, self.frontend)
        root = "app/app_test.go::TestFlow"
        self.assertIn("lib/leaf.go::Leaf", graph["tests"][root]["nodes"])
        self.assertFalse(any(node["path"] == "lib/config.go" for node in graph["nodes"]))
        self.write("lib/config.go", "package lib\nconst Value = 2\n")
        changed = build_graph(self.root, self.frontend)
        self.assertNotEqual(graph["source_fingerprint"], changed["source_fingerprint"])
        self.assertEqual(graph["tests"], changed["tests"])

    def test_constant_only_file_has_package_dependent_candidate(self):
        self.write("lib/config.go", "package lib\nconst Value = 1\n")
        self.write("lib/leaf.go", "package lib\nfunc Leaf() int { return Value }\n")
        self.write(
            "app/app.go",
            'package app\nimport "example.com/demo/lib"\nfunc Start() int { return lib.Leaf() }\n',
        )
        self.write(
            "app/app_test.go",
            'package app_test\nimport "testing"\nimport "example.com/demo/app"\n'
            "func TestFlow(t *testing.T) { _ = app.Start() }\n",
        )
        self.write(
            "other/other_test.go",
            'package other\nimport "testing"\nfunc TestOther(t *testing.T) {}\n',
        )
        graph = build_graph(self.root, self.frontend)
        plan = plan_file_changes(graph, ["lib/config.go"])
        self.assertEqual(plan.candidate_tests, ("app/app_test.go::TestFlow",))
        self.assertTrue(plan.run_full_suite)

    def test_deleted_file_uses_previous_snapshot_for_candidates(self):
        self.write("lib/leaf.go", "package lib\nfunc Leaf() {}\n")
        self.write(
            "app/app_test.go",
            'package app_test\nimport "testing"\nimport "example.com/demo/lib"\n'
            "func TestFlow(t *testing.T) { lib.Leaf() }\n",
        )
        before = build_graph(self.root, self.frontend)
        (self.root / "lib/leaf.go").unlink()
        after = build_graph(self.root, self.frontend)
        plan = plan_file_changes(after, ["lib/leaf.go"], previous_graph=before)
        self.assertEqual(plan.candidate_tests, ("app/app_test.go::TestFlow",))
        self.assertTrue(plan.run_full_suite)

    @staticmethod
    def edges(graph, root):
        return {(edge["caller"], edge["callee"]) for edge in graph["tests"][root]["edges"]}
