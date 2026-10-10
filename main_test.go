package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/aayushhzz/hexagon-ci/hexagon"
)

func TestBuildGraphMatchesArchivedPythonGraphV2(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "lib/leaf.go", "package lib\nfunc Leaf() {}\n")
	writeFixture(t, root, "app/start.go", "package app\nimport leaf \"example.com/demo/lib\"\nfunc Start() { leaf.Leaf() }\n")
	writeFixture(t, root, "app/start_test.go", "package app\nimport \"testing\"\nfunc TestFlow(t *testing.T) { Start() }\nfunc TestUnrelated(t *testing.T) {}\n")

	got, err := hexagon.BuildGraph(root, nil)
	if err != nil {
		t.Fatalf("BuildGraph() error = %v", err)
	}
	want := legacyPythonGraph(t, root, nil)
	// The archived v2 reference predates source end lines. Compare its graph
	// facts while the dedicated span round-trip test checks the added metadata.
	for i := range got.Nodes {
		got.Nodes[i].EndLine = 0
	}
	got.AnalysisFingerprint = ""
	want.AnalysisFingerprint = ""
	got.AnalysisContext.AnalyzerSHA256 = ""
	want.AnalysisContext.AnalyzerSHA256 = ""
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("Go graph differs from archived Python graph v2\nGo:\n%s\nPython:\n%s", gotJSON, wantJSON)
	}
	if got.FormatVersion != 2 || got.Language != "go" || got.FileCount != 3 {
		t.Fatalf("graph header = (%d, %q, %d), want (2, go, 3)", got.FormatVersion, got.Language, got.FileCount)
	}
	if _, ok := got.Tests["app/start_test.go::TestFlow"]; !ok {
		t.Fatal("graph omitted the Go test root")
	}
}

func TestBuildTagsSelectMatchingDefinitionAndRemainInGraphContext(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "app/default.go", "//go:build !alt\n\npackage app\nfunc Selected() string { return \"default\" }\n")
	writeFixture(t, root, "app/alternate.go", "//go:build alt\n\npackage app\nfunc Selected() string { return \"alternate\" }\n")
	writeFixture(t, root, "app/app_test.go", "package app\nimport \"testing\"\nfunc TestSelected(t *testing.T) { _ = Selected() }\n")
	defaultGraph, err := hexagon.BuildGraph(root, nil)
	if err != nil {
		t.Fatalf("BuildGraph(default) error = %v", err)
	}
	altGraph, err := hexagon.BuildGraph(root, []string{"alt"})
	if err != nil {
		t.Fatalf("BuildGraph(alt) error = %v", err)
	}
	if defaultGraph.SourceFingerprint != altGraph.SourceFingerprint || defaultGraph.AnalysisFingerprint == altGraph.AnalysisFingerprint {
		t.Fatal("build tags should change analysis context without changing source fingerprint")
	}
	if !reflect.DeepEqual(altGraph.AnalysisContext.BuildTags, []string{"alt"}) {
		t.Fatalf("build tags in graph context = %v", altGraph.AnalysisContext.BuildTags)
	}
	rootID := "app/app_test.go::TestSelected"
	if got := altGraph.Tests[rootID].Edges; !reflect.DeepEqual(got, []hexagon.TestGraphEdge{{Caller: rootID, Callee: "app/alternate.go::Selected"}}) {
		t.Fatalf("alternate-tag edges = %v", got)
	}
	want := legacyPythonGraph(t, root, []string{"alt"})
	for i := range altGraph.Nodes {
		altGraph.Nodes[i].EndLine = 0
	}
	altGraph.AnalysisFingerprint = ""
	want.AnalysisFingerprint = ""
	altGraph.AnalysisContext.AnalyzerSHA256 = ""
	want.AnalysisContext.AnalyzerSHA256 = ""
	if !reflect.DeepEqual(altGraph, want) {
		t.Fatal("tagged Go graph differs from the archived Python implementation")
	}
}

