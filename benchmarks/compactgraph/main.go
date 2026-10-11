// Command compactgraph compares graph-v2 gzip, the historical experimental
// bitset/gob artifact, and the supported direct compact artifact. Only the
// direct compact artifact is a supported Hexagon CI graph format.
package main

import (
	"compress/gzip"
	"encoding/gob"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/aayushhzz/hexagon-ci/hexagon"
)

// Node positions are the function IDs. The global edge and unresolved lists
// are stored once; per-test membership is a bitset instead of three lists.
type compactGraph struct {
	FormatVersion       int
	Language            string
	SourceFingerprint   string
	AnalysisContext     hexagon.BuildContext
	AnalysisFingerprint string
	FileCount           int
	Files               []hexagon.FileMetadata
	Nodes               []hexagon.GraphNode
	Edges               []compactEdge
	Unresolved          []compactCall
	Tests               []compactTest
}

type compactEdge struct {
	Caller, Callee uint32
	Callsites      []hexagon.CallSite
}

type compactCall struct {
	Caller     uint32
	Expression string
	Path       string
	Line       int
}

type compactTest struct {
	Root      uint32
	Reachable []byte
}

type callRange struct{ start, end int }

type lookup struct {
	graph     *compactGraph
	adjacency [][]uint32
	calls     []callRange
	tests     map[uint32]int
}

