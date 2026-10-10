package hexagon

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// LineRange is a 1-based, inclusive range of changed source lines.
type LineRange struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
}

// FileDiff describes changed lines on the old and new sides of a Git diff.
type FileDiff struct {
	OldPath   string      `json:"old_path,omitempty"`
	NewPath   string      `json:"new_path,omitempty"`
	OldRanges []LineRange `json:"old_ranges,omitempty"`
	NewRanges []LineRange `json:"new_ranges,omitempty"`
}

// GitDiff contains file paths and changed line ranges extracted from a Git diff.
type GitDiff struct {
	Files []FileDiff `json:"files"`
}

// FunctionDiffPlan contains the unique changed functions, their caller chain,
// runnable test roots, and the union of functions selected by the diff.
type FunctionDiffPlan struct {
	ChangedFunctions  []string `json:"changed_functions"`
	ParentFunctions   []string `json:"parent_functions"`
	TestRoots         []string `json:"test_roots"`
	FunctionsToRun    []string `json:"functions_to_run"`
	SelectionComplete bool     `json:"selection_complete"`
	RunFullSuite      bool     `json:"run_full_suite"`
	Reason            string   `json:"reason,omitempty"`
}

// PlanGitDiff maps structured changed lines to functions, then walks caller
// edges to find unique test roots and the full connected function trace.
func PlanGitDiff(current *Graph, diff GitDiff, previous *Graph) (*FunctionDiffPlan, error) {
	if current == nil || current.FormatVersion != graphFormatVersion || current.Language != "go" {
		return nil, fmt.Errorf("current graph must be a supported Go format v2 graph")
	}
	currentSnapshot := snapshotFromNamed(current)
	var previousSnapshot *planningSnapshot
	if previous != nil {
		if previous.FormatVersion != graphFormatVersion || previous.Language != "go" {
			return nil, fmt.Errorf("previous graph must be a supported Go format v2 graph")
		}
		previousSnapshot = snapshotFromNamed(previous)
	}
	return planFunctionDiff(currentSnapshot, diff, previousSnapshot)
}

// PlanGitDiffCompact plans from compact snapshots without expanding test views.
func PlanGitDiffCompact(current *CompactGraph, diff GitDiff, previous *CompactGraph) (*FunctionDiffPlan, error) {
	currentSnapshot, err := snapshotFromCompact(current)
	if err != nil {
		return nil, fmt.Errorf("read current compact graph: %w", err)
	}
	var previousSnapshot *planningSnapshot
	if previous != nil {
		previousSnapshot, err = snapshotFromCompact(previous)
		if err != nil {
			return nil, fmt.Errorf("read previous compact graph: %w", err)
		}
	}
	return planFunctionDiff(currentSnapshot, diff, previousSnapshot)
}

func planFunctionDiff(current *planningSnapshot, diff GitDiff, previous *planningSnapshot) (*FunctionDiffPlan, error) {
	if err := validateFunctionDiff(diff); err != nil {
		return nil, err
	}

	currentIndex := newFunctionGraphIndex(current)
	var previousIndex *functionGraphIndex
	if previous != nil {
		previousIndex = newFunctionGraphIndex(previous)
	}
	reasons := make(map[string]bool)
	addReason := func(reason string) {
		if reason != "" {
			reasons[reason] = true
		}
	}
	for _, reason := range currentIndex.issues {
		addReason(reason)
	}
	if previousIndex != nil {
		for _, reason := range previousIndex.issues {
			addReason(reason)
		}
		if !reflect.DeepEqual(current.context, previous.context) {
			addReason("base and head graphs use different Go build contexts")
		}
	}

	changed := make(map[string]bool)
	for _, file := range diff.Files {
		if len(file.OldRanges) == 0 && len(file.NewRanges) == 0 {
			addReason("a changed file has no line ranges to map")
		}
		if len(file.OldRanges) > 0 {
			if previousIndex == nil {
				addReason("base graph is required to map old-side changed lines")
			} else {
				mapChangedRanges(previousIndex, file.OldPath, file.OldRanges, changed, addReason)
			}
		}
		if len(file.NewRanges) > 0 {
			mapChangedRanges(currentIndex, file.NewPath, file.NewRanges, changed, addReason)
		}
	}
	if len(changed) == 0 {
		addReason("no changed source lines mapped to a function")
	}

	currentRoots := make(map[string]bool, len(currentIndex.roots))
	for _, root := range currentIndex.roots {
		currentRoots[root] = true
	}
	changedFunctions := sortedSet(changed)
	parentFunctions := make(map[string]bool)
	testRoots := make(map[string]bool)
	visited := make(map[string]bool)
	queue := append([]string(nil), changedFunctions...)
	for _, id := range changedFunctions {
		visited[id] = true
		if currentRoots[id] {
			testRoots[id] = true
		}
	}
	for head := 0; head < len(queue); head++ {
		function := queue[head]
		callers := make(map[string]bool)
		for _, index := range []*functionGraphIndex{currentIndex, previousIndex} {
			if index == nil {
				continue
			}
			for _, caller := range index.callers[function] {
				callers[caller] = true
			}
		}
		for _, caller := range sortedSet(callers) {
			if currentRoots[caller] {
				testRoots[caller] = true
			} else if !changed[caller] {
				parentFunctions[caller] = true
			}
			if !visited[caller] {
				visited[caller] = true
				queue = append(queue, caller)
			}
		}
	}
	if len(testRoots) == 0 {
		addReason("no current test root is connected to the changed functions")
	}

	if len(changed) > 0 && currentIndex.unresolvedCalls+unresolvedCount(previousIndex) > 0 {
		addReason(fullSuiteReason)
	}
	selectionComplete := len(reasons) == 0
	if !selectionComplete {
		testRoots = make(map[string]bool, len(currentIndex.roots))
		for _, root := range currentIndex.roots {
			testRoots[root] = true
		}
	}

	changedList := sortedSet(changed)
	parentList := sortedSet(parentFunctions)
	testList := sortedSet(testRoots)
	all := make(map[string]bool, len(changed)+len(parentFunctions)+len(testRoots))
	for _, id := range changedList {
		all[id] = true
	}
	for _, id := range parentList {
		all[id] = true
	}
	for _, id := range testList {
		all[id] = true
	}

	reasonList := sortedSet(reasons)
	return &FunctionDiffPlan{
		ChangedFunctions: changedList, ParentFunctions: parentList, TestRoots: testList,
		FunctionsToRun: sortedSet(all), SelectionComplete: selectionComplete,
		RunFullSuite: !selectionComplete, Reason: strings.Join(reasonList, "; "),
	}, nil
}

