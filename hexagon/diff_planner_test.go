package hexagon

import (
	"compress/gzip"
	"encoding/gob"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPlanDiffMapsChangedFunctionsAndDeduplicatesTestRoots(t *testing.T) {
	const (
		changed   = "pkg/impl.go::Changed"
		wrapA     = "pkg/impl.go::WrapA"
		wrapB     = "pkg/impl.go::WrapB"
		testOne   = "pkg/impl_test.go::TestOne"
		testTwo   = "pkg/impl_test.go::TestTwo"
		unrelated = "pkg/impl_test.go::TestUnrelated"
	)
	facts := sourceFacts{
		Nodes: []GraphNode{
			{ID: changed, Path: "pkg/impl.go", Qualname: "Changed", Line: 10, EndLine: 13},
			{ID: wrapA, Path: "pkg/impl.go", Qualname: "WrapA", Line: 20, EndLine: 22},
			{ID: wrapB, Path: "pkg/impl.go", Qualname: "WrapB", Line: 30, EndLine: 32},
			{ID: testOne, Path: "pkg/impl_test.go", Qualname: "TestOne", Line: 2, EndLine: 6, IsTest: true},
			{ID: testTwo, Path: "pkg/impl_test.go", Qualname: "TestTwo", Line: 8, EndLine: 12, IsTest: true},
			{ID: unrelated, Path: "pkg/impl_test.go", Qualname: "TestUnrelated", Line: 14, EndLine: 16, IsTest: true},
		},
		Edges: []sourceEdge{
			{Caller: testOne, Callee: wrapA},
			{Caller: testOne, Callee: wrapB},
			{Caller: wrapA, Callee: changed},
			{Caller: wrapB, Callee: changed},
			{Caller: testTwo, Callee: changed},
		},
		Files: []FileMetadata{
			{Path: "pkg/impl.go", Package: "impl"},
			{Path: "pkg/impl_test.go", Package: "impl"},
		},
	}
	graph := graphFromPlannerFacts(t, facts)
	diff := ParsedDiff{Files: []DiffFile{{
		OldPath: "pkg/impl.go", NewPath: "pkg/impl.go",
		Hunks: []DiffHunk{{OldStart: 11, OldLines: 1, NewStart: 11, NewLines: 1}},
	}}}

	plan, err := PlanDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanDiff() error = %v", err)
	}
	if got, want := plan.ChangedFunctions, []string{changed}; !reflect.DeepEqual(got, want) {
		t.Errorf("changed functions = %v, want %v", got, want)
	}
	if got, want := plan.CandidateTests, []string{testOne, testTwo}; !reflect.DeepEqual(got, want) {
		t.Errorf("candidate tests = %v, want %v", got, want)
	}
	if got, want := plan.ImpactFunctions, []string{changed, wrapA, wrapB, testOne, testTwo}; !reflect.DeepEqual(got, want) {
		t.Errorf("impact functions = %v, want %v", got, want)
	}
	if got, want := len(plan.DiffMappings), 2; got != want {
		t.Fatalf("diff mappings = %d, want %d", got, want)
	}
	for _, mapping := range plan.DiffMappings {
		if got, want := mapping.Functions, []string{changed}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s mapping functions = %v, want %v", mapping.Side, got, want)
		}
	}
	assertSortedUniquePlannerValues(t, plan.CandidateTests)
	assertSortedUniquePlannerValues(t, plan.ImpactFunctions)
	if !plan.RunFullSuite || plan.SelectionComplete {
		t.Errorf("diff planning must retain the full-suite safety gate: %+v", plan)
	}
}

