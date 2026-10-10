package hexagon

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanGitDiffReturnsUniqueFunctionsAndAllConnectedTestRoots(t *testing.T) {
	graph := functionDiffFixtureGraph()
	diff := GitDiff{Files: []FileDiff{
		{NewPath: "pkg/leaf.go", NewRanges: []LineRange{{StartLine: 12, EndLine: 12}}},
		{NewPath: "pkg/worker.go", NewRanges: []LineRange{{StartLine: 22, EndLine: 22}}},
	}}

	plan, err := PlanGitDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanGitDiff() error = %v", err)
	}
	wantChanged := []string{"pkg/leaf.go::Changed", "pkg/worker.go::AlsoChanged"}
	wantParents := []string{"pkg/middle.go::Middle"}
	wantTests := []string{"pkg/test_a_test.go::TestA", "pkg/test_b_test.go::TestB"}
	wantFunctions := []string{
		"pkg/leaf.go::Changed", "pkg/middle.go::Middle", "pkg/test_a_test.go::TestA",
		"pkg/test_b_test.go::TestB", "pkg/worker.go::AlsoChanged",
	}
	if !reflect.DeepEqual(plan.ChangedFunctions, wantChanged) {
		t.Fatalf("changed functions = %v; want %v", plan.ChangedFunctions, wantChanged)
	}
	if !reflect.DeepEqual(plan.ParentFunctions, wantParents) {
		t.Fatalf("parent functions = %v; want %v", plan.ParentFunctions, wantParents)
	}
	if !reflect.DeepEqual(plan.TestRoots, wantTests) {
		t.Fatalf("test roots = %v; want %v", plan.TestRoots, wantTests)
	}
	if !reflect.DeepEqual(plan.FunctionsToRun, wantFunctions) {
		t.Fatalf("functions to run = %v; want %v", plan.FunctionsToRun, wantFunctions)
	}
	if !plan.SelectionComplete || plan.RunFullSuite {
		t.Fatalf("complete direct-call fixture should allow scoped selection: %+v", plan)
	}
}

func TestPlanGitDiffCompactMatchesNamedPlanner(t *testing.T) {
	graph := functionDiffFixtureGraph()
	compact := compactFunctionDiffFixture(t, graph)
	diff := GitDiff{Files: []FileDiff{{
		OldPath:   "pkg/leaf.go",
		NewPath:   "pkg/leaf.go",
		OldRanges: []LineRange{{StartLine: 12, EndLine: 12}},
		NewRanges: []LineRange{{StartLine: 12, EndLine: 12}},
	}}}

	namedPlan, err := PlanGitDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanGitDiff() error = %v", err)
	}
	compactPlan, err := PlanGitDiffCompact(compact, diff, compact)
	if err != nil {
		t.Fatalf("PlanGitDiffCompact() error = %v", err)
	}
	if !reflect.DeepEqual(compactPlan, namedPlan) {
		t.Fatalf("compact plan = %+v; named plan = %+v", compactPlan, namedPlan)
	}
}

func TestPlanGitDiffFallsBackWhenFunctionSpansAreUnavailable(t *testing.T) {
	graph := functionDiffFixtureGraph()
	for i := range graph.Nodes {
		if graph.Nodes[i].ID == "pkg/leaf.go::Changed" {
			graph.Nodes[i].EndLine = 0
		}
	}
	diff := GitDiff{Files: []FileDiff{{
		NewPath: "pkg/leaf.go", NewRanges: []LineRange{{StartLine: 12, EndLine: 12}},
	}}}

	plan, err := PlanGitDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanGitDiff() error = %v", err)
	}
	if plan.SelectionComplete || !plan.RunFullSuite || plan.Reason == "" {
		t.Fatalf("missing function span must request full suite: %+v", plan)
	}
}

func TestPlanGitDiffBroadensRootsWhenAnyRangeIsUnmappable(t *testing.T) {
	graph := functionDiffFixtureGraph()
	diff := GitDiff{Files: []FileDiff{
		{NewPath: "pkg/leaf.go", NewRanges: []LineRange{{StartLine: 12, EndLine: 12}}},
		{NewPath: "pkg/not_in_graph.go", NewRanges: []LineRange{{StartLine: 1, EndLine: 1}}},
	}}

	plan, err := PlanGitDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanGitDiff() error = %v", err)
	}
	want := []string{
		"pkg/test_a_test.go::TestA", "pkg/test_b_test.go::TestB", "pkg/unrelated_test.go::TestUnrelated",
	}
	if !reflect.DeepEqual(plan.TestRoots, want) {
		t.Fatalf("incomplete mapping test roots = %v; want every current root %v", plan.TestRoots, want)
	}
	if plan.SelectionComplete || !plan.RunFullSuite {
		t.Fatalf("incomplete mapping must request full suite: %+v", plan)
	}
}