func validateFunctionDiff(diff GitDiff) error {
	if len(diff.Files) == 0 {
		return fmt.Errorf("Git diff has no changed files")
	}
	for i, file := range diff.Files {
		if file.OldPath == "" && file.NewPath == "" {
			return fmt.Errorf("Git diff file %d has no old or new path", i)
		}
		if len(file.OldRanges) > 0 && file.OldPath == "" {
			return fmt.Errorf("Git diff file %d has old line ranges without an old path", i)
		}
		if len(file.NewRanges) > 0 && file.NewPath == "" {
			return fmt.Errorf("Git diff file %d has new line ranges without a new path", i)
		}
		for _, lineRange := range append(append([]LineRange(nil), file.OldRanges...), file.NewRanges...) {
			if lineRange.StartLine < 1 || lineRange.EndLine < lineRange.StartLine {
				return fmt.Errorf("Git diff file %d has invalid line range [%d,%d]", i, lineRange.StartLine, lineRange.EndLine)
			}
		}
	}
	return nil
}

type functionGraphIndex struct {
	nodes           map[string]GraphNode
	byPath          map[string][]GraphNode
	callers         map[string][]string
	roots           []string
	issues          []string
	unresolvedCalls int
}

func newFunctionGraphIndex(snapshot *planningSnapshot) *functionGraphIndex {
	index := &functionGraphIndex{
		nodes:  make(map[string]GraphNode, len(snapshot.nodes)),
		byPath: make(map[string][]GraphNode), callers: make(map[string][]string),
		roots: append([]string(nil), snapshot.roots...), unresolvedCalls: snapshot.unresolvedCalls,
	}
	for _, node := range snapshot.nodes {
		if _, exists := index.nodes[node.ID]; exists {
			index.issues = append(index.issues, "graph contains duplicate function IDs")
			continue
		}
		index.nodes[node.ID] = node
		index.byPath[node.Path] = append(index.byPath[node.Path], node)
	}
	for _, edge := range snapshot.edges {
		if _, ok := index.nodes[edge.Caller]; !ok {
			index.issues = append(index.issues, "graph has a call edge with an unknown caller")
			continue
		}
		if _, ok := index.nodes[edge.Callee]; !ok {
			index.issues = append(index.issues, "graph has a call edge with an unknown callee")
			continue
		}
		index.callers[edge.Callee] = append(index.callers[edge.Callee], edge.Caller)
	}
	for id, callers := range index.callers {
		index.callers[id] = uniqueSorted(callers)
	}
	sort.Strings(index.roots)
	for _, root := range index.roots {
		if _, ok := index.nodes[root]; !ok {
			index.issues = append(index.issues, "graph has a test root with no function node")
		}
	}
	index.issues = uniqueSorted(index.issues)
	return index
}

func mapChangedRanges(index *functionGraphIndex, path string, ranges []LineRange, changed map[string]bool, addReason func(string)) {
	nodes := index.byPath[path]
	for _, lineRange := range ranges {
		matched := false
		for _, node := range nodes {
			if node.Line < 1 || node.EndLine < node.Line {
				addReason("function spans are unavailable for a changed file")
				continue
			}
			if node.Line <= lineRange.EndLine && node.EndLine >= lineRange.StartLine {
				changed[node.ID] = true
				matched = true
			}
		}
		if !matched {
			addReason("a changed line range did not map to a function")
		}
	}
}

func sortedSet(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func unresolvedCount(index *functionGraphIndex) int {
	if index == nil {
		return 0
	}
	return index.unresolvedCalls
}
