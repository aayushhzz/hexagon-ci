package hexagon

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCompactGraphRejectsCorruptMembershipAndPreservesPreviousSnapshot(t *testing.T) {
	root := "test.go::TestRun"
	facts := sourceFacts{
		Nodes: []GraphNode{{ID: "other.go::Other"}, {ID: root, IsTest: true}},
		Edges: []sourceEdge{{Caller: root, Callee: "other.go::Other", Path: "test.go", Line: 3}},
	}
	graph, err := compactFromFacts(facts, "source", 2)
	if err != nil {
		t.Fatal(err)
	}
	named, err := graphFromFacts(facts, "source", 2)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "graph.hgc.gz")
	if err := WriteCompactGraph(path, graph); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readBack, err := ReadCompactGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := readBack.ToGraph()
	if err != nil || !reflect.DeepEqual(named, expanded) {
		t.Fatalf("round-trip changed the graph: %v", err)
	}
	index, err := NewCompactIndex(readBack)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.TestGraph("other.go::Other"); err == nil {
		t.Fatal("non-test node was accepted as a test root")
	}
	graph.Tests[0].Reachable = []uint32{0}
	if err := WriteCompactGraph(path, graph); err == nil {
		t.Fatal("writer accepted a test that cannot reach its root")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("failed write replaced the complete snapshot: %v", err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.hgc.gz")
	if err := os.WriteFile(truncated, original[:len(original)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCompactGraph(truncated); err == nil {
		t.Fatal("truncated compact graph was accepted")
	}
	wrongMagic := filepath.Join(t.TempDir(), "wrong.hgc.gz")
	if err := os.WriteFile(wrongMagic, original[:3], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCompactGraph(wrongMagic); err == nil {
		t.Fatal("incorrect magic was accepted")
	}
}
