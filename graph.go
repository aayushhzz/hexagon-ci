package main

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
	"sort"
	"strings"
	"time"

	"example.com/hexagon-ci/internal/gosource"
)

//go:embed graph.go planner.go internal/gosource/analyzer.go
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

type callPair struct{ caller, callee string }

// BuildGraph scans and analyzes a complete Go repository snapshot.
func BuildGraph(repository string, tags []string) (*Graph, error) {
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
	graph, err := graphFromFacts(facts, fingerprint(sources), lenGoFiles(sources))
	if err != nil {
		return nil, err
	}
	current, err := collectSources(root)
	if err != nil {
		return nil, err
	}
	if !sameSources(sources, current) {
		return nil, fmt.Errorf("repository sources changed during the rebuild; retry")
	}
	return graph, nil
}

// RebuildToFile replaces the graph only after a complete successful rebuild.
func RebuildToFile(repository, output string, tags []string) (*Graph, error) {
	graph, err := BuildGraph(repository, tags)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(graph, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode graph: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWrite(output, data, strings.HasSuffix(output, ".gz")); err != nil {
		return nil, err
	}
	return graph, nil
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
	nodes := make(map[string]GraphNode, len(facts.Nodes))
	for _, node := range facts.Nodes {
		if _, exists := nodes[node.ID]; exists {
			return nil, fmt.Errorf("duplicate graph node: %s", node.ID)
		}
		nodes[node.ID] = node
	}
	callSites := make(map[callPair][]CallSite)
	adjacency := make(map[string]map[string]bool)
	for _, edge := range facts.Edges {
		if _, ok := nodes[edge.Caller]; !ok {
			return nil, fmt.Errorf("call edge references a missing graph node: %s", edge.Caller)
		}
		if _, ok := nodes[edge.Callee]; !ok {
			return nil, fmt.Errorf("call edge references a missing graph node: %s", edge.Callee)
		}
		pair := callPair{edge.Caller, edge.Callee}
		callSites[pair] = append(callSites[pair], CallSite{Path: edge.Path, Line: edge.Line})
		if adjacency[edge.Caller] == nil {
			adjacency[edge.Caller] = make(map[string]bool)
		}
		adjacency[edge.Caller][edge.Callee] = true
	}
	pairs := make([]callPair, 0, len(callSites))
	for pair := range callSites {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].caller == pairs[j].caller {
			return pairs[i].callee < pairs[j].callee
		}
		return pairs[i].caller < pairs[j].caller
	})
	edges := make([]GraphEdge, 0, len(pairs))
	for _, pair := range pairs {
		locations := callSites[pair]
		sort.Slice(locations, func(i, j int) bool {
			if locations[i].Path == locations[j].Path {
				return locations[i].Line < locations[j].Line
			}
			return locations[i].Path < locations[j].Path
		})
		edges = append(edges, GraphEdge{Caller: pair.caller, Callee: pair.callee, Callsites: locations})
	}
	unresolved := append([]UnresolvedCall{}, facts.UnresolvedCalls...)
	for _, call := range unresolved {
		if _, ok := nodes[call.Caller]; !ok {
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
	files := append([]FileMetadata{}, facts.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	nodeIDs := make([]string, 0, len(nodes))
	for id := range nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	orderedNodes := make([]GraphNode, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		orderedNodes = append(orderedNodes, nodes[id])
	}
	testRoots := make([]string, 0)
	for id, node := range nodes {
		if node.IsTest {
			testRoots = append(testRoots, id)
		}
	}
	sort.Strings(testRoots)
	tests := make(map[string]TestGraph, len(testRoots))
	for _, root := range testRoots {
		visited := map[string]bool{root: true}
		pending := []string{root}
		reached := make(map[callPair]bool)
		for len(pending) > 0 {
			caller := pending[0]
			pending = pending[1:]
			callees := make([]string, 0, len(adjacency[caller]))
			for callee := range adjacency[caller] {
				callees = append(callees, callee)
			}
			sort.Strings(callees)
			for _, callee := range callees {
				reached[callPair{caller, callee}] = true
				if !visited[callee] {
					visited[callee] = true
					pending = append(pending, callee)
				}
			}
		}
		viewNodes := make([]string, 0, len(visited))
		for id := range visited {
			viewNodes = append(viewNodes, id)
		}
		sort.Strings(viewNodes)
		viewPairs := make([]callPair, 0, len(reached))
		for pair := range reached {
			viewPairs = append(viewPairs, pair)
		}
		sort.Slice(viewPairs, func(i, j int) bool {
			if viewPairs[i].caller == viewPairs[j].caller {
				return viewPairs[i].callee < viewPairs[j].callee
			}
			return viewPairs[i].caller < viewPairs[j].caller
		})
		viewEdges := make([]TestGraphEdge, 0, len(viewPairs))
		for _, pair := range viewPairs {
			viewEdges = append(viewEdges, TestGraphEdge{Caller: pair.caller, Callee: pair.callee})
		}
		viewUnresolved := make([]UnresolvedCall, 0)
		for _, call := range unresolved {
			if visited[call.Caller] {
				viewUnresolved = append(viewUnresolved, call)
			}
		}
		tests[root] = TestGraph{Nodes: viewNodes, Edges: viewEdges, UnresolvedCalls: viewUnresolved}
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
	return &Graph{
		FormatVersion: graphFormatVersion, Language: "go", SourceFingerprint: sourceHash,
		AnalysisContext: context, AnalysisFingerprint: hex.EncodeToString(analysisHash[:]),
		FileCount: fileCount, Files: files, Nodes: orderedNodes, Edges: edges,
		UnresolvedCalls: unresolved, Tests: tests,
	}, nil
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
	paths := []string{"graph.go", "planner.go", "internal/gosource/analyzer.go"}
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
		if _, err := writer.Write(data); err != nil {
			_ = writer.Close()
			_ = temporary.Close()
			return fmt.Errorf("compress output: %w", err)
		}
		if err := writer.Close(); err != nil {
			_ = temporary.Close()
			return fmt.Errorf("finish compressed output: %w", err)
		}
	} else if _, err := temporary.Write(data); err != nil {
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
