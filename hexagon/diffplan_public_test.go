package hexagon_test

import (
	"reflect"
	"testing"

	"github.com/aayushhzz/hexagon-ci/hexagon"
)

func TestStructuredGitDiffPlannerPublicAPI(t *testing.T) {
	const testRoot = "pkg/leaf_test.go::TestChanged"
	graph := &hexagon.Graph{
		FormatVersion: 2,
		Language:      "go",
		Nodes: []hexagon.GraphNode{
			{ID: "pkg/leaf.go::Changed", Path: "pkg/leaf.go", Qualname: "Changed", Line: 1, EndLine: 4},
			{ID: testRoot, Path: "pkg/leaf_test.go", Qualname: "TestChanged", Line: 1, EndLine: 4, IsTest: true},
		},
		Edges: []hexagon.GraphEdge{{Caller: testRoot, Callee: "pkg/leaf.go::Changed"}},
		Tests: map[string]hexagon.TestGraph{testRoot: {}},
	}
	diff := hexagon.GitDiff{Files: []hexagon.FileDiff{{
		OldPath:   "pkg/leaf.go",
		NewPath:   "pkg/leaf.go",
		OldRanges: []hexagon.LineRange{{StartLine: 2, EndLine: 2}},
		NewRanges: []hexagon.LineRange{{StartLine: 2, EndLine: 2}},
	}}}

	plan, err := hexagon.PlanGitDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanGitDiff() error = %v", err)
	}
	if !reflect.DeepEqual(plan.TestRoots, []string{testRoot}) {
		t.Fatalf("test roots = %v; want [%s]", plan.TestRoots, testRoot)
	}
	if !reflect.DeepEqual(plan.FunctionsToRun, []string{"pkg/leaf.go::Changed", testRoot}) {
		t.Fatalf("functions to run = %v", plan.FunctionsToRun)
	}
	if !plan.SelectionComplete || plan.RunFullSuite {
		t.Fatalf("complete fixture unexpectedly requested full suite: %+v", plan)
	}
}