func TestPlanFileChangesIncludesPreviousImportersAndKeepsFullSuiteFallback(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "lib/leaf.go", "package lib\nfunc Leaf() {}\n")
	writeFixture(t, root, "app/start.go", "package app\nimport \"example.com/demo/lib\"\nfunc Start() { lib.Leaf() }\n")
	writeFixture(t, root, "app/start_test.go", "package app_test\nimport (\"testing\"; \"example.com/demo/app\")\nfunc TestFlow(t *testing.T) { app.Start() }\n")
	writeFixture(t, root, "other/other_test.go", "package other\nimport \"testing\"\nfunc TestOther(t *testing.T) {}\n")
	previous, err := hexagon.BuildGraph(root, nil)
	if err != nil {
		t.Fatalf("BuildGraph(before) error = %v", err)
	}
	if err := os.Remove(filepath.Join(root, "lib/leaf.go")); err != nil {
		t.Fatal(err)
	}
	current, err := hexagon.BuildGraph(root, nil)
	if err != nil {
		t.Fatalf("BuildGraph(after) error = %v", err)
	}

	plan, err := hexagon.PlanFileChanges(current, []string{"lib/leaf.go"}, previous)
	if err != nil {
		t.Fatalf("PlanFileChanges() error = %v", err)
	}
	if !reflect.DeepEqual(plan.CandidateTests, []string{"app/start_test.go::TestFlow"}) {
		t.Fatalf("candidate tests = %v", plan.CandidateTests)
	}
	if !plan.RunFullSuite || plan.Reason == "" {
		t.Fatalf("plan must preserve conservative full-suite fallback: %+v", plan)
	}
	if plan.SelectionComplete {
		t.Fatal("AST graph planner must not claim complete test selection")
	}
	legacyPlan := legacyPythonPlan(t, current, previous, []string{"lib/leaf.go"})
	if !reflect.DeepEqual(plan.CandidateTests, legacyPlan.CandidateTests) || plan.RunFullSuite != legacyPlan.RunFullSuite || plan.Reason != legacyPlan.Reason {
		t.Fatalf("Go plan differs from archived Python selector: Go=%+v Python=%+v", plan, legacyPlan)
	}
}

func TestRebuildToFileIsDeterministicAndPreservesLastGraphOnParseError(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "app/app.go", "package app\nfunc Start() {}\n")
	writeFixture(t, root, "app/app_test.go", "package app\nimport \"testing\"\nfunc TestStart(t *testing.T) { Start() }\n")
	output := filepath.Join(root, "artifacts", "graph.json.gz")
	if _, err := hexagon.RebuildToFile(root, output, nil); err != nil {
		t.Fatalf("RebuildToFile(first) error = %v", err)
	}
	first, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hexagon.RebuildToFile(root, output, nil); err != nil {
		t.Fatalf("RebuildToFile(second) error = %v", err)
	}
	second, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical graph rebuilds produced different gzip bytes")
	}
	writeFixture(t, root, "app/broken.go", "package app\nfunc Broken( {\n")
	if _, err := hexagon.RebuildToFile(root, output, nil); err == nil {
		t.Fatal("RebuildToFile() succeeded with malformed Go source")
	}
	afterFailure, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, afterFailure) {
		t.Fatal("failed rebuild replaced the last complete graph")
	}
}