func TestPlanDiffUnionsBaseAndHeadForRenamedFunctions(t *testing.T) {
	const testRoot = "pkg/flow_test.go::TestFlow"
	base := graphFromPlannerFacts(t, sourceFacts{
		Nodes: []GraphNode{
			{ID: "pkg/old.go::Removed", Path: "pkg/old.go", Qualname: "Removed", Line: 5, EndLine: 8},
			{ID: testRoot, Path: "pkg/flow_test.go", Qualname: "TestFlow", Line: 2, EndLine: 7, IsTest: true},
		},
		Edges: []sourceEdge{{Caller: testRoot, Callee: "pkg/old.go::Removed"}},
		Files: []FileMetadata{{Path: "pkg/old.go", Package: "flow"}, {Path: "pkg/flow_test.go", Package: "flow"}},
	})
	head := graphFromPlannerFacts(t, sourceFacts{
		Nodes: []GraphNode{
			{ID: "pkg/new.go::Added", Path: "pkg/new.go", Qualname: "Added", Line: 5, EndLine: 8},
			{ID: testRoot, Path: "pkg/flow_test.go", Qualname: "TestFlow", Line: 2, EndLine: 7, IsTest: true},
		},
		Edges: []sourceEdge{{Caller: testRoot, Callee: "pkg/new.go::Added"}},
		Files: []FileMetadata{{Path: "pkg/new.go", Package: "flow"}, {Path: "pkg/flow_test.go", Package: "flow"}},
	})
	diff := ParsedDiff{Files: []DiffFile{{
		OldPath: "pkg/old.go", NewPath: "pkg/new.go",
		Hunks: []DiffHunk{{OldStart: 6, OldLines: 1, NewStart: 6, NewLines: 1}},
	}}}

	plan, err := PlanDiff(head, diff, base)
	if err != nil {
		t.Fatalf("PlanDiff() error = %v", err)
	}
	if got, want := plan.ChangedFunctions, []string{"pkg/new.go::Added", "pkg/old.go::Removed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("changed functions = %v, want %v", got, want)
	}
	if got, want := plan.CandidateTests, []string{testRoot}; !reflect.DeepEqual(got, want) {
		t.Errorf("candidate tests = %v, want %v", got, want)
	}
}

func TestPlanDiffMapsEveryOverlappingFunctionSpan(t *testing.T) {
	const testRoot = "pkg/impl_test.go::TestChanged"
	facts := sourceFacts{
		Nodes: []GraphNode{
			{ID: "pkg/impl.go::Outer", Path: "pkg/impl.go", Qualname: "Outer", Line: 4, EndLine: 20},
			{ID: "pkg/impl.go::Inner", Path: "pkg/impl.go", Qualname: "Inner", Line: 10, EndLine: 12},
			{ID: testRoot, Path: "pkg/impl_test.go", Qualname: "TestChanged", Line: 2, EndLine: 4, IsTest: true},
		},
		Edges: []sourceEdge{
			{Caller: testRoot, Callee: "pkg/impl.go::Outer"},
			{Caller: testRoot, Callee: "pkg/impl.go::Inner"},
		},
		Files: []FileMetadata{{Path: "pkg/impl.go", Package: "impl"}, {Path: "pkg/impl_test.go", Package: "impl"}},
	}
	graph := graphFromPlannerFacts(t, facts)
	diff := ParsedDiff{Files: []DiffFile{{
		OldPath: "pkg/impl.go", NewPath: "pkg/impl.go",
		Hunks: []DiffHunk{{OldStart: 11, OldLines: 1, NewStart: 11, NewLines: 1}},
	}}}

	plan, err := PlanDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanDiff() error = %v", err)
	}
	want := []string{"pkg/impl.go::Inner", "pkg/impl.go::Outer"}
	if !reflect.DeepEqual(plan.ChangedFunctions, want) {
		t.Fatalf("changed functions = %v, want all overlapping spans %v", plan.ChangedFunctions, want)
	}
	if got, want := plan.CandidateTests, []string{testRoot}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate tests = %v, want %v", got, want)
	}
}

