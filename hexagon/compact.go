package hexagon

import (
	"compress/gzip"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const compactFormatVersion = 1
const compactMagic = "HGC\x01"

// CompactGraph stores global call facts once and uses sorted function IDs for
// each test's reachability. It carries the same source and analysis identity as
// Graph v2; its gob wire representation is specific to this Go implementation.
type CompactGraph struct {
	FormatVersion       int
	Language            string
	SourceFingerprint   string
	AnalysisContext     BuildContext
	AnalysisFingerprint string
	FileCount           int
	Files               []FileMetadata
	Nodes               []GraphNode
	Edges               []CompactEdge
	Unresolved          []CompactCall
	Tests               []CompactTest
}

type CompactEdge struct {
	Caller, Callee uint32
	Callsites      []CallSite
}

type CompactCall struct {
	Caller     uint32
	Expression string
	Path       string
	Line       int
}

type CompactTest struct {
	Root      uint32
	Reachable []uint32
}

// BuildCompactGraph analyzes the repository without materializing named views
// for every test. The same graph preparation and traversal are used by v2.
func BuildCompactGraph(repository string, tags []string) (*CompactGraph, error) {
	snapshot, err := analyzeRepository(repository, tags)
	if err != nil {
		return nil, err
	}
	graph, err := compactFromFacts(snapshot.facts, snapshot.sourceHash, snapshot.fileCount)
	if err != nil {
		return nil, err
	}
	if err := snapshot.verifyUnchanged(); err != nil {
		return nil, err
	}
	return graph, nil
}

func compactFromFacts(facts sourceFacts, sourceHash string, fileCount int) (*CompactGraph, error) {
	prepared, err := prepareGraph(facts, sourceHash, fileCount)
	if err != nil {
		return nil, err
	}
	graph := prepared.compact
	graph.Tests = make([]CompactTest, 0, len(prepared.testRoots))
	visited := make([]uint32, len(graph.Nodes))
	var epoch uint32
	pending := make([]functionID, 0)
	reached := make([]functionID, 0)
	for _, root := range prepared.testRoots {
		epoch = nextEpoch(visited, epoch)
		reached, pending = prepared.reachable(root, visited, epoch, reached, pending)
		sort.Slice(reached, func(i, j int) bool { return reached[i] < reached[j] })
		membership := make([]uint32, len(reached))
		for i, id := range reached {
			membership[i] = uint32(id)
		}
		graph.Tests = append(graph.Tests, CompactTest{Root: uint32(root), Reachable: membership})
	}
	return graph, nil
}

// RebuildCompactToFile publishes a complete snapshot atomically. The file is
// selected explicitly by the caller; existing graph-v2 paths remain unchanged.
func RebuildCompactToFile(repository, output string, tags []string) (*CompactGraph, error) {
	graph, err := BuildCompactGraph(repository, tags)
	if err != nil {
		return nil, err
	}
	if err := WriteCompactGraph(output, graph); err != nil {
		return nil, err
	}
	return graph, nil
}

// WriteCompactGraph writes magic, a gzip stream, and one versioned gob value.
func WriteCompactGraph(filename string, graph *CompactGraph) error {
	if err := graph.validate(); err != nil {
		return err
	}
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	file, err := os.CreateTemp(directory, filepath.Base(filename)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err = io.WriteString(file, compactMagic); err == nil {
		writer := gzip.NewWriter(file)
		writer.Header.ModTime = time.Time{}
		err = gob.NewEncoder(writer).Encode(graph)
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("encode compact graph: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync compact graph: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close compact graph: %w", err)
	}
	if err := os.Rename(file.Name(), filename); err != nil {
		return fmt.Errorf("replace compact graph: %w", err)
	}
	return nil
}

// ReadCompactGraph verifies the header, compressed stream, IDs, and test sets.
func ReadCompactGraph(filename string) (*CompactGraph, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	header := make([]byte, len(compactMagic))
	if _, err := io.ReadFull(file, header); err != nil || string(header) != compactMagic {
		return nil, fmt.Errorf("not a supported compact graph: %s", filename)
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open compact graph: %w", err)
	}
	defer reader.Close()
	decoder := gob.NewDecoder(reader)
	var graph CompactGraph
	if err := decoder.Decode(&graph); err != nil {
		return nil, fmt.Errorf("decode compact graph: %w", err)
	}
	var extra CompactGraph
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing or corrupt compact graph data: %v", err)
	}
	graph.normalizeEmptySlices()
	if err := graph.validate(); err != nil {
		return nil, err
	}
	return &graph, nil
}

func (graph *CompactGraph) normalizeEmptySlices() {
	if graph.AnalysisContext.BuildTags == nil {
		graph.AnalysisContext.BuildTags = []string{}
	}
	if graph.Files == nil {
		graph.Files = []FileMetadata{}
	}
	if graph.Nodes == nil {
		graph.Nodes = []GraphNode{}
	}
	if graph.Edges == nil {
		graph.Edges = []CompactEdge{}
	}
	if graph.Unresolved == nil {
		graph.Unresolved = []CompactCall{}
	}
	if graph.Tests == nil {
		graph.Tests = []CompactTest{}
	}
	for i := range graph.Files {
		if graph.Files[i].Imports == nil {
			graph.Files[i].Imports = []string{}
		}
	}
}

func (graph *CompactGraph) validate() error {
	if graph == nil || graph.FormatVersion != compactFormatVersion || graph.Language != "go" {
		return errors.New("unsupported compact graph version or language")
	}
	if uint64(len(graph.Nodes)) > uint64(^uint32(0)) {
		return errors.New("too many compact graph nodes for uint32 IDs")
	}
	for i, node := range graph.Nodes {
		if i > 0 && graph.Nodes[i-1].ID >= node.ID {
			return errors.New("compact graph nodes are not uniquely sorted")
		}
	}
	for i, edge := range graph.Edges {
		if uint64(edge.Caller) >= uint64(len(graph.Nodes)) || uint64(edge.Callee) >= uint64(len(graph.Nodes)) ||
			(i > 0 && (graph.Edges[i-1].Caller > edge.Caller ||
				(graph.Edges[i-1].Caller == edge.Caller && graph.Edges[i-1].Callee >= edge.Callee))) {
			return fmt.Errorf("invalid compact edge %d", i)
		}
	}
	for i, call := range graph.Unresolved {
		if uint64(call.Caller) >= uint64(len(graph.Nodes)) || (i > 0 && graph.Unresolved[i-1].Caller > call.Caller) {
			return fmt.Errorf("invalid compact unresolved call %d", i)
		}
	}
	count := 0
	for _, node := range graph.Nodes {
		if node.IsTest {
			count++
		}
	}
	if len(graph.Tests) != count {
		return errors.New("compact graph test inventory differs from its test nodes")
	}
	for i, test := range graph.Tests {
		if uint64(test.Root) >= uint64(len(graph.Nodes)) || !graph.Nodes[test.Root].IsTest ||
			len(test.Reachable) == 0 ||
			(i > 0 && graph.Tests[i-1].Root >= test.Root) {
			return fmt.Errorf("invalid compact test %d", i)
		}
		rootFound := false
		for j, id := range test.Reachable {
			if uint64(id) >= uint64(len(graph.Nodes)) || (j > 0 && test.Reachable[j-1] >= id) {
				return fmt.Errorf("invalid reachable function ID in compact test %d", i)
			}
			rootFound = rootFound || id == test.Root
		}
		if !rootFound {
			return fmt.Errorf("compact test %d does not reach its root", i)
		}
	}
	return nil
}

// CompactIndex resolves function names and builds the caller ranges used to
// reconstruct one named test view without expanding every test in memory.
type CompactIndex struct {
	graph     *CompactGraph
	ids       map[string]uint32
	adjacency [][]uint32
	calls     []callerRange
	tests     map[uint32]int
}

func NewCompactIndex(graph *CompactGraph) (*CompactIndex, error) {
	if err := graph.validate(); err != nil {
		return nil, err
	}
	index := &CompactIndex{
		graph: graph, ids: make(map[string]uint32, len(graph.Nodes)),
		adjacency: make([][]uint32, len(graph.Nodes)), calls: make([]callerRange, len(graph.Nodes)),
		tests: make(map[uint32]int, len(graph.Tests)),
	}
	for i, node := range graph.Nodes {
		index.ids[node.ID] = uint32(i)
	}
	for _, edge := range graph.Edges {
		index.adjacency[edge.Caller] = append(index.adjacency[edge.Caller], edge.Callee)
	}
	for i, call := range graph.Unresolved {
		group := &index.calls[call.Caller]
		if group.end == 0 {
			group.start = i
		}
		group.end = i + 1
	}
	for i, test := range graph.Tests {
		index.tests[test.Root] = i
	}
	return index, nil
}

func (index *CompactIndex) forEachTestNode(root string, visit func(uint32) bool) error {
	id, exists := index.ids[root]
	if !exists {
		return fmt.Errorf("unknown test root %q", root)
	}
	i, exists := index.tests[id]
	if !exists {
		return fmt.Errorf("unknown test root %q", root)
	}
	for _, id := range index.graph.Tests[i].Reachable {
		if !visit(id) {
			return nil
		}
	}
	return nil
}

// TestGraph expands one compact test view with the same ordering as graph v2.
func (index *CompactIndex) TestGraph(root string) (TestGraph, error) {
	view := TestGraph{
		Nodes: make([]string, 0), Edges: make([]TestGraphEdge, 0),
		UnresolvedCalls: make([]UnresolvedCall, 0),
	}
	err := index.forEachTestNode(root, func(id uint32) bool {
		name := index.graph.Nodes[id].ID
		view.Nodes = append(view.Nodes, name)
		for _, callee := range index.adjacency[id] {
			view.Edges = append(view.Edges, TestGraphEdge{Caller: name, Callee: index.graph.Nodes[callee].ID})
		}
		group := index.calls[id]
		for _, call := range index.graph.Unresolved[group.start:group.end] {
			view.UnresolvedCalls = append(view.UnresolvedCalls, UnresolvedCall{
				Caller: name, Expression: call.Expression, Path: call.Path, Line: call.Line,
			})
		}
		return true
	})
	return view, err
}

// ToGraph explicitly expands the compact snapshot for graph-v2 consumers.
func (graph *CompactGraph) ToGraph() (*Graph, error) {
	index, err := NewCompactIndex(graph)
	if err != nil {
		return nil, err
	}
	tests := make(map[string]TestGraph, len(graph.Tests))
	for _, test := range graph.Tests {
		root := graph.Nodes[test.Root].ID
		tests[root], err = index.TestGraph(root)
		if err != nil {
			return nil, err
		}
	}
	edges := make([]GraphEdge, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges = append(edges, GraphEdge{
			Caller: graph.Nodes[edge.Caller].ID, Callee: graph.Nodes[edge.Callee].ID, Callsites: edge.Callsites,
		})
	}
	unresolved := make([]UnresolvedCall, 0, len(graph.Unresolved))
	for _, call := range graph.Unresolved {
		unresolved = append(unresolved, UnresolvedCall{
			Caller: graph.Nodes[call.Caller].ID, Expression: call.Expression, Path: call.Path, Line: call.Line,
		})
	}
	return &Graph{
		FormatVersion: graphFormatVersion, Language: graph.Language, SourceFingerprint: graph.SourceFingerprint,
		AnalysisContext: graph.AnalysisContext, AnalysisFingerprint: graph.AnalysisFingerprint,
		FileCount: graph.FileCount, Files: graph.Files, Nodes: graph.Nodes, Edges: edges,
		UnresolvedCalls: unresolved, Tests: tests,
	}, nil
}
