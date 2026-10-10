package hexagon

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/aayushhzz/hexagon-ci/hexagon/internal/gosource"
)

//go:embed graph.go compact.go planner.go internal/gosource/analyzer.go
var analyzerSources embed.FS

const graphFormatVersion = 2

var ignoredDirectories = map[string]bool{
	".git": true, "vendor": true, "node_modules": true, "build": true,
	"dist": true, "__pycache__": true,
}

type BuildContext struct {
	GOOS           string   `json:"goos"`
	GOARCH         string   `json:"goarch"`
	GoVersion      string   `json:"go_version"`
	Compiler       string   `json:"compiler"`
	CgoEnabled     bool     `json:"cgo_enabled"`
	BuildTags      []string `json:"build_tags"`
	AnalyzerSHA256 string   `json:"analyzer_sha256"`
}

type FileMetadata struct {
	Path    string   `json:"path"`
	Package string   `json:"package"`
	Imports []string `json:"imports"`
}

type GraphNode struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Qualname string `json:"qualname"`
	Line     int    `json:"line"`
	IsTest   bool   `json:"is_test"`
}

type CallSite struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

type GraphEdge struct {
	Caller    string     `json:"caller"`
	Callee    string     `json:"callee"`
	Callsites []CallSite `json:"callsites"`
}