func TestPlanDiffUsesAllCurrentTestsWhenRangeCannotBeMapped(t *testing.T) {
	const (
		function = "pkg/impl.go::Changed"
		testOne  = "pkg/impl_test.go::TestOne"
		testTwo  = "pkg/impl_test.go::TestTwo"
	)
	graph := graphFromPlannerFacts(t, sourceFacts{
		Nodes: []GraphNode{
			{ID: function, Path: "pkg/impl.go", Qualname: "Changed", Line: 10}, // Old snapshot without an end line.
			{ID: testOne, Path: "pkg/impl_test.go", Qualname: "TestOne", Line: 2, EndLine: 4, IsTest: true},
			{ID: testTwo, Path: "pkg/impl_test.go", Qualname: "TestTwo", Line: 6, EndLine: 8, IsTest: true},
		},
		Edges: []sourceEdge{{Caller: testOne, Callee: function}},
		Files: []FileMetadata{{Path: "pkg/impl.go", Package: "impl"}, {Path: "pkg/impl_test.go", Package: "impl"}},
	})
	diff := ParsedDiff{Files: []DiffFile{{
		OldPath: "pkg/impl.go", NewPath: "pkg/impl.go",
		Hunks: []DiffHunk{{OldStart: 10, OldLines: 1, NewStart: 10, NewLines: 1}},
	}}}

	plan, err := PlanDiff(graph, diff, graph)
	if err != nil {
		t.Fatalf("PlanDiff() error = %v", err)
	}
	if got, want := plan.CandidateTests, []string{testOne, testTwo}; !reflect.DeepEqual(got, want) {
		t.Errorf("fallback candidates = %v, want all current test roots %v", got, want)
	}
	if len(plan.DiffMappings) != 2 || len(plan.DiffMappings[0].Functions) != 0 || len(plan.DiffMappings[1].Functions) != 0 {
		t.Errorf("unmapped diff ranges were not reported: %+v", plan.DiffMappings)
	}
}

func TestPlanCompactDiffMatchesNamedDiffPlan(t *testing.T) {
	facts := sourceFacts{
		Nodes: []GraphNode{
			{ID: "pkg/impl.go::Changed", Path: "pkg/impl.go", Qualname: "Changed", Line: 4, EndLine: 6},
			{ID: "pkg/impl_test.go::TestChanged", Path: "pkg/impl_test.go", Qualname: "TestChanged", Line: 2, EndLine: 4, IsTest: true},
		},
		Edges: []sourceEdge{{Caller: "pkg/impl_test.go::TestChanged", Callee: "pkg/impl.go::Changed"}},
		Files: []FileMetadata{{Path: "pkg/impl.go", Package: "impl"}, {Path: "pkg/impl_test.go", Package: "impl"}},
	}
	named := graphFromPlannerFacts(t, facts)
	compact, err := compactFromFacts(facts, named.SourceFingerprint, named.FileCount)
	if err != nil {
		t.Fatalf("compactFromFacts() error = %v", err)
	}
	diff := ParsedDiff{Files: []DiffFile{{
		OldPath: "pkg/impl.go", NewPath: "pkg/impl.go",
		Hunks: []DiffHunk{{OldStart: 5, OldLines: 1, NewStart: 5, NewLines: 1}},
	}}}

	namedPlan, err := PlanDiff(named, diff, named)
	if err != nil {
		t.Fatalf("PlanDiff() error = %v", err)
	}
	compactPlan, err := PlanCompactDiff(compact, diff, compact)
	if err != nil {
		t.Fatalf("PlanCompactDiff() error = %v", err)
	}
	if !reflect.DeepEqual(compactPlan, namedPlan) {
		t.Fatalf("compact plan differs from named plan\ncompact: %+v\nnamed: %+v", compactPlan, namedPlan)
	}
}

func TestBuildGraphRecordsFunctionEndLine(t *testing.T) {
	root := t.TempDir()
	writePlannerFixture(t, root, "go.mod", "module example.com/demo\n\ngo 1.24.0\n")
	writePlannerFixture(t, root, "pkg/impl.go", "package impl\nfunc Changed() {\n\tvalue := 1\n\t_ = value\n}\n")

	graph, err := BuildGraph(root, nil)
	if err != nil {
		t.Fatalf("BuildGraph() error = %v", err)
	}
	found := false
	for _, node := range graph.Nodes {
		if node.ID == "pkg/impl.go::Changed" {
			found = true
			if node.Line != 2 || node.EndLine != 5 {
				t.Fatalf("function span = %d..%d, want 2..5", node.Line, node.EndLine)
			}
			break
		}
	}
	if !found {
		t.Fatal("BuildGraph() omitted Changed")
	}
	graphPath := filepath.Join(t.TempDir(), "graph.json")
	if _, err := RebuildToFile(root, graphPath, nil); err != nil {
		t.Fatalf("RebuildToFile() error = %v", err)
	}
	readGraph, err := ReadGraph(graphPath)
	if err != nil {
		t.Fatalf("ReadGraph() error = %v", err)
	}
	compact, err := BuildCompactGraph(root, nil)
	if err != nil {
		t.Fatalf("BuildCompactGraph() error = %v", err)
	}
	compactPath := filepath.Join(t.TempDir(), "graph.hgc.gz")
	if err := WriteCompactGraph(compactPath, compact); err != nil {
		t.Fatalf("WriteCompactGraph() error = %v", err)
	}
	readCompact, err := ReadCompactGraph(compactPath)
	if err != nil {
		t.Fatalf("ReadCompactGraph() error = %v", err)
	}
	expanded, err := readCompact.ToGraph()
	if err != nil {
		t.Fatalf("ToGraph() error = %v", err)
	}
	for _, snapshot := range []*Graph{readGraph, expanded} {
		found := false
		for _, node := range snapshot.Nodes {
			if node.ID == "pkg/impl.go::Changed" {
				found = true
				if node.Line != 2 || node.EndLine != 5 {
					t.Fatalf("round-tripped function span = %d..%d, want 2..5", node.Line, node.EndLine)
				}
				break
			}
		}
		if !found {
			t.Fatal("round-tripped graph omitted Changed")
		}
	}
	if !reflect.DeepEqual(readGraph.Nodes, expanded.Nodes) {
		t.Fatalf("JSON and compact node spans differ:\nJSON: %+v\ncompact: %+v", readGraph.Nodes, expanded.Nodes)
	}
}

