package hexagon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAnalyzedRepositoryRejectsChangesBeforePublication(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/demo\n\ngo 1.24.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "app.go")
	if err := os.WriteFile(path, []byte("package demo\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := analyzeRepository(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package demo\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.verifyUnchanged(); err == nil || !strings.Contains(err.Error(), "changed during the rebuild") {
		t.Fatalf("changed source was accepted: %v", err)
	}
}

func TestExternalGRPCClientCallReachesAssertedServerMethod(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/demo\n\ngo 1.24.0\n",
		"server/server.go": `package server
import api "example.com/generated/workflows"
type Registry struct{}
var _ api.WorkflowsServiceServer = (*Registry)(nil)
func (*Registry) ListWorkflows() {}
`,
		"client/client.go": `package client
import api "example.com/generated/workflows"
func GetClient() (api.WorkflowsServiceClient, func()) { return nil, nil }
`,
		"e2e/e2e_test.go": `package e2e
import (
	"testing"
	api "example.com/generated/workflows"
	other "example.com/other/workflows"
	"example.com/demo/client"
)
func check(c api.WorkflowsServiceClient) { c.ListWorkflows() }
func TestWorkflow(t *testing.T) {
	c, close := client.GetClient()
	_ = close
	c.ListWorkflows()
}
func TestTypedHelper(t *testing.T) {
	var c api.WorkflowsServiceClient
	check(c)
}
func TestOtherService(t *testing.T) {
	var c other.WorkflowsServiceClient
	c.ListWorkflows()
}
`,
	}
	for name, contents := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	graph, err := BuildGraph(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	contains := func(root, node string) bool {
		for _, candidate := range graph.Tests[root].Nodes {
			if candidate == node {
				return true
			}
		}
		return false
	}
	handler := "server/server.go::Registry.ListWorkflows"
	if !contains("e2e/e2e_test.go::TestWorkflow", handler) {
		t.Fatalf("gRPC E2E root did not reach %s", handler)
	}
	if !contains("e2e/e2e_test.go::TestTypedHelper", handler) {
		t.Fatalf("typed helper root did not reach %s", handler)
	}
	if contains("e2e/e2e_test.go::TestOtherService", handler) {
		t.Fatalf("unrelated gRPC client incorrectly reached %s", handler)
	}
	// Two implementations of one server interface cannot be mapped to a
	// particular handler without more evidence.
	ambiguous := `package server
import api "example.com/generated/workflows"
type OtherRegistry struct{}
var _ api.WorkflowsServiceServer = (*OtherRegistry)(nil)
func (*OtherRegistry) ListWorkflows() {}
`
	if err := os.WriteFile(filepath.Join(root, "server", "other.go"), []byte(ambiguous), 0o644); err != nil {
		t.Fatal(err)
	}
	graph, err = BuildGraph(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if contains("e2e/e2e_test.go::TestWorkflow", handler) || contains("e2e/e2e_test.go::TestWorkflow", "server/other.go::OtherRegistry.ListWorkflows") {
		t.Fatal("ambiguous service implementations must remain unresolved")
	}
}

func TestFunctionLineSpansRoundTripThroughGraphFormats(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/spans\n\ngo 1.24.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := "package spans\nfunc MultiLine() {\n\t_ = 1\n}\nfunc SingleLine() {}\n"
	if err := os.WriteFile(filepath.Join(root, "functions.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	assertSpans := func(nodes []GraphNode) {
		t.Helper()
		byID := make(map[string]GraphNode, len(nodes))
		for _, node := range nodes {
			byID[node.ID] = node
		}
		if got := byID["functions.go::MultiLine"]; got.Line != 2 || got.EndLine != 4 {
			t.Errorf("MultiLine span = [%d,%d], want [2,4]", got.Line, got.EndLine)
		}
		if got := byID["functions.go::SingleLine"]; got.Line != 5 || got.EndLine != 5 {
			t.Errorf("SingleLine span = [%d,%d], want [5,5]", got.Line, got.EndLine)
		}
	}

	namedPath := filepath.Join(root, "graph.json")
	graph, err := RebuildToFile(root, namedPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSpans(graph.Nodes)
	namedFromDisk, err := ReadGraph(namedPath)
	if err != nil {
		t.Fatal(err)
	}
	assertSpans(namedFromDisk.Nodes)

	compactPath := filepath.Join(root, "graph.hgc.gz")
	compact, err := RebuildCompactToFile(root, compactPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSpans(compact.Nodes)
	compactFromDisk, err := ReadCompactGraph(compactPath)
	if err != nil {
		t.Fatal(err)
	}
	assertSpans(compactFromDisk.Nodes)
}

func TestStreamedGraphJSONPreservesV2Bytes(t *testing.T) {
	for _, roots := range []int{0, 1, 2} {
		graph := &Graph{
			FormatVersion: 2, Language: "go", SourceFingerprint: "a<b>&",
			AnalysisContext: BuildContext{BuildTags: []string{}},
			Files:           []FileMetadata{}, Nodes: []GraphNode{}, Edges: []GraphEdge{},
			UnresolvedCalls: []UnresolvedCall{}, Tests: make(map[string]TestGraph),
		}
		for i := 0; i < roots; i++ {
			root := []string{"a.go::Test<One>", "b.go::TestTwo"}[i]
			graph.Tests[root] = TestGraph{
				Nodes:           []string{root, "shared.go::Function"},
				Edges:           []TestGraphEdge{{Caller: root, Callee: "shared.go::Function"}},
				UnresolvedCalls: []UnresolvedCall{{Caller: root, Expression: "x()", Path: "a.go", Line: 7}},
			}
		}
		want, err := json.MarshalIndent(graph, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, '\n')
		var got bytes.Buffer
		if err := writeGraphJSON(&got, graph); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("streamed graph differs from graph-v2 JSON for %d tests\nwant: %s\ngot: %s", roots, want, got.Bytes())
		}
	}
}

func TestNumericTraversalPreservesNamedViewsAndCallsites(t *testing.T) {
	const first = "a.go::TestFirst"
	const second = "b.go::TestSecond"
	const a = "shared.go::A"
	const b = "shared.go::B"
	const unused = "unused.go::C"
	facts := sourceFacts{
		Nodes: []GraphNode{
			{ID: b}, {ID: unused}, {ID: second, IsTest: true},
			{ID: a}, {ID: first, IsTest: true},
		},
		Edges: []sourceEdge{
			{Caller: a, Callee: b, Path: "shared.go", Line: 9},
			{Caller: second, Callee: b, Path: "b.go", Line: 2},
			{Caller: b, Callee: a, Path: "shared.go", Line: 12},
			{Caller: first, Callee: a, Path: "a.go", Line: 2},
			{Caller: a, Callee: b, Path: "shared.go", Line: 4},
		},
		UnresolvedCalls: []UnresolvedCall{
			{Caller: unused, Expression: "never", Path: "unused.go", Line: 2},
			{Caller: b, Expression: "other", Path: "shared.go", Line: 13},
			{Caller: a, Expression: "later", Path: "shared.go", Line: 10},
			{Caller: a, Expression: "earlier", Path: "shared.go", Line: 3},
		},
	}
	graph, err := graphFromFacts(facts, "snapshot", 4)
	if err != nil {
		t.Fatal(err)
	}
	wantUnresolved := []UnresolvedCall{
		{Caller: a, Expression: "earlier", Path: "shared.go", Line: 3},
		{Caller: a, Expression: "later", Path: "shared.go", Line: 10},
		{Caller: b, Expression: "other", Path: "shared.go", Line: 13},
	}
	for _, tc := range []struct {
		root  string
		nodes []string
		edges []TestGraphEdge
	}{
		{first, []string{first, a, b}, []TestGraphEdge{{first, a}, {a, b}, {b, a}}},
		{second, []string{second, a, b}, []TestGraphEdge{{second, b}, {a, b}, {b, a}}},
	} {
		view := graph.Tests[tc.root]
		if !reflect.DeepEqual(view.Nodes, tc.nodes) || !reflect.DeepEqual(view.Edges, tc.edges) ||
			!reflect.DeepEqual(view.UnresolvedCalls, wantUnresolved) {
			t.Fatalf("view for %s = %+v, want nodes %v, edges %v, unresolved %v",
				tc.root, view, tc.nodes, tc.edges, wantUnresolved)
		}
	}
	for _, edge := range graph.Edges {
		if edge.Caller == a && edge.Callee == b {
			if !reflect.DeepEqual(edge.Callsites, []CallSite{{"shared.go", 4}, {"shared.go", 9}}) {
				t.Fatalf("duplicate callsites were not sorted and grouped: %v", edge.Callsites)
			}
			return
		}
	}
	t.Fatal("missing aggregated edge")
}
