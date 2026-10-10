package hexagon

import (
	"bytes"
	"compress/gzip"
	"encoding/gob"
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

func TestReadLegacyCompactGraphWithoutFunctionSpans(t *testing.T) {
	type legacyNode struct {
		ID       string
		Path     string
		Qualname string
		Line     int
		IsTest   bool
	}
	type legacyGraph struct {
		FormatVersion       int
		Language            string
		SourceFingerprint   string
		AnalysisContext     BuildContext
		AnalysisFingerprint string
		FileCount           int
		Files               []FileMetadata
		Nodes               []legacyNode
		Edges               []CompactEdge
		Unresolved          []CompactCall
		Tests               []CompactTest
	}
	path := filepath.Join(t.TempDir(), "legacy.hgc.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("HGC\x01")); err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	legacy := legacyGraph{
		FormatVersion: 1, Language: "go", SourceFingerprint: "legacy",
		Files: []FileMetadata{}, Nodes: []legacyNode{{ID: "legacy.go::Function", Path: "legacy.go", Line: 2}},
		Edges: []CompactEdge{}, Unresolved: []CompactCall{}, Tests: []CompactTest{},
	}
	if err := gob.NewEncoder(writer).Encode(legacy); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := ReadCompactGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.FormatVersion != 1 || got.Nodes[0].EndLine != 0 {
		t.Fatalf("legacy graph = version %d, end line %d; want version 1 with no end line", got.FormatVersion, got.Nodes[0].EndLine)
	}
	if _, err := NewCompactIndex(got); err != nil {
		t.Fatalf("legacy graph should remain readable: %v", err)
	}
}