func TestReadCompactV1AndFallbackWhenFunctionSpansAreAbsent(t *testing.T) {
	const testRoot = "pkg/impl_test.go::TestChanged"
	facts := sourceFacts{
		Nodes: []GraphNode{
			{ID: "pkg/impl.go::Changed", Path: "pkg/impl.go", Qualname: "Changed", Line: 4, EndLine: 6},
			{ID: testRoot, Path: "pkg/impl_test.go", Qualname: "TestChanged", Line: 2, EndLine: 4, IsTest: true},
		},
		Edges: []sourceEdge{{Caller: testRoot, Callee: "pkg/impl.go::Changed"}},
		Files: []FileMetadata{{Path: "pkg/impl.go", Package: "impl"}, {Path: "pkg/impl_test.go", Package: "impl"}},
	}
	compact, err := compactFromFacts(facts, "old-snapshot", len(facts.Files))
	if err != nil {
		t.Fatalf("compactFromFacts() error = %v", err)
	}
	compact.FormatVersion = legacyCompactFormatVersion
	for i := range compact.Nodes {
		compact.Nodes[i].EndLine = 0
	}
	filename := filepath.Join(t.TempDir(), "legacy.hgc.gz")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "HGC\x01"); err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	if err := gob.NewEncoder(compressed).Encode(compact); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	read, err := ReadCompactGraph(filename)
	if err != nil {
		t.Fatalf("ReadCompactGraph(v1) error = %v", err)
	}
	diff := ParsedDiff{Files: []DiffFile{{
		OldPath: "pkg/impl.go", NewPath: "pkg/impl.go",
		Hunks: []DiffHunk{{OldStart: 5, OldLines: 1, NewStart: 5, NewLines: 1}},
	}}}
	plan, err := PlanCompactDiff(read, diff, read)
	if err != nil {
		t.Fatalf("PlanCompactDiff(v1) error = %v", err)
	}
	if got, want := plan.CandidateTests, []string{testRoot}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy candidate tests = %v, want %v", got, want)
	}
}