type UnresolvedCall struct {
	Caller     string `json:"caller"`
	Expression string `json:"expression"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
}

type TestGraph struct {
	Nodes           []string         `json:"nodes"`
	Edges           []TestGraphEdge  `json:"edges"`
	UnresolvedCalls []UnresolvedCall `json:"unresolved_calls"`
}

type TestGraphEdge struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
}

// Graph keeps the v2 JSON contract used by the archived Python implementation.
type Graph struct {
	FormatVersion       int                  `json:"format_version"`
	Language            string               `json:"language"`
	SourceFingerprint   string               `json:"source_fingerprint"`
	AnalysisContext     BuildContext         `json:"analysis_context"`
	AnalysisFingerprint string               `json:"analysis_fingerprint"`
	FileCount           int                  `json:"file_count"`
	Files               []FileMetadata       `json:"files"`
	Nodes               []GraphNode          `json:"nodes"`
	Edges               []GraphEdge          `json:"edges"`
	UnresolvedCalls     []UnresolvedCall     `json:"unresolved_calls"`
	Tests               map[string]TestGraph `json:"tests"`
}

type sourceFacts struct {
	Nodes           []GraphNode      `json:"nodes"`
	Edges           []sourceEdge     `json:"edges"`
	UnresolvedCalls []UnresolvedCall `json:"unresolved_calls"`
	Files           []FileMetadata   `json:"files"`
	BuildContext    BuildContext     `json:"build_context"`
}

type sourceEdge struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
}

// functionID is the position of a function in the name-sorted node table.
// Traversal uses compact IDs; graph v2 is materialized with names at the end.
type functionID uint32

type numericCallPair struct{ caller, callee functionID }
type callerRange struct{ start, end int }

type preparedGraph struct {
	compact            *CompactGraph
	names              []string
	adjacency          [][]functionID
	unresolvedByCaller []callerRange
	testRoots          []functionID
}

// BuildGraph scans and analyzes a complete Go repository snapshot.
func BuildGraph(repository string, tags []string) (*Graph, error) {
	snapshot, err := analyzeRepository(repository, tags)
	if err != nil {
		return nil, err
	}
	graph, err := graphFromFacts(snapshot.facts, snapshot.sourceHash, snapshot.fileCount)
	if err != nil {
		return nil, err
	}
	if err := snapshot.verifyUnchanged(); err != nil {
		return nil, err
	}
	return graph, nil
}

type analyzedRepository struct {
	facts      sourceFacts
	sourceHash string
	fileCount  int
	root       string
	sources    map[string][]byte
}

func analyzeRepository(repository string, tags []string) (*analyzedRepository, error) {
	root, err := filepath.Abs(repository)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository symlinks: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("repository directory does not exist: %s", root)
	}
	if err := rejectUnsupportedGOFLAGS(); err != nil {
		return nil, err
	}
	tags = normalizedTags(tags)
	sources, err := collectSources(root)
	if err != nil {
		return nil, err
	}
	encoded, err := gosource.Analyze(sources, root, tags)
	if err != nil {
		return nil, fmt.Errorf("Go parser failed: %w", err)
	}
	var facts sourceFacts
	if err := json.Unmarshal(encoded, &facts); err != nil {
		return nil, fmt.Errorf("decode Go source facts: %w", err)
	}
	facts.BuildContext.AnalyzerSHA256, err = analyzerFingerprint()
	if err != nil {
		return nil, err
	}
	return &analyzedRepository{
		facts: facts, sourceHash: fingerprint(sources), fileCount: lenGoFiles(sources),
		root: root, sources: sources,
	}, nil
}

func (snapshot *analyzedRepository) verifyUnchanged() error {
	current, err := collectSources(snapshot.root)
	if err != nil {
		return err
	}
	if !sameSources(snapshot.sources, current) {
		return fmt.Errorf("repository sources changed during the rebuild; retry")
	}
	return nil
}

// RebuildToFile replaces the graph only after a complete successful rebuild.
func RebuildToFile(repository, output string, tags []string) (*Graph, error) {
	graph, err := BuildGraph(repository, tags)
	if err != nil {
		return nil, err
	}
	if err := atomicWriteStream(output, strings.HasSuffix(output, ".gz"), func(w io.Writer) error {
		return writeGraphJSON(w, graph)
	}); err != nil {
		return nil, err
	}
	return graph, nil
}

// writeGraphJSON writes one test at a time. It preserves MarshalIndent's v2
// bytes without allocating a second, repository-sized JSON buffer.
func writeGraphJSON(writer io.Writer, graph *Graph) error {
	fields := []struct {
		name  string
		value any
	}{
		{"format_version", graph.FormatVersion}, {"language", graph.Language},
		{"source_fingerprint", graph.SourceFingerprint}, {"analysis_context", graph.AnalysisContext},
		{"analysis_fingerprint", graph.AnalysisFingerprint}, {"file_count", graph.FileCount},
		{"files", graph.Files}, {"nodes", graph.Nodes}, {"edges", graph.Edges},
		{"unresolved_calls", graph.UnresolvedCalls},
	}
	// Refuse to silently drop a newly added Graph field from the v2 writer.
	if reflect.TypeOf(*graph).NumField() != len(fields)+1 { // plus Tests
		return fmt.Errorf("graph-v2 writer must account for every Graph field")
	}
	if _, err := io.WriteString(writer, "{\n"); err != nil {
		return err
	}
	for _, field := range fields {
		data, err := json.MarshalIndent(field.value, "  ", "  ")
		if err != nil {
			return fmt.Errorf("encode graph %s: %w", field.name, err)
		}
		if _, err := fmt.Fprintf(writer, "  %q: %s,\n", field.name, data); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(writer, "  \"tests\": {"); err != nil {
		return err
	}
	roots := make([]string, 0, len(graph.Tests))
	for root := range graph.Tests {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for i, root := range roots {
		key, err := json.Marshal(root)
		if err != nil {
			return err
		}
		value, err := json.MarshalIndent(graph.Tests[root], "    ", "  ")
		if err != nil {
			return fmt.Errorf("encode test graph %q: %w", root, err)
		}
		separator := "\n"
		if i > 0 {
			separator = ",\n"
		}
		if _, err := fmt.Fprintf(writer, "%s    %s: %s", separator, key, value); err != nil {
			return err
		}
	}
	if len(roots) > 0 {
		_, err := io.WriteString(writer, "\n  }\n}\n")
		return err
	}
	_, err := io.WriteString(writer, "}\n}\n")
	return err
}

// ReadGraph accepts v2 graphs with additional fields for forward compatibility.
func ReadGraph(filename string) (*Graph, error) {
	data, err := readMaybeGzip(filename)
	if err != nil {
		return nil, fmt.Errorf("read graph %s: %w", filename, err)
	}
	var graph Graph
	if err := json.Unmarshal(data, &graph); err != nil {
		return nil, fmt.Errorf("parse graph %s: %w", filename, err)
	}
	if graph.FormatVersion != graphFormatVersion || graph.Language != "go" {
		return nil, fmt.Errorf("unsupported graph format or language: format_version=%d language=%q", graph.FormatVersion, graph.Language)
	}
	return &graph, nil
}

func graphFromFacts(facts sourceFacts, sourceHash string, fileCount int) (*Graph, error) {
	prepared, err := prepareGraph(facts, sourceHash, fileCount)
	if err != nil {
		return nil, err
	}
	compact := prepared.compact
	visited := make([]uint32, len(compact.Nodes))
	var epoch uint32
	pending := make([]functionID, 0)
	reached := make([]functionID, 0)
	tests := make(map[string]TestGraph, len(prepared.testRoots))
	for _, root := range prepared.testRoots {
		epoch = nextEpoch(visited, epoch)
		reached, pending = prepared.reachable(root, visited, epoch, reached, pending)
		sort.Slice(reached, func(i, j int) bool { return reached[i] < reached[j] })
		viewNodes := make([]string, 0, len(reached))
		viewEdges := make([]TestGraphEdge, 0)
		viewUnresolved := make([]UnresolvedCall, 0)
		for _, caller := range reached {
			name := prepared.names[caller]
			viewNodes = append(viewNodes, name)
			for _, callee := range prepared.adjacency[caller] {
				viewEdges = append(viewEdges, TestGraphEdge{Caller: name, Callee: prepared.names[callee]})
			}
			group := prepared.unresolvedByCaller[caller]
			for _, call := range compact.Unresolved[group.start:group.end] {
				viewUnresolved = append(viewUnresolved, UnresolvedCall{
					Caller: name, Expression: call.Expression, Path: call.Path, Line: call.Line,
				})
			}
		}
		tests[prepared.names[root]] = TestGraph{Nodes: viewNodes, Edges: viewEdges, UnresolvedCalls: viewUnresolved}
	}
	edges := make([]GraphEdge, 0, len(compact.Edges))
	for _, edge := range compact.Edges {
		edges = append(edges, GraphEdge{
			Caller: prepared.names[edge.Caller], Callee: prepared.names[edge.Callee], Callsites: edge.Callsites,
		})
	}
	unresolved := make([]UnresolvedCall, 0, len(compact.Unresolved))
	for _, call := range compact.Unresolved {
		unresolved = append(unresolved, UnresolvedCall{
			Caller: prepared.names[call.Caller], Expression: call.Expression, Path: call.Path, Line: call.Line,
		})
	}
	return &Graph{
		FormatVersion: graphFormatVersion, Language: compact.Language, SourceFingerprint: compact.SourceFingerprint,
		AnalysisContext: compact.AnalysisContext, AnalysisFingerprint: compact.AnalysisFingerprint,
		FileCount: compact.FileCount, Files: compact.Files, Nodes: compact.Nodes, Edges: edges,
		UnresolvedCalls: unresolved, Tests: tests,
	}, nil
}

func nextEpoch(visited []uint32, epoch uint32) uint32 {
	epoch++
	if epoch == 0 {
		clear(visited)
		epoch++
	}
	return epoch
}

func (graph *preparedGraph) reachable(root functionID, visited []uint32, epoch uint32, reached, pending []functionID) ([]functionID, []functionID) {
	visited[root] = epoch
	pending = append(pending[:0], root)
	reached = reached[:0]
	for head := 0; head < len(pending); head++ {
		caller := pending[head]
		reached = append(reached, caller)
		for _, callee := range graph.adjacency[caller] {
			if visited[callee] != epoch {
				visited[callee] = epoch
				pending = append(pending, callee)
			}
		}
	}
	return reached, pending
}

func prepareGraph(facts sourceFacts, sourceHash string, fileCount int) (*preparedGraph, error) {
	if uint64(len(facts.Nodes)) > uint64(^functionID(0)) {
		return nil, fmt.Errorf("too many graph nodes for numeric function IDs")
	}
	orderedNodes := append([]GraphNode{}, facts.Nodes...)
	sort.Slice(orderedNodes, func(i, j int) bool { return orderedNodes[i].ID < orderedNodes[j].ID })
	ids := make(map[string]functionID, len(orderedNodes))
	names := make([]string, len(orderedNodes))
	testRoots := make([]functionID, 0)
	for i, node := range orderedNodes {
		if i > 0 && orderedNodes[i-1].ID == node.ID {
			return nil, fmt.Errorf("duplicate graph node: %s", node.ID)
		}
		id := functionID(i)
		ids[node.ID] = id
		names[id] = node.ID
		if node.IsTest {
			testRoots = append(testRoots, id)
		}
	}
	callSites := make(map[numericCallPair][]CallSite)
	for _, edge := range facts.Edges {
		caller, ok := ids[edge.Caller]
		if !ok {
			return nil, fmt.Errorf("call edge references a missing graph node: %s", edge.Caller)
		}
		callee, ok := ids[edge.Callee]
		if !ok {
			return nil, fmt.Errorf("call edge references a missing graph node: %s", edge.Callee)
		}
		pair := numericCallPair{caller, callee}
		callSites[pair] = append(callSites[pair], CallSite{Path: edge.Path, Line: edge.Line})
	}
	pairs := make([]numericCallPair, 0, len(callSites))
	for pair := range callSites {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].caller == pairs[j].caller {
			return pairs[i].callee < pairs[j].callee
		}
		return pairs[i].caller < pairs[j].caller
	})
	adjacency := make([][]functionID, len(orderedNodes))
	edges := make([]CompactEdge, 0, len(pairs))
	for _, pair := range pairs {
		adjacency[pair.caller] = append(adjacency[pair.caller], pair.callee)
		locations := callSites[pair]
		sort.Slice(locations, func(i, j int) bool {
			if locations[i].Path == locations[j].Path {
				return locations[i].Line < locations[j].Line
			}
			return locations[i].Path < locations[j].Path
		})
		edges = append(edges, CompactEdge{Caller: uint32(pair.caller), Callee: uint32(pair.callee), Callsites: locations})
	}
	unresolved := append([]UnresolvedCall{}, facts.UnresolvedCalls...)
	for _, call := range unresolved {
		if _, ok := ids[call.Caller]; !ok {
			return nil, fmt.Errorf("unresolved call references a missing graph node: %s", call.Caller)
		}
	}
	sort.Slice(unresolved, func(i, j int) bool {
		a, b := unresolved[i], unresolved[j]
		if a.Caller != b.Caller {
			return a.Caller < b.Caller
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Expression < b.Expression
	})
	// Unresolved calls are sorted by caller, so each caller owns one range.
	unresolvedByCaller := make([]callerRange, len(orderedNodes))
	for i, call := range unresolved {
		group := &unresolvedByCaller[ids[call.Caller]]
		if group.end == 0 {
			group.start = i
		}
		group.end = i + 1
	}
	files := append([]FileMetadata{}, facts.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	numericUnresolved := make([]CompactCall, 0, len(unresolved))
	for _, call := range unresolved {
		numericUnresolved = append(numericUnresolved, CompactCall{
			Caller: uint32(ids[call.Caller]), Expression: call.Expression, Path: call.Path, Line: call.Line,
		})
	}
	context := facts.BuildContext
	if context.BuildTags == nil {
		context.BuildTags = []string{}
	}
	analysisJSON, err := json.Marshal([]any{sourceHash, contextMap(context)})
	if err != nil {
		return nil, fmt.Errorf("encode analysis fingerprint: %w", err)
	}
	analysisHash := sha256.Sum256(analysisJSON)
	return &preparedGraph{compact: &CompactGraph{
		FormatVersion: compactFormatVersion, Language: "go", SourceFingerprint: sourceHash,
		AnalysisContext: context, AnalysisFingerprint: hex.EncodeToString(analysisHash[:]),
		FileCount: fileCount, Files: files, Nodes: orderedNodes, Edges: edges,
		Unresolved: numericUnresolved,
	}, names: names, adjacency: adjacency, unresolvedByCaller: unresolvedByCaller, testRoots: testRoots}, nil
}

func collectSources(root string) (map[string][]byte, error) {
	sources := make(map[string][]byte)
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename != root && entry.IsDir() {
			name := entry.Name()
			if ignoredDirectories[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() != "go.mod" && !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read source %s: %w", filepath.ToSlash(relative), err)
		}
		sources[filepath.ToSlash(relative)] = content
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan repository: %w", err)
	}
	return sources, nil
}

func rejectUnsupportedGOFLAGS() error {
	command := exec.Command("go", "env", "GOFLAGS")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot determine effective GOFLAGS: %s", strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != "" {
		return fmt.Errorf("GOFLAGS are not applied by this analyzer; unset them and pass build tags with --go-tags")
	}
	return nil
}

func normalizedTags(tags []string) []string {
	unique := make(map[string]bool)
	for _, tag := range tags {
		for _, item := range strings.Split(tag, ",") {
			if tag := strings.TrimSpace(item); tag != "" {
				unique[tag] = true
			}
		}
	}
	result := make([]string, 0, len(unique))
	for tag := range unique {
		result = append(result, tag)
	}
	sort.Strings(result)
	return result
}

func fingerprint(sources map[string][]byte) string {
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	hash := sha256.New()
	var length [8]byte
	for _, path := range paths {
		binary.BigEndian.PutUint64(length[:], uint64(len([]byte(path))))
		_, _ = hash.Write(length[:])
		_, _ = io.WriteString(hash, path)
		content := sources[path]
		binary.BigEndian.PutUint64(length[:], uint64(len(content)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(content)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func analyzerFingerprint() (string, error) {
	hash := sha256.New()
	paths := []string{"graph.go", "compact.go", "planner.go", "internal/gosource/analyzer.go"}
	for _, path := range paths {
		content, err := analyzerSources.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read analyzer source %s: %w", path, err)
		}
		_, _ = io.WriteString(hash, path)
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(content)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func contextMap(context BuildContext) map[string]any {
	return map[string]any{
		"goos": context.GOOS, "goarch": context.GOARCH, "go_version": context.GoVersion,
		"compiler": context.Compiler, "cgo_enabled": context.CgoEnabled,
		"build_tags": context.BuildTags, "analyzer_sha256": context.AnalyzerSHA256,
	}
}

func lenGoFiles(sources map[string][]byte) int {
	count := 0
	for path := range sources {
		if strings.HasSuffix(path, ".go") {
			count++
		}
	}
	return count
}

func sameSources(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for path, content := range left {
		other, exists := right[path]
		if !exists || !bytes.Equal(content, other) {
			return false
		}
	}
	return true
}

func atomicWrite(filename string, data []byte, compressed bool) error {
	return atomicWriteStream(filename, compressed, func(writer io.Writer) error {
		_, err := writer.Write(data)
		return err
	})
}

func atomicWriteStream(filename string, compressed bool, write func(io.Writer) error) error {
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, filepath.Base(filename)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if compressed {
		writer := gzip.NewWriter(temporary)
		writer.Header.ModTime = time.Time{}
		writer.Header.Name = ""
		if err := write(writer); err != nil {
			_ = writer.Close()
			_ = temporary.Close()
			return fmt.Errorf("write compressed output: %w", err)
		}
		if err := writer.Close(); err != nil {
			_ = temporary.Close()
			return fmt.Errorf("finish compressed output: %w", err)
		}
	} else if err := write(temporary); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write output: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("replace output: %w", err)
	}
	return nil
}

func readMaybeGzip(filename string) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if strings.HasSuffix(filename, ".gz") {
		reader, err := gzip.NewReader(file)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	}
	return io.ReadAll(file)
}