func fromNamed(graph *hexagon.Graph) (*compactGraph, error) {
	if uint64(len(graph.Nodes)) > uint64(^uint32(0)) {
		return nil, errors.New("too many nodes for uint32 IDs")
	}
	result := &compactGraph{
		FormatVersion: 3, Language: graph.Language,
		SourceFingerprint: graph.SourceFingerprint, AnalysisContext: graph.AnalysisContext,
		AnalysisFingerprint: graph.AnalysisFingerprint, FileCount: graph.FileCount,
		Files: graph.Files, Nodes: graph.Nodes,
		Edges:      make([]compactEdge, 0, len(graph.Edges)),
		Unresolved: make([]compactCall, 0, len(graph.UnresolvedCalls)),
		Tests:      make([]compactTest, 0, len(graph.Tests)),
	}
	ids := make(map[string]uint32, len(graph.Nodes))
	for index, node := range graph.Nodes {
		ids[node.ID] = uint32(index)
	}
	for _, edge := range graph.Edges {
		caller, ok := ids[edge.Caller]
		if !ok {
			return nil, fmt.Errorf("missing caller %s", edge.Caller)
		}
		callee, ok := ids[edge.Callee]
		if !ok {
			return nil, fmt.Errorf("missing callee %s", edge.Callee)
		}
		result.Edges = append(result.Edges, compactEdge{caller, callee, edge.Callsites})
	}
	for _, call := range graph.UnresolvedCalls {
		caller, ok := ids[call.Caller]
		if !ok {
			return nil, fmt.Errorf("missing unresolved caller %s", call.Caller)
		}
		result.Unresolved = append(result.Unresolved, compactCall{caller, call.Expression, call.Path, call.Line})
	}
	roots := make([]string, 0, len(graph.Tests))
	for root := range graph.Tests {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, root := range roots {
		rootID, ok := ids[root]
		if !ok {
			return nil, fmt.Errorf("missing test root %s", root)
		}
		membership := make([]byte, (len(graph.Nodes)+7)/8)
		for _, node := range graph.Tests[root].Nodes {
			id, ok := ids[node]
			if !ok {
				return nil, fmt.Errorf("missing reached node %s", node)
			}
			membership[id/8] |= byte(1 << (id % 8))
		}
		result.Tests = append(result.Tests, compactTest{rootID, membership})
	}
	return result, nil
}

func newLookup(graph *compactGraph) (*lookup, error) {
	if graph.FormatVersion != 3 || graph.Language != "go" {
		return nil, errors.New("unsupported compact graph")
	}
	result := &lookup{
		graph:     graph,
		adjacency: make([][]uint32, len(graph.Nodes)),
		calls:     make([]callRange, len(graph.Nodes)),
		tests:     make(map[uint32]int, len(graph.Tests)),
	}
	for _, edge := range graph.Edges {
		if int(edge.Caller) >= len(graph.Nodes) || int(edge.Callee) >= len(graph.Nodes) {
			return nil, errors.New("invalid edge ID")
		}
		result.adjacency[edge.Caller] = append(result.adjacency[edge.Caller], edge.Callee)
	}
	for i, call := range graph.Unresolved {
		if int(call.Caller) >= len(graph.Nodes) {
			return nil, errors.New("invalid unresolved caller ID")
		}
		group := &result.calls[call.Caller]
		if group.end == 0 {
			group.start = i
		}
		group.end = i + 1
	}
	for i, test := range graph.Tests {
		if int(test.Root) >= len(graph.Nodes) || len(test.Reachable) != (len(graph.Nodes)+7)/8 {
			return nil, errors.New("invalid test membership")
		}
		result.tests[test.Root] = i
	}
	return result, nil
}

func (index *lookup) view(root uint32) (hexagon.TestGraph, error) {
	i, ok := index.tests[root]
	if !ok {
		return hexagon.TestGraph{}, fmt.Errorf("unknown test root %d", root)
	}
	view := hexagon.TestGraph{
		Nodes: make([]string, 0), Edges: make([]hexagon.TestGraphEdge, 0),
		UnresolvedCalls: make([]hexagon.UnresolvedCall, 0),
	}
	for byteIndex, membership := range index.graph.Tests[i].Reachable {
		for bit := uint32(0); bit < 8; bit++ {
			id := uint32(byteIndex)*8 + bit
			if membership&(1<<bit) == 0 || int(id) >= len(index.graph.Nodes) {
				continue
			}
			name := index.graph.Nodes[id].ID
			view.Nodes = append(view.Nodes, name)
			for _, callee := range index.adjacency[id] {
				view.Edges = append(view.Edges, hexagon.TestGraphEdge{Caller: name, Callee: index.graph.Nodes[callee].ID})
			}
			group := index.calls[id]
			for _, call := range index.graph.Unresolved[group.start:group.end] {
				view.UnresolvedCalls = append(view.UnresolvedCalls, hexagon.UnresolvedCall{
					Caller: name, Expression: call.Expression, Path: call.Path, Line: call.Line,
				})
			}
		}
	}
	return view, nil
}

func writeCompact(filename string, graph *compactGraph) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	writer := gzip.NewWriter(file)
	if err := gob.NewEncoder(writer).Encode(graph); err != nil {
		_ = writer.Close()
		_ = file.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readCompact(filename string) (*compactGraph, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var graph compactGraph
	if err := gob.NewDecoder(reader).Decode(&graph); err != nil {
		return nil, err
	}
	// Gob does not distinguish an empty slice from nil. Graph v2 encodes these
	// as [] and includes them in its public JSON contract.
	if graph.AnalysisContext.BuildTags == nil {
		graph.AnalysisContext.BuildTags = []string{}
	}
	for i := range graph.Files {
		if graph.Files[i].Imports == nil {
			graph.Files[i].Imports = []string{}
		}
	}
	return &graph, nil
}

func selectedRoots(roots []string, largest string) []string {
	sort.Strings(roots)
	return []string{roots[0], roots[len(roots)/2], roots[len(roots)-1], largest}
}

func queryNamed(graph *hexagon.Graph, loops int) (int, time.Duration, error) {
	roots := make([]string, 0, len(graph.Tests))
	for root := range graph.Tests {
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return 0, 0, errors.New("no test roots")
	}
	sort.Strings(roots)
	largest := roots[0]
	for _, root := range roots {
		if len(graph.Tests[root].Nodes) > len(graph.Tests[largest].Nodes) {
			largest = root
		}
	}
	roots = selectedRoots(roots, largest)
	start := time.Now()
	var totalBytes int
	for i := 0; i < loops; i++ {
		encoded, err := json.Marshal(graph.Tests[roots[i%len(roots)]])
		if err != nil {
			return 0, 0, err
		}
		totalBytes += len(encoded)
	}
	return totalBytes, time.Since(start), nil
}

func queryCompact(index *lookup, loops int) (int, time.Duration, error) {
	roots := make([]string, len(index.graph.Tests))
	ids := make(map[string]uint32, len(roots))
	for i, test := range index.graph.Tests {
		root := index.graph.Nodes[test.Root].ID
		roots[i] = root
		ids[root] = test.Root
	}
	if len(roots) == 0 {
		return 0, 0, errors.New("no test roots")
	}
	sort.Strings(roots)
	largest := roots[0]
	largestCount := -1
	for _, root := range roots {
		count := 0
		for _, value := range index.graph.Tests[index.tests[ids[root]]].Reachable {
			count += bits.OnesCount8(value)
		}
		if count > largestCount {
			largest, largestCount = root, count
		}
	}
	roots = selectedRoots(roots, largest)
	start := time.Now()
	var totalBytes int
	for i := 0; i < loops; i++ {
		view, err := index.view(ids[roots[i%len(roots)]])
		if err != nil {
			return 0, 0, err
		}
		encoded, err := json.Marshal(view)
		if err != nil {
			return 0, 0, err
		}
		totalBytes += len(encoded)
	}
	return totalBytes, time.Since(start), nil
}

func queryDirect(index *hexagon.CompactIndex, graph *hexagon.CompactGraph, loops int) (int, time.Duration, error) {
	roots := make([]string, len(graph.Tests))
	for i, test := range graph.Tests {
		roots[i] = graph.Nodes[test.Root].ID
	}
	if len(roots) == 0 {
		return 0, 0, errors.New("no test roots")
	}
	sort.Strings(roots)
	largest, largestCount := roots[0], -1
	for _, test := range graph.Tests {
		count := len(test.Reachable)
		if count > largestCount {
			largest, largestCount = graph.Nodes[test.Root].ID, count
		}
	}
	roots = selectedRoots(roots, largest)
	start := time.Now()
	var totalBytes int
	for i := 0; i < loops; i++ {
		view, err := index.TestGraph(roots[i%len(roots)])
		if err != nil {
			return 0, 0, err
		}
		encoded, err := json.Marshal(view)
		if err != nil {
			return 0, 0, err
		}
		totalBytes += len(encoded)
	}
	return totalBytes, time.Since(start), nil
}

func run(mode, repo, filename, namedFile, compactFile string, loops int) (map[string]any, error) {
	result := map[string]any{"mode": mode}
	start := time.Now()
	switch mode {
	case "build-v2":
		graph, err := hexagon.RebuildToFile(repo, filename, nil)
		if err != nil {
			return nil, err
		}
		result["tests"] = len(graph.Tests)
	case "build-compact":
		graph, err := hexagon.BuildGraph(repo, nil)
		if err != nil {
			return nil, err
		}
		result["build_ms"] = time.Since(start).Milliseconds()
		convertStart := time.Now()
		compact, err := fromNamed(graph)
		if err != nil {
			return nil, err
		}
		result["convert_ms"] = time.Since(convertStart).Milliseconds()
		writeStart := time.Now()
		if err := writeCompact(filename, compact); err != nil {
			return nil, err
		}
		result["write_ms"] = time.Since(writeStart).Milliseconds()
		result["tests"] = len(compact.Tests)
	case "build-direct-compact":
		graph, err := hexagon.RebuildCompactToFile(repo, filename, nil)
		if err != nil {
			return nil, err
		}
		result["tests"] = len(graph.Tests)
	case "read-v2":
		graph, err := hexagon.ReadGraph(filename)
		if err != nil {
			return nil, err
		}
		result["load_ms"] = time.Since(start).Milliseconds()
		bytes, elapsed, err := queryNamed(graph, loops)
		if err != nil {
			return nil, err
		}
		result["query_ms"] = elapsed.Milliseconds()
		result["query_bytes"] = bytes
	case "read-compact":
		graph, err := readCompact(filename)
		if err != nil {
			return nil, err
		}
		index, err := newLookup(graph)
		if err != nil {
			return nil, err
		}
		result["load_ms"] = time.Since(start).Milliseconds()
		bytes, elapsed, err := queryCompact(index, loops)
		if err != nil {
			return nil, err
		}
		result["query_ms"] = elapsed.Milliseconds()
		result["query_bytes"] = bytes
	case "read-direct-compact":
		graph, err := hexagon.ReadCompactGraph(filename)
		if err != nil {
			return nil, err
		}
		index, err := hexagon.NewCompactIndex(graph)
		if err != nil {
			return nil, err
		}
		result["load_ms"] = time.Since(start).Milliseconds()
		bytes, elapsed, err := queryDirect(index, graph, loops)
		if err != nil {
			return nil, err
		}
		result["query_ms"] = elapsed.Milliseconds()
		result["query_bytes"] = bytes
	case "stats-direct":
		graph, err := hexagon.ReadCompactGraph(filename)
		if err != nil {
			return nil, err
		}
		bitsetBytes, sparseBytes, sparseTests, reached := 0, 0, 0, 0
		for _, test := range graph.Tests {
			count := len(test.Reachable)
			bitsetBytes += (len(graph.Nodes) + 7) / 8
			sparseBytes += count * 4
			reached += count
			if count*4 < (len(graph.Nodes)+7)/8 {
				sparseTests++
			}
		}
		result["nodes"] = len(graph.Nodes)
		result["tests"] = len(graph.Tests)
		result["reached_total"] = reached
		result["bitset_bytes"] = bitsetBytes
		result["sparse_bytes"] = sparseBytes
		result["sparse_tests"] = sparseTests
	case "validate-direct":
		named, err := hexagon.ReadGraph(namedFile)
		if err != nil {
			return nil, err
		}
		compact, err := hexagon.ReadCompactGraph(compactFile)
		if err != nil {
			return nil, err
		}
		expanded, err := compact.ToGraph()
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(named, expanded) {
			return nil, errors.New("production compact graph differs from named graph v2")
		}
		result["tests_checked"] = len(compact.Tests)
	case "validate":
		named, err := hexagon.ReadGraph(namedFile)
		if err != nil {
			return nil, err
		}
		compact, err := readCompact(compactFile)
		if err != nil {
			return nil, err
		}
		if named.SourceFingerprint != compact.SourceFingerprint ||
			named.AnalysisFingerprint != compact.AnalysisFingerprint ||
			!reflect.DeepEqual(named.Nodes, compact.Nodes) || !reflect.DeepEqual(named.Files, compact.Files) ||
			len(named.Edges) != len(compact.Edges) || len(named.UnresolvedCalls) != len(compact.Unresolved) ||
			len(named.Tests) != len(compact.Tests) {
			return nil, fmt.Errorf("top-level graph facts differ: source=%t analysis=%t nodes=%t files=%t edges=%d/%d unresolved=%d/%d tests=%d/%d",
				named.SourceFingerprint == compact.SourceFingerprint, named.AnalysisFingerprint == compact.AnalysisFingerprint,
				reflect.DeepEqual(named.Nodes, compact.Nodes), reflect.DeepEqual(named.Files, compact.Files),
				len(named.Edges), len(compact.Edges), len(named.UnresolvedCalls), len(compact.Unresolved),
				len(named.Tests), len(compact.Tests))
		}
		index, err := newLookup(compact)
		if err != nil {
			return nil, err
		}
		for i, edge := range named.Edges {
			other := compact.Edges[i]
			if edge.Caller != compact.Nodes[other.Caller].ID || edge.Callee != compact.Nodes[other.Callee].ID ||
				!reflect.DeepEqual(edge.Callsites, other.Callsites) {
				return nil, fmt.Errorf("edge %d differs", i)
			}
		}
		for i, call := range named.UnresolvedCalls {
			other := compact.Unresolved[i]
			if call.Caller != compact.Nodes[other.Caller].ID || call.Expression != other.Expression ||
				call.Path != other.Path || call.Line != other.Line {
				return nil, fmt.Errorf("unresolved call %d differs", i)
			}
		}
		for _, test := range compact.Tests {
			root := compact.Nodes[test.Root].ID
			view, err := index.view(test.Root)
			if err != nil || !reflect.DeepEqual(view, named.Tests[root]) {
				return nil, fmt.Errorf("test view %s differs: %v", root, err)
			}
		}
		result["tests_checked"] = len(compact.Tests)
	default:
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
	result["total_ms"] = time.Since(start).Milliseconds()
	if mode != "validate" && mode != "validate-direct" {
		info, err := os.Stat(filename)
		if err != nil {
			return nil, err
		}
		result["artifact_bytes"] = info.Size()
	}
	return result, nil
}

func main() {
	mode := flag.String("mode", "", "build-v2, build-compact, build-direct-compact, read-v2, read-compact, read-direct-compact, stats-direct, validate, or validate-direct")
	repo := flag.String("repo", "", "repository for build modes")
	filename := flag.String("file", "", "artifact to write or read")
	named := flag.String("named", "", "graph-v2 artifact for validation")
	compact := flag.String("compact", "", "compact artifact for validation")
	loops := flag.Int("queries", 300, "named test-view queries for read modes")
	flag.Parse()
	if *mode != "validate" && *mode != "validate-direct" && *filename == "" {
		fmt.Fprintln(os.Stderr, "-file is required")
		os.Exit(2)
	}
	if *mode == "build-compact" || *mode == "build-direct-compact" {
		if err := os.MkdirAll(filepath.Dir(*filename), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	result, err := run(*mode, *repo, *filename, *named, *compact, *loops)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