func TestPlanGraphDiffFilesSupportsMixedSnapshotFormats(t *testing.T) {
	const testRoot = "pkg/flow_test.go::TestFlow"
	baseFacts := sourceFacts{
		Nodes: []GraphNode{
			{ID: "pkg/old.go::Removed", Path: "pkg/old.go", Qualname: "Removed", Line: 5, EndLine: 8},
			{ID: testRoot, Path: "pkg/flow_test.go", Qualname: "TestFlow", Line: 2, EndLine: 7, IsTest: true},
		},
		Edges: []sourceEdge{{Caller: testRoot, Callee: "pkg/old.go::Removed"}},
		Files: []FileMetadata{{Path: "pkg/old.go", Package: "flow"}, {Path: "pkg/flow_test.go", Package: "flow"}},
	}
	headFacts := sourceFacts{
		Nodes: []GraphNode{
			{ID: "pkg/new.go::Added", Path: "pkg/new.go", Qualname: "Added", Line: 5, EndLine: 8},
			{ID: testRoot, Path: "pkg/flow_test.go", Qualname: "TestFlow", Line: 2, EndLine: 7, IsTest: true},
		},
		Edges: []sourceEdge{{Caller: testRoot, Callee: "pkg/new.go::Added"}},
		Files: []FileMetadata{{Path: "pkg/new.go", Package: "flow"}, {Path: "pkg/flow_test.go", Package: "flow"}},
	}
	baseGraph := graphFromPlannerFacts(t, baseFacts)
	headGraph := graphFromPlannerFacts(t, headFacts)
	previous, err := compactFromFacts(baseFacts, baseGraph.SourceFingerprint, baseGraph.FileCount)
	if err != nil {
		t.Fatalf("compactFromFacts() error = %v", err)
	}
	currentFile := filepath.Join(t.TempDir(), "head.json")
	if err := os.WriteFile(currentFile, mustMarshalPlannerGraph(t, headGraph), 0o644); err != nil {
		t.Fatal(err)
	}
	previousFile := filepath.Join(t.TempDir(), "base.hgc.gz")
	if err := WriteCompactGraph(previousFile, previous); err != nil {
		t.Fatalf("WriteCompactGraph() error = %v", err)
	}
	diff := ParsedDiff{Files: []DiffFile{{
		OldPath: "pkg/old.go", NewPath: "pkg/new.go",
		Hunks: []DiffHunk{{OldStart: 6, OldLines: 1, NewStart: 6, NewLines: 1}},
	}}}
	want, err := PlanDiff(headGraph, diff, baseGraph)
	if err != nil {
		t.Fatalf("PlanDiff() error = %v", err)
	}
	got, err := PlanGraphDiffFiles(currentFile, diff, previousFile)
	if err != nil {
		t.Fatalf("PlanGraphDiffFiles() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed file plan differs:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestPlanDiffRejectsMalformedParsedInput(t *testing.T) {
	const testRoot = "pkg/impl_test.go::TestChanged"
	graph := graphFromPlannerFacts(t, sourceFacts{
		Nodes: []GraphNode{
			{ID: "pkg/impl.go::Changed", Path: "pkg/impl.go", Qualname: "Changed", Line: 4, EndLine: 6},
			{ID: testRoot, Path: "pkg/impl_test.go", Qualname: "TestChanged", Line: 2, EndLine: 4, IsTest: true},
		},
		Edges: []sourceEdge{{Caller: testRoot, Callee: "pkg/impl.go::Changed"}},
		Files: []FileMetadata{{Path: "pkg/impl.go", Package: "impl"}, {Path: "pkg/impl_test.go", Package: "impl"}},
	})
	tests := []struct {
		name string
		diff ParsedDiff
	}{
		{name: "no files"},
		{name: "path escapes repository", diff: ParsedDiff{Files: []DiffFile{{NewPath: "../impl.go"}}}},
		{name: "negative line count", diff: ParsedDiff{Files: []DiffFile{{NewPath: "pkg/impl.go", Hunks: []DiffHunk{{NewLines: -1}}}}}},
		{name: "zero start for non-empty range", diff: ParsedDiff{Files: []DiffFile{{NewPath: "pkg/impl.go", Hunks: []DiffHunk{{NewStart: 0, NewLines: 1}}}}}},
		{name: "range without matching path", diff: ParsedDiff{Files: []DiffFile{{NewPath: "pkg/impl.go", Hunks: []DiffHunk{{OldStart: 1, OldLines: 1}}}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PlanDiff(graph, test.diff, graph); err == nil {
				t.Fatal("PlanDiff() accepted malformed parsed input")
			}
		})
	}
}

func graphFromPlannerFacts(t *testing.T, facts sourceFacts) *Graph {
	t.Helper()
	graph, err := graphFromFacts(facts, "diff-planner-fixture", len(facts.Files))
	if err != nil {
		t.Fatalf("graphFromFacts() error = %v", err)
	}
	return graph
}

func mustMarshalPlannerGraph(t *testing.T, graph *Graph) []byte {
	t.Helper()
	data, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertSortedUniquePlannerValues(t *testing.T, values []string) {
	t.Helper()
	for i := 1; i < len(values); i++ {
		if values[i-1] >= values[i] {
			t.Fatalf("values are not sorted and unique: %v", values)
		}
	}
}

func writePlannerFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, relative), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