func TestPlanGitDiffMapsOldAndNewFunctionSpansAcrossRename(t *testing.T) {
	previous := functionDiffFixtureGraph()
	for i := range previous.Nodes {
		if previous.Nodes[i].ID == "pkg/leaf.go::Changed" {
			previous.Nodes[i].ID = "pkg/old_leaf.go::Changed"
			previous.Nodes[i].Path = "pkg/old_leaf.go"
		}
	}
	for i := range previous.Edges {
		if previous.Edges[i].Callee == "pkg/leaf.go::Changed" {
			previous.Edges[i].Callee = "pkg/old_leaf.go::Changed"
		}
	}

	current := functionDiffFixtureGraph()
	for i := range current.Nodes {
		if current.Nodes[i].ID == "pkg/leaf.go::Changed" {
			current.Nodes[i].ID = "pkg/new_leaf.go::Changed"
			current.Nodes[i].Path = "pkg/new_leaf.go"
		}
	}
	for i := range current.Edges {
		if current.Edges[i].Callee == "pkg/leaf.go::Changed" {
			current.Edges[i].Callee = "pkg/new_leaf.go::Changed"
		}
	}

	diff := GitDiff{Files: []FileDiff{{
		OldPath: "pkg/old_leaf.go", OldRanges: []LineRange{{StartLine: 12, EndLine: 12}},
		NewPath: "pkg/new_leaf.go", NewRanges: []LineRange{{StartLine: 12, EndLine: 12}},
	}}}
	plan, err := PlanGitDiff(current, diff, previous)
	if err != nil {
		t.Fatalf("PlanGitDiff() error = %v", err)
	}
	want := []string{"pkg/new_leaf.go::Changed", "pkg/old_leaf.go::Changed"}
	if !reflect.DeepEqual(plan.ChangedFunctions, want) {
		t.Fatalf("changed functions = %v; want %v", plan.ChangedFunctions, want)
	}
	if !reflect.DeepEqual(plan.TestRoots, []string{"pkg/test_a_test.go::TestA", "pkg/test_b_test.go::TestB"}) {
		t.Fatalf("test roots = %v", plan.TestRoots)
	}
	if !plan.SelectionComplete || plan.RunFullSuite {
		t.Fatalf("complete rename mapping should allow scoped selection: %+v", plan)
	}
}

func TestPlanGitDiffFallsBackWhenOldLinesHaveNoBaseGraph(t *testing.T) {
	graph := functionDiffFixtureGraph()
	diff := GitDiff{Files: []FileDiff{{
		OldPath: "pkg/leaf.go", OldRanges: []LineRange{{StartLine: 12, EndLine: 12}},
	}}}

	plan, err := PlanGitDiff(graph, diff, nil)
	if err != nil {
		t.Fatalf("PlanGitDiff() error = %v", err)
	}
	if plan.SelectionComplete || !plan.RunFullSuite || !strings.Contains(plan.Reason, "base graph") {
		t.Fatalf("missing base graph must request full suite: %+v", plan)
	}
}

func TestPlanGitDiffFallsBackWhenGraphHasUnresolvedCalls(t *testing.T) {
	graph := functionDiffFixtureGraph()
	graph.UnresolvedCalls = []UnresolvedCall{{Caller: "pkg/middle.go::Middle", Expression: "dynamicCall()"}}
	diff := GitDiff{Files: []FileDiff{{
		NewPath: "pkg/leaf.go", NewRanges: []LineRange{{StartLine: 12, EndLine: 12}},
	}}}

	plan, err := PlanGitDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanGitDiff() error = %v", err)
	}
	if plan.SelectionComplete || !plan.RunFullSuite || !strings.Contains(plan.Reason, fullSuiteReason) {
		t.Fatalf("unresolved graph dependencies must request full suite: %+v", plan)
	}
	if !reflect.DeepEqual(plan.TestRoots, []string{
		"pkg/test_a_test.go::TestA", "pkg/test_b_test.go::TestB", "pkg/unrelated_test.go::TestUnrelated",
	}) {
		t.Fatalf("incomplete dependency graph must include every current test root: %v", plan.TestRoots)
	}
}

func functionDiffFixtureGraph() *Graph {
	return &Graph{
		FormatVersion: graphFormatVersion,
		Language:      "go",
		Nodes: []GraphNode{
			{ID: "pkg/leaf.go::Changed", Path: "pkg/leaf.go", Qualname: "Changed", Line: 10, EndLine: 14},
			{ID: "pkg/middle.go::Middle", Path: "pkg/middle.go", Qualname: "Middle", Line: 3, EndLine: 8},
			{ID: "pkg/test_a_test.go::TestA", Path: "pkg/test_a_test.go", Qualname: "TestA", Line: 5, EndLine: 9, IsTest: true},
			{ID: "pkg/test_b_test.go::TestB", Path: "pkg/test_b_test.go", Qualname: "TestB", Line: 4, EndLine: 8, IsTest: true},
			{ID: "pkg/unrelated_test.go::TestUnrelated", Path: "pkg/unrelated_test.go", Qualname: "TestUnrelated", Line: 4, EndLine: 7, IsTest: true},
			{ID: "pkg/worker.go::AlsoChanged", Path: "pkg/worker.go", Qualname: "AlsoChanged", Line: 20, EndLine: 25},
		},
		Edges: []GraphEdge{
			{Caller: "pkg/middle.go::Middle", Callee: "pkg/leaf.go::Changed"},
			{Caller: "pkg/middle.go::Middle", Callee: "pkg/worker.go::AlsoChanged"},
			{Caller: "pkg/test_a_test.go::TestA", Callee: "pkg/middle.go::Middle"},
			{Caller: "pkg/test_b_test.go::TestB", Callee: "pkg/middle.go::Middle"},
		},
		Tests: map[string]TestGraph{
			"pkg/test_a_test.go::TestA":            {},
			"pkg/test_b_test.go::TestB":            {},
			"pkg/unrelated_test.go::TestUnrelated": {},
		},
	}
}

func compactFunctionDiffFixture(t *testing.T, graph *Graph) *CompactGraph {
	t.Helper()
	facts := sourceFacts{Nodes: graph.Nodes, Files: graph.Files}
	for _, edge := range graph.Edges {
		facts.Edges = append(facts.Edges, sourceEdge{Caller: edge.Caller, Callee: edge.Callee})
	}
	compact, err := compactFromFacts(facts, "fixture", len(facts.Files))
	if err != nil {
		t.Fatalf("compactFromFacts() error = %v", err)
	}
	return compact
}