func TestCompactGraphRoundTripAndMixedFormatPlanning(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "lib/leaf.go", "package lib\nfunc Leaf() {}\n")
	writeFixture(t, root, "app/start.go", "package app\nimport \"example.com/demo/lib\"\nfunc Start() { lib.Leaf(); missing() }\n")
	writeFixture(t, root, "app/start_test.go", "package app\nimport \"testing\"\nfunc TestFlow(t *testing.T) { Start() }\n")
	writeFixture(t, root, "other/other_test.go", "package other\nimport \"testing\"\nfunc TestOther(t *testing.T) {}\n")
	namedPath := filepath.Join(t.TempDir(), "base.json.gz")
	named, err := hexagon.RebuildToFile(root, namedPath, []string{"alt"})
	if err != nil {
		t.Fatal(err)
	}
	compactPath := filepath.Join(t.TempDir(), "head.hgc.gz")
	var stdout, stderr bytes.Buffer
	if err := runCLI(context.Background(), []string{"graph", root, "--go-tags", "alt", "--output", compactPath}, &stdout, &stderr); err != nil {
		t.Fatalf("compact CLI build failed: %v, stderr: %s", err, stderr.String())
	}
	first, err := os.ReadFile(compactPath)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := hexagon.ReadCompactGraph(compactPath)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := compact.ToGraph()
	if err != nil || !reflect.DeepEqual(named, expanded) {
		t.Fatalf("compact graph differs from graph v2: %v", err)
	}
	index, err := hexagon.NewCompactIndex(compact)
	if err != nil {
		t.Fatal(err)
	}
	for root, want := range named.Tests {
		view, err := index.TestGraph(root)
		if err != nil || !reflect.DeepEqual(view, want) {
			t.Fatalf("named view %s differs: %v", root, err)
		}
	}
	if _, err := hexagon.RebuildCompactToFile(root, compactPath, []string{"alt"}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(compactPath)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("identical compact graph builds differed: %v", err)
	}
	baseline, err := hexagon.PlanFileChanges(named, []string{"lib/leaf.go"}, named)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{compactPath, namedPath}, {namedPath, compactPath}, {compactPath, compactPath}} {
		plan, err := hexagon.PlanGraphFiles(pair[0], []string{"lib/leaf.go"}, pair[1])
		if err != nil || !reflect.DeepEqual(plan, baseline) {
			t.Fatalf("plan for %v differs: plan=%+v, err=%v", pair, plan, err)
		}
	}
	if !baseline.RunFullSuite || !reflect.DeepEqual(baseline.CandidateTests, []string{"app/start_test.go::TestFlow"}) {
		t.Fatalf("compact selection did not preserve safe fallback and candidate: %+v", baseline)
	}
	stdout.Reset()
	if err := runCLI(context.Background(), []string{"plan", compactPath, "--changed-file", "lib/leaf.go", "--previous-graph", namedPath}, &stdout, &stderr); err != nil {
		t.Fatalf("compact CLI plan failed: %v", err)
	}
	var cliPlan hexagon.ImpactPlan
	if err := json.Unmarshal(stdout.Bytes(), &cliPlan); err != nil || !reflect.DeepEqual(cliPlan, *baseline) {
		t.Fatalf("compact CLI plan differs: %+v, err=%v", cliPlan, err)
	}
	if err := os.Remove(filepath.Join(root, "lib/leaf.go")); err != nil {
		t.Fatal(err)
	}
	deletedPath := filepath.Join(t.TempDir(), "deleted.hgc.gz")
	if _, err := hexagon.RebuildCompactToFile(root, deletedPath, []string{"alt"}); err != nil {
		t.Fatal(err)
	}
	currentNamed, err := hexagon.BuildGraph(root, []string{"alt"})
	if err != nil {
		t.Fatal(err)
	}
	wantDeleted, err := hexagon.PlanFileChanges(currentNamed, []string{"lib/leaf.go"}, named)
	if err != nil {
		t.Fatal(err)
	}
	gotDeleted, err := hexagon.PlanGraphFiles(deletedPath, []string{"lib/leaf.go"}, namedPath)
	if err != nil || !reflect.DeepEqual(gotDeleted, wantDeleted) {
		t.Fatalf("compact current/JSON previous lost deleted-file candidates: plan=%+v, want=%+v, err=%v", gotDeleted, wantDeleted, err)
	}
	writeFixture(t, root, "app/broken.go", "package app\nfunc Broken( {\n")
	if _, err := hexagon.RebuildCompactToFile(root, compactPath, nil); err == nil {
		t.Fatal("malformed source replaced the last complete compact graph")
	}
	afterFailure, err := os.ReadFile(compactPath)
	if err != nil || !bytes.Equal(first, afterFailure) {
		t.Fatalf("failed compact rebuild replaced the prior artifact: %v", err)
	}
}

