package hexagon

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

const (
	DiffSideBase = "base"
	DiffSideHead = "head"
	DiffSideBoth = "both"
)

// ParsedDiff is structured output from a Git diff parser. Hunk ranges are
// one-based and include any context lines retained by that parser.
type ParsedDiff struct {
	Files []DiffFile `json:"files"`
}

// DiffFile identifies old and new paths. An empty OldPath denotes an added
// file; an empty NewPath denotes a deleted file. Both paths may be set for a
// modification or rename.
type DiffFile struct {
	OldPath string     `json:"old_path,omitempty"`
	NewPath string     `json:"new_path,omitempty"`
	Hunks   []DiffHunk `json:"hunks"`
}

// DiffHunk carries the old-side and new-side ranges from a parsed Git hunk.
// A zero line count means that side has no changed lines in this hunk.
type DiffHunk struct {
	OldStart int `json:"old_start"`
	OldLines int `json:"old_lines"`
	NewStart int `json:"new_start"`
	NewLines int `json:"new_lines"`
}

// DiffMapping records which graph functions overlap one side of a diff hunk.
type DiffMapping struct {
	Path      string   `json:"path"`
	Side      string   `json:"side"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Functions []string `json:"functions"`
}

type diffSnapshot struct {
	sourceFingerprint  string
	context            BuildContext
	nodes              []GraphNode
	nodeByID           map[string]uint32
	nodesByPath        map[string][]uint32
	prefixMaxEndByPath map[string][]int
	validSpansByPath   map[string]bool
	parents            [][]uint32
	rootIDs            []string
	testRoots          map[string]bool
	packages           map[string]string
	testRootsByKey     map[string][]string
}

// PlanDiff maps parsed Git hunk ranges to function spans, traverses caller
// edges toward test roots, and returns stable, unique candidates. It keeps the
// full-suite gate because static call graphs cannot prove all dependencies.
func PlanDiff(current *Graph, diff ParsedDiff, previous *Graph) (*ImpactPlan, error) {
	currentSnapshot, err := namedDiffSnapshot(current)
	if err != nil {
		return nil, fmt.Errorf("prepare current graph: %w", err)
	}
	var previousSnapshot *diffSnapshot
	if previous != nil {
		previousSnapshot, err = namedDiffSnapshot(previous)
		if err != nil {
			return nil, fmt.Errorf("prepare previous graph: %w", err)
		}
	}
	return planParsedDiff(currentSnapshot, diff, previousSnapshot)
}

// PlanCompactDiff is the compact-snapshot counterpart to PlanDiff.
func PlanCompactDiff(current *CompactGraph, diff ParsedDiff, previous *CompactGraph) (*ImpactPlan, error) {
	currentSnapshot, err := compactDiffSnapshot(current)
	if err != nil {
		return nil, fmt.Errorf("prepare current compact graph: %w", err)
	}
	var previousSnapshot *diffSnapshot
	if previous != nil {
		previousSnapshot, err = compactDiffSnapshot(previous)
		if err != nil {
			return nil, fmt.Errorf("prepare previous compact graph: %w", err)
		}
	}
	return planParsedDiff(currentSnapshot, diff, previousSnapshot)
}

// PlanGraphDiffFiles plans from JSON graph-v2 or compact snapshots. The two
// paths may use different formats, which is useful when a PR changes graph
// representation between its base and head revisions.
func PlanGraphDiffFiles(currentFile string, diff ParsedDiff, previousFile string) (*ImpactPlan, error) {
	current, err := readDiffSnapshot(currentFile)
	if err != nil {
		return nil, fmt.Errorf("read current graph: %w", err)
	}
	var previous *diffSnapshot
	if previousFile != "" {
		previous, err = readDiffSnapshot(previousFile)
		if err != nil {
			return nil, fmt.Errorf("read previous graph: %w", err)
		}
	}
	return planParsedDiff(current, diff, previous)
}

func readDiffSnapshot(filename string) (*diffSnapshot, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 3)
	_, readErr := io.ReadFull(file, header)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if string(header) == "HGC" {
		graph, err := ReadCompactGraph(filename)
		if err != nil {
			return nil, err
		}
		return compactDiffSnapshot(graph)
	}
	graph, err := ReadGraph(filename)
	if err != nil {
		return nil, err
	}
	return namedDiffSnapshot(graph)
}

func namedDiffSnapshot(graph *Graph) (*diffSnapshot, error) {
	if graph == nil || graph.FormatVersion != graphFormatVersion || graph.Language != "go" {
		return nil, fmt.Errorf("graph must be a supported Go format v2 snapshot")
	}
	parents := make([][]uint32, len(graph.Nodes))
	nodeByID := make(map[string]uint32, len(graph.Nodes))
	for i, node := range graph.Nodes {
		if node.ID == "" {
			return nil, fmt.Errorf("graph contains a function with an empty ID")
		}
		if _, exists := nodeByID[node.ID]; exists {
			return nil, fmt.Errorf("graph contains duplicate function ID %q", node.ID)
		}
		nodeByID[node.ID] = uint32(i)
	}
	for _, edge := range graph.Edges {
		caller, callerOK := nodeByID[edge.Caller]
		callee, calleeOK := nodeByID[edge.Callee]
		if !callerOK || !calleeOK {
			return nil, fmt.Errorf("call edge references a missing graph node: %q -> %q", edge.Caller, edge.Callee)
		}
		parents[callee] = append(parents[callee], caller)
	}
	rootIDs := make([]string, 0, len(graph.Tests))
	for root := range graph.Tests {
		rootIDs = append(rootIDs, root)
	}
	sort.Strings(rootIDs)
	return newDiffSnapshot(graph.SourceFingerprint, graph.AnalysisContext, graph.Nodes, graph.Files, rootIDs, parents)
}

func compactDiffSnapshot(graph *CompactGraph) (*diffSnapshot, error) {
	if err := graph.validate(); err != nil {
		return nil, err
	}
	parents := make([][]uint32, len(graph.Nodes))
	for _, edge := range graph.Edges {
		parents[edge.Callee] = append(parents[edge.Callee], edge.Caller)
	}
	rootIDs := make([]string, 0, len(graph.Tests))
	for _, test := range graph.Tests {
		rootIDs = append(rootIDs, graph.Nodes[test.Root].ID)
	}
	sort.Strings(rootIDs)
	return newDiffSnapshot(graph.SourceFingerprint, graph.AnalysisContext, graph.Nodes, graph.Files, rootIDs, parents)
}

func newDiffSnapshot(sourceFingerprint string, context BuildContext, nodes []GraphNode, files []FileMetadata, rootIDs []string, parents [][]uint32) (*diffSnapshot, error) {
	if len(parents) != len(nodes) {
		return nil, fmt.Errorf("caller adjacency does not match graph node count")
	}
	snapshot := &diffSnapshot{
		sourceFingerprint: sourceFingerprint, context: context, nodes: nodes,
		nodeByID: make(map[string]uint32, len(nodes)), nodesByPath: make(map[string][]uint32),
		prefixMaxEndByPath: make(map[string][]int), validSpansByPath: make(map[string]bool),
		parents: parents, rootIDs: rootIDs, testRoots: make(map[string]bool, len(rootIDs)),
		packages: make(map[string]string, len(files)), testRootsByKey: make(map[string][]string, len(rootIDs)),
	}
	for _, file := range files {
		if file.Path != "" {
			snapshot.packages[normalizeGraphPath(file.Path)] = file.Package
		}
	}
	for i, node := range nodes {
		if node.ID == "" {
			return nil, fmt.Errorf("graph contains a function with an empty ID")
		}
		if _, exists := snapshot.nodeByID[node.ID]; exists {
			return nil, fmt.Errorf("graph contains duplicate function ID %q", node.ID)
		}
		snapshot.nodeByID[node.ID] = uint32(i)
		if node.Path != "" {
			path := normalizeGraphPath(node.Path)
			snapshot.nodesByPath[path] = append(snapshot.nodesByPath[path], uint32(i))
			if node.Line < 1 || node.EndLine < node.Line {
				snapshot.validSpansByPath[path] = false
			} else if _, seen := snapshot.validSpansByPath[path]; !seen {
				snapshot.validSpansByPath[path] = true
			}
		}
	}
	testNodeCount := 0
	for _, node := range nodes {
		if node.IsTest {
			testNodeCount++
		}
	}
	if testNodeCount != len(rootIDs) {
		return nil, fmt.Errorf("test inventory has %d roots for %d test nodes", len(rootIDs), testNodeCount)
	}
	for filename, nodeIndexes := range snapshot.nodesByPath {
		sort.Slice(nodeIndexes, func(i, j int) bool {
			left, right := nodes[nodeIndexes[i]], nodes[nodeIndexes[j]]
			if left.Line != right.Line {
				return left.Line < right.Line
			}
			if left.EndLine != right.EndLine {
				return left.EndLine < right.EndLine
			}
			return left.ID < right.ID
		})
		prefixMaxEnd := make([]int, len(nodeIndexes))
		maxEnd := 0
		for i, index := range nodeIndexes {
			if nodes[index].EndLine > maxEnd {
				maxEnd = nodes[index].EndLine
			}
			prefixMaxEnd[i] = maxEnd
		}
		snapshot.prefixMaxEndByPath[filename] = prefixMaxEnd
	}
	for _, nodeID := range rootIDs {
		index, exists := snapshot.nodeByID[nodeID]
		if !exists || !nodes[index].IsTest {
			return nil, fmt.Errorf("test inventory references a missing or non-test node %q", nodeID)
		}
		snapshot.testRoots[nodeID] = true
		if key := snapshot.testKey(nodes[index]); key != "" {
			snapshot.testRootsByKey[key] = append(snapshot.testRootsByKey[key], nodeID)
		}
	}
	for callee, callerIDs := range snapshot.parents {
		sort.Slice(callerIDs, func(i, j int) bool {
			return nodes[callerIDs[i]].ID < nodes[callerIDs[j]].ID
		})
		snapshot.parents[callee] = callerIDs
	}
	return snapshot, nil
}

func (snapshot *diffSnapshot) testKey(node GraphNode) string {
	directory := path.Dir(normalizeGraphPath(node.Path))
	packageName := snapshot.packages[normalizeGraphPath(node.Path)]
	if packageName == "" || node.Qualname == "" {
		return ""
	}
	return directory + "::" + packageName + "::" + node.Qualname
}

func planParsedDiff(current *diffSnapshot, diff ParsedDiff, previous *diffSnapshot) (*ImpactPlan, error) {
	if len(diff.Files) == 0 {
		return nil, fmt.Errorf("parsed diff must contain at least one changed file")
	}
	baseSeeds := make(map[string]bool)
	headSeeds := make(map[string]bool)
	changedFiles := make(map[string]bool)
	changedFunctions := make(map[string]bool)
	impactFunctions := make(map[string]bool)
	candidateTests := make(map[string]bool)
	mappings := make([]DiffMapping, 0)
	complete := true
	seenRange := false

	for _, file := range diff.Files {
		oldPath, err := normalizeOptionalDiffPath(file.OldPath)
		if err != nil {
			return nil, fmt.Errorf("invalid old diff path %q: %w", file.OldPath, err)
		}
		newPath, err := normalizeOptionalDiffPath(file.NewPath)
		if err != nil {
			return nil, fmt.Errorf("invalid new diff path %q: %w", file.NewPath, err)
		}
		if oldPath == "" && newPath == "" {
			return nil, fmt.Errorf("each diff file must identify an old path, a new path, or both")
		}
		if oldPath != "" {
			changedFiles[oldPath] = true
		}
		if newPath != "" {
			changedFiles[newPath] = true
		}
		if len(file.Hunks) == 0 {
			complete = false
			mappings = append(mappings, DiffMapping{Path: chooseDiffPath(oldPath, newPath), Side: DiffSideBoth, Functions: []string{}})
			continue
		}
		for _, hunk := range file.Hunks {
			if hunk.OldLines < 0 || hunk.NewLines < 0 {
				return nil, fmt.Errorf("diff hunk line counts cannot be negative")
			}
			if oldPath == "" && hunk.OldLines > 0 {
				return nil, fmt.Errorf("old diff range has no old path")
			}
			if newPath == "" && hunk.NewLines > 0 {
				return nil, fmt.Errorf("new diff range has no new path")
			}
			if hunk.OldLines > 0 {
				start, end, err := diffLineRange(hunk.OldStart, hunk.OldLines)
				if err != nil {
					return nil, fmt.Errorf("invalid old range for %s: %w", oldPath, err)
				}
				seenRange = true
				mapping, functions, mapped := mapDiffRange(previous, oldPath, DiffSideBase, start, end)
				mappings = append(mappings, mapping)
				if !mapped {
					complete = false
				}
				for _, function := range functions {
					baseSeeds[function] = true
					changedFunctions[function] = true
				}
			}
			if hunk.NewLines > 0 {
				start, end, err := diffLineRange(hunk.NewStart, hunk.NewLines)
				if err != nil {
					return nil, fmt.Errorf("invalid new range for %s: %w", newPath, err)
				}
				seenRange = true
				mapping, functions, mapped := mapDiffRange(current, newPath, DiffSideHead, start, end)
				mappings = append(mappings, mapping)
				if !mapped {
					complete = false
				}
				for _, function := range functions {
					headSeeds[function] = true
					changedFunctions[function] = true
				}
			}
			if hunk.OldLines == 0 && hunk.NewLines == 0 {
				complete = false
				mappings = append(mappings, DiffMapping{Path: chooseDiffPath(oldPath, newPath), Side: DiffSideBoth, Functions: []string{}})
			}
		}
	}
	if !seenRange {
		complete = false
	}

	if functions, roots, ok := collectDiffAncestors(current, headSeeds); ok {
		for _, function := range functions {
			impactFunctions[function] = true
		}
		for _, root := range roots {
			candidateTests[root] = true
		}
	} else {
		complete = false
	}
	if len(baseSeeds) > 0 {
		functions, roots, ok := collectDiffAncestors(previous, baseSeeds)
		if !ok {
			complete = false
		} else {
			for _, function := range functions {
				impactFunctions[function] = true
			}
			for _, baseRoot := range roots {
				matches := currentRootsForPrevious(current, previous, baseRoot)
				if len(matches) == 0 {
					complete = false
					continue
				}
				for _, root := range matches {
					candidateTests[root] = true
				}
			}
		}
	}
	if !complete {
		for _, root := range current.rootIDs {
			candidateTests[root] = true
		}
	}

	changedFileList := sortedBoolKeys(changedFiles)
	changedFunctionList := sortedBoolKeys(changedFunctions)
	impactFunctionList := sortedBoolKeys(impactFunctions)
	candidateTestList := sortedBoolKeys(candidateTests)
	mappings = normalizeDiffMappings(mappings)
	reason := fullSuiteReason
	if !complete {
		reason = "one or more diff ranges could not be mapped completely; " + fullSuiteReason
	}
	return &ImpactPlan{
		FormatVersion: planFormatVersion, Language: "go", SourceFingerprint: current.sourceFingerprint,
		ChangedFiles: changedFileList, CandidateTests: candidateTestList,
		ChangedFunctions: changedFunctionList, ImpactFunctions: impactFunctionList, DiffMappings: mappings,
		BuildTags: append([]string{}, current.context.BuildTags...), AnalysisContext: current.context,
		RunFullSuite: true, Reason: reason,
	}, nil
}

func mapDiffRange(snapshot *diffSnapshot, filename, side string, start, end int) (DiffMapping, []string, bool) {
	mapping := DiffMapping{Path: filename, Side: side, StartLine: start, EndLine: end, Functions: []string{}}
	if snapshot == nil {
		return mapping, nil, false
	}
	indexes := snapshot.nodesByPath[filename]
	if len(indexes) == 0 {
		return mapping, nil, false
	}
	prefixMaxEnd := snapshot.prefixMaxEndByPath[filename]
	first := sort.Search(len(indexes), func(i int) bool { return prefixMaxEnd[i] >= start })
	limit := sort.Search(len(indexes), func(i int) bool { return snapshot.nodes[indexes[i]].Line > end })
	functions := make([]string, 0, 1)
	for i := first; i < limit; i++ {
		index := indexes[i]
		node := snapshot.nodes[index]
		if node.Line <= end && node.EndLine >= start {
			functions = append(functions, node.ID)
		}
	}
	sort.Strings(functions)
	functions = uniqueStrings(functions)
	mapping.Functions = functions
	return mapping, functions, snapshot.validSpansByPath[filename] && len(functions) > 0
}

func collectDiffAncestors(snapshot *diffSnapshot, seeds map[string]bool) ([]string, []string, bool) {
	if len(seeds) == 0 {
		return nil, nil, true
	}
	if snapshot == nil {
		return nil, nil, false
	}
	visited := make([]bool, len(snapshot.nodes))
	queue := make([]uint32, 0, len(seeds))
	for seed := range seeds {
		index, exists := snapshot.nodeByID[seed]
		if !exists {
			return nil, nil, false
		}
		if !visited[index] {
			visited[index] = true
			queue = append(queue, index)
		}
	}
	roots := make([]string, 0)
	functions := make([]string, 0)
	for cursor := 0; cursor < len(queue); cursor++ {
		index := queue[cursor]
		node := snapshot.nodes[index]
		functions = append(functions, node.ID)
		if snapshot.testRoots[node.ID] {
			roots = append(roots, node.ID)
		}
		for _, caller := range snapshot.parents[index] {
			if !visited[caller] {
				visited[caller] = true
				queue = append(queue, caller)
			}
		}
	}
	sort.Strings(functions)
	functions = uniqueStrings(functions)
	sort.Strings(roots)
	roots = uniqueStrings(roots)
	return functions, roots, true
}

func currentRootsForPrevious(current, previous *diffSnapshot, previousRoot string) []string {
	if current.testRoots[previousRoot] {
		return []string{previousRoot}
	}
	index, exists := previous.nodeByID[previousRoot]
	if !exists {
		return nil
	}
	key := previous.testKey(previous.nodes[index])
	if key == "" {
		return nil
	}
	return current.testRootsByKey[key]
}

func diffLineRange(start, count int) (int, int, error) {
	if count < 1 || start < 1 {
		return 0, 0, fmt.Errorf("a non-empty line range requires a positive start and count")
	}
	maxInt := int(^uint(0) >> 1)
	if start > maxInt-count+1 {
		return 0, 0, fmt.Errorf("line range exceeds the supported line-number range")
	}
	return start, start + count - 1, nil
}

func normalizeOptionalDiffPath(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	filename := path.Clean(strings.ReplaceAll(raw, "\\", "/"))
	if filename == "." || path.IsAbs(filename) || filename == ".." || strings.HasPrefix(filename, "../") {
		return "", fmt.Errorf("path must be repository-relative")
	}
	return filename, nil
}

func normalizeGraphPath(raw string) string {
	return path.Clean(strings.ReplaceAll(raw, "\\", "/"))
}

func chooseDiffPath(oldPath, newPath string) string {
	if newPath != "" {
		return newPath
	}
	return oldPath
}

func sortedBoolKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizeDiffMappings(mappings []DiffMapping) []DiffMapping {
	sort.Slice(mappings, func(i, j int) bool {
		left, right := mappings[i], mappings[j]
		if left.Side != right.Side {
			return left.Side < right.Side
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.StartLine != right.StartLine {
			return left.StartLine < right.StartLine
		}
		return left.EndLine < right.EndLine
	})
	if len(mappings) < 2 {
		return mappings
	}
	result := mappings[:1]
	for _, mapping := range mappings[1:] {
		last := &result[len(result)-1]
		if last.Side == mapping.Side && last.Path == mapping.Path && last.StartLine == mapping.StartLine && last.EndLine == mapping.EndLine {
			last.Functions = append(last.Functions, mapping.Functions...)
			sort.Strings(last.Functions)
			last.Functions = uniqueStrings(last.Functions)
			continue
		}
		mapping.Functions = append([]string{}, mapping.Functions...)
		result = append(result, mapping)
	}
	return result
}