func TestRunTestsUsesGoFullSuiteAndRejectsUnknownPlanVersions(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "app/app.go", "package app\nfunc Value() int { return 42 }\n")
	writeFixture(t, root, "app/app_test.go", "package app\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 42 { t.Fatal(Value()) } }\n")
	plan := &hexagon.ImpactPlan{FormatVersion: 1, Language: "go", ChangedFiles: []string{"app/app.go"}, RunFullSuite: true}
	var stdout, stderr bytes.Buffer
	if err := hexagon.RunTests(context.Background(), root, plan, &stdout, &stderr); err != nil {
		t.Fatalf("RunTests() error = %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "example.com/demo/app") {
		t.Fatalf("go test output does not show the executed package: %s", stdout.String())
	}
	plan.FormatVersion = 99
	if err := hexagon.RunTests(context.Background(), root, plan, &stdout, &stderr); err == nil {
		t.Fatal("RunTests() accepted an unsupported plan version")
	}
}

func TestRunTestsRequiresCompletenessBeforeSelectiveExecution(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "app/app.go", "package app\nfunc Value() int { return 42 }\n")
	writeFixture(t, root, "app/app_test.go", "package app\nimport \"testing\"\nfunc TestChosen(t *testing.T) {}\nfunc TestOther(t *testing.T) { t.Fatal(\"full-suite fallback ran\") }\n")
	plan := &hexagon.ImpactPlan{
		FormatVersion: 1, Language: "go", ChangedFiles: []string{"app/app.go"},
		CandidateTests: []string{"app/app_test.go::TestChosen"}, RunFullSuite: false,
	}
	var stdout, stderr bytes.Buffer
	err := hexagon.RunTests(context.Background(), root, plan, &stdout, &stderr)
	if err == nil || !strings.Contains(stdout.String()+stderr.String(), "full-suite fallback ran") {
		t.Fatalf("incomplete plan did not run the full suite: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}

	writeFixture(t, root, "app/app_test.go", "package app\nimport \"testing\"\nfunc TestChosen(t *testing.T) {}\nfunc TestOther(t *testing.T) { t.Fatal(\"selective test executed an unrelated test\") }\n")
	plan.SelectionComplete = true
	graph, err := hexagon.BuildGraph(root, nil)
	if err != nil {
		t.Fatalf("BuildGraph() error = %v", err)
	}
	plan.SourceFingerprint = graph.SourceFingerprint
	plan.AnalysisContext = graph.AnalysisContext
	plan.AnalysisContext.GOOS = "mismatched-platform"
	stdout.Reset()
	stderr.Reset()
	err = hexagon.RunTests(context.Background(), root, plan, &stdout, &stderr)
	if err == nil || !strings.Contains(stdout.String()+stderr.String(), "selective test executed an unrelated test") {
		t.Fatalf("mismatched build context did not fall back to the full suite: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	plan.AnalysisContext = graph.AnalysisContext
	stdout.Reset()
	stderr.Reset()
	if err := hexagon.RunTests(context.Background(), root, plan, &stdout, &stderr); err != nil {
		t.Fatalf("complete selective plan failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "selective test executed an unrelated test") {
		t.Fatal("selective runner executed an unrelated test")
	}
}

func TestBuildGraphRejectsImplicitGoBuildFlags(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "app/app.go", "package app\nfunc Value() int { return 42 }\n")
	t.Setenv("GOFLAGS", "-tags=alt")
	if _, err := hexagon.BuildGraph(root, nil); err == nil || !strings.Contains(err.Error(), "GOFLAGS") {
		t.Fatalf("BuildGraph() error = %v, want an explicit GOFLAGS rejection", err)
	}
}

func TestLegacyPositionalCLIFlagsStillBuildGraphV2(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writeFixture(t, root, "app/app.go", "package app\nfunc Value() int { return 42 }\n")
	writeFixture(t, root, "app/app_test.go", "package app\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 42 { t.Fatal(Value()) } }\n")
	output := filepath.Join(t.TempDir(), "graph.json")
	var stdout, stderr bytes.Buffer
	args := []string{root, "--language", "go", "--go-helper", "/unused/legacy-helper", "--output", output}
	if err := runCLI(context.Background(), args, &stdout, &stderr); err != nil {
		t.Fatalf("legacy graph invocation failed: %v\nstderr: %s", err, stderr.String())
	}
	graph, err := hexagon.ReadGraph(output)
	if err != nil {
		t.Fatalf("legacy invocation output is not graph v2: %v", err)
	}
	if graph.Language != "go" || graph.FormatVersion != 2 {
		t.Fatalf("legacy invocation graph header = (%q, %d)", graph.Language, graph.FormatVersion)
	}
}

func TestReadersAcceptAddedFieldsAndRejectUnsupportedVersions(t *testing.T) {
	directory := t.TempDir()
	graphPath := filepath.Join(directory, "graph.json")
	if err := os.WriteFile(graphPath, []byte(`{"format_version":2,"language":"go","future_extension":{"version":3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hexagon.ReadGraph(graphPath); err != nil {
		t.Fatalf("ReadGraph rejected an added v2 field: %v", err)
	}
	planPath := filepath.Join(directory, "plan.json")
	if err := os.WriteFile(planPath, []byte(`{"format_version":1,"language":"go","changed_files":["app/app.go"],"candidate_tests":[],"run_full_suite":true,"future_extension":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hexagon.ReadPlan(planPath); err != nil {
		t.Fatalf("ReadPlan rejected an added v1 field: %v", err)
	}
	if err := os.WriteFile(graphPath, []byte(`{"format_version":3,"language":"go"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hexagon.ReadGraph(graphPath); err == nil {
		t.Fatal("ReadGraph accepted unsupported graph format 3")
	}
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func legacyPythonGraph(t *testing.T, repository string, tags []string) *hexagon.Graph {
	t.Helper()
	workspace := repositoryRoot(t)
	output := filepath.Join(t.TempDir(), "python-graph.json")
	args := []string{"-m", "test_call_graph", repository, "--language", "go", "--output", output}
	if len(tags) != 0 {
		args = append(args, "--go-tags", strings.Join(tags, ","))
	}
	cmd := exec.Command("python3", args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(workspace, "python", "graph_v2_reference"), "PATH="+filepath.Join(runtime.GOROOT(), "bin")+":"+os.Getenv("PATH"))
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("archived Python graph command failed: %v\n%s", err, result)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var graph hexagon.Graph
	if err := json.Unmarshal(data, &graph); err != nil {
		t.Fatalf("decode archived Python graph: %v", err)
	}
	return &graph
}

func legacyPythonPlan(t *testing.T, current, previous *hexagon.Graph, changed []string) *hexagon.ImpactPlan {
	t.Helper()
	workspace := repositoryRoot(t)
	currentPath := filepath.Join(t.TempDir(), "current.json")
	previousPath := filepath.Join(t.TempDir(), "previous.json")
	currentData, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	previousData, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, currentData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previousPath, previousData, 0o644); err != nil {
		t.Fatal(err)
	}
	program := "import json,sys; from test_call_graph.impact import plan_file_changes; " +
		"current=json.load(open(sys.argv[1])); previous=json.load(open(sys.argv[2])); " +
		"plan=plan_file_changes(current, json.loads(sys.argv[3]), previous); " +
		"print(json.dumps({'candidate_tests':plan.candidate_tests,'run_full_suite':plan.run_full_suite,'reason':plan.reason}))"
	cmd := exec.Command("python3", "-c", program, currentPath, previousPath, mustJSON(t, changed))
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(workspace, "python", "graph_v2_reference"))
	result, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("archived Python selector failed: %v\n%s", err, result)
	}
	var want struct {
		CandidateTests []string `json:"candidate_tests"`
		RunFullSuite   bool     `json:"run_full_suite"`
		Reason         string   `json:"reason"`
	}
	if err := json.Unmarshal(result, &want); err != nil {
		t.Fatalf("decode archived Python plan: %v", err)
	}
	return &hexagon.ImpactPlan{CandidateTests: want.CandidateTests, RunFullSuite: want.RunFullSuite, Reason: want.Reason}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return filepath.Dir(filename)
}
