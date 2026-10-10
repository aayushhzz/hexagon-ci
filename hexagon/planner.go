package hexagon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

const planFormatVersion = 1

const fullSuiteReason = "AST call and package graphs cannot prove all runtime or non-call dependencies"

// ImpactPlan reports known affected tests and records whether skipping the rest is safe.
type ImpactPlan struct {
	FormatVersion     int           `json:"format_version"`
	Language          string        `json:"language"`
	SourceFingerprint string        `json:"source_fingerprint"`
	ChangedFiles      []string      `json:"changed_files"`
	CandidateTests    []string      `json:"candidate_tests"`
	ChangedFunctions  []string      `json:"changed_functions,omitempty"`
	ImpactFunctions   []string      `json:"impact_functions,omitempty"`
	DiffMappings      []DiffMapping `json:"diff_mappings,omitempty"`
	BuildTags         []string      `json:"build_tags"`
	AnalysisContext   BuildContext  `json:"analysis_context"`
	RunFullSuite      bool          `json:"run_full_suite"`
	SelectionComplete bool          `json:"selection_complete,omitempty"`
	Reason            string        `json:"reason"`
}

// PlanFileChanges reports diagnostic test candidates and always retains the full-suite safety gate.
func PlanFileChanges(current *Graph, changedFiles []string, previous *Graph) (*ImpactPlan, error) {
	if current == nil || current.FormatVersion != graphFormatVersion || current.Language != "go" {
		return nil, fmt.Errorf("current graph must be a supported Go format v2 graph")
	}
	var before *planningSnapshot
	if previous != nil {
		before = snapshotFromNamed(previous)
	}
	return planFileChanges(snapshotFromNamed(current), changedFiles, before)
}

type planningSnapshot struct {
	sourceFingerprint string
	context           BuildContext
	files             []FileMetadata
	nodes             []GraphNode
	edges             []GraphEdge
	roots             []string
	unresolvedCalls   int
	forEachNode       func(string, func(string) bool) error
}

func snapshotFromNamed(graph *Graph) *planningSnapshot {
	roots := make([]string, 0, len(graph.Tests))
	for root := range graph.Tests {
		roots = append(roots, root)
	}
	return &planningSnapshot{
		sourceFingerprint: graph.SourceFingerprint, context: graph.AnalysisContext,
		files: graph.Files, nodes: graph.Nodes, edges: graph.Edges, roots: roots,
		unresolvedCalls: len(graph.UnresolvedCalls),
		forEachNode: func(root string, visit func(string) bool) error {
			for _, node := range graph.Tests[root].Nodes {
				if !visit(node) {
					break
				}
			}
			return nil
		},
	}
}

func snapshotFromCompact(graph *CompactGraph) (*planningSnapshot, error) {
	index, err := NewCompactIndex(graph)
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(graph.Tests))
	for _, test := range graph.Tests {
		roots = append(roots, graph.Nodes[test.Root].ID)
	}
	edges := make([]GraphEdge, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges = append(edges, GraphEdge{Caller: graph.Nodes[edge.Caller].ID, Callee: graph.Nodes[edge.Callee].ID})
	}
	return &planningSnapshot{
		sourceFingerprint: graph.SourceFingerprint, context: graph.AnalysisContext,
		files: graph.Files, nodes: graph.Nodes, edges: edges, roots: roots,
		unresolvedCalls: len(graph.Unresolved),
		forEachNode: func(root string, visit func(string) bool) error {
			return index.forEachTestNode(root, func(id uint32) bool { return visit(graph.Nodes[id].ID) })
		},
	}, nil
}

// PlanGraphFiles reads either graph-v2 JSON or a compact snapshot on each side.
// Compact test views are visited directly without expanding all views.
func PlanGraphFiles(currentFile string, changedFiles []string, previousFile string) (*ImpactPlan, error) {
	current, err := readPlanningSnapshot(currentFile)
	if err != nil {
		return nil, err
	}
	var previous *planningSnapshot
	if previousFile != "" {
		previous, err = readPlanningSnapshot(previousFile)
		if err != nil {
			return nil, err
		}
	}
	return planFileChanges(current, changedFiles, previous)
}

func readPlanningSnapshot(filename string) (*planningSnapshot, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	header := make([]byte, len(compactMagic))
	_, readErr := io.ReadFull(file, header)
	_ = file.Close()
	if readErr == nil && string(header[:3]) == "HGC" {
		graph, err := ReadCompactGraph(filename)
		if err != nil {
			return nil, err
		}
		return snapshotFromCompact(graph)
	}
	graph, err := ReadGraph(filename)
	if err != nil {
		return nil, err
	}
	return snapshotFromNamed(graph), nil
}

func planFileChanges(current *planningSnapshot, changedFiles []string, previous *planningSnapshot) (*ImpactPlan, error) {
	changed := make(map[string]bool, len(changedFiles))
	for _, filename := range changedFiles {
		if filename != "" {
			changed[filename] = true
		}
	}
	if len(changed) == 0 {
		return nil, fmt.Errorf("at least one changed file is required")
	}
	currentRoots := make(map[string]bool, len(current.roots))
	for _, root := range current.roots {
		currentRoots[root] = true
	}
	candidates := make(map[string]bool)
	for _, snapshot := range []*planningSnapshot{current, previous} {
		if snapshot == nil {
			continue
		}
		files := make(map[string]FileMetadata, len(snapshot.files))
		nodePaths := make(map[string]string, len(snapshot.nodes))
		reverseImports := make(map[string]map[string]bool)
		for _, file := range snapshot.files {
			files[file.Path] = file
			for _, imported := range file.Imports {
				if reverseImports[imported] == nil {
					reverseImports[imported] = make(map[string]bool)
				}
				reverseImports[imported][file.Package] = true
			}
		}
		for _, node := range snapshot.nodes {
			nodePaths[node.ID] = node.Path
		}
		affected := make(map[string]bool)
		queue := make([]string, 0)
		for filename := range changed {
			if file, exists := files[filename]; exists && !affected[file.Package] {
				affected[file.Package] = true
				queue = append(queue, file.Package)
			}
		}
		for len(queue) > 0 {
			packageName := queue[0]
			queue = queue[1:]
			for importer := range reverseImports[packageName] {
				if !affected[importer] {
					affected[importer] = true
					queue = append(queue, importer)
				}
			}
		}
		for _, root := range snapshot.roots {
			if !currentRoots[root] {
				continue
			}
			testFile, hasTestFile := files[nodePaths[root]]
			if hasTestFile && affected[testFile.Package] {
				candidates[root] = true
				continue
			}
			if err := snapshot.forEachNode(root, func(nodeID string) bool {
				if changed[nodePaths[nodeID]] {
					candidates[root] = true
					return false
				}
				return true
			}); err != nil {
				return nil, err
			}
		}
	}
	changedList := make([]string, 0, len(changed))
	for filename := range changed {
		changedList = append(changedList, filename)
	}
	sort.Strings(changedList)
	candidateList := make([]string, 0, len(candidates))
	for candidate := range candidates {
		candidateList = append(candidateList, candidate)
	}
	sort.Strings(candidateList)
	tags := append([]string{}, current.context.BuildTags...)
	return &ImpactPlan{
		FormatVersion: planFormatVersion, Language: "go",
		SourceFingerprint: current.sourceFingerprint, ChangedFiles: changedList,
		CandidateTests: candidateList, BuildTags: tags, AnalysisContext: current.context, RunFullSuite: true,
		Reason: fullSuiteReason,
	}, nil
}

// WritePlan atomically publishes a plan so readers never see partial JSON.
func WritePlan(filename string, plan *ImpactPlan) error {
	if err := validatePlan(plan); err != nil {
		return err
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("encode impact plan: %w", err)
	}
	return atomicWrite(filename, append(data, '\n'), false)
}

// ReadPlan tolerates added fields but rejects unsupported versions and languages.
func ReadPlan(filename string) (*ImpactPlan, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read plan %s: %w", filename, err)
	}
	var plan ImpactPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("parse plan %s: %w", filename, err)
	}
	if err := validatePlan(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func validatePlan(plan *ImpactPlan) error {
	if plan == nil || plan.FormatVersion != planFormatVersion || plan.Language != "go" {
		if plan == nil {
			return fmt.Errorf("impact plan is required")
		}
		return fmt.Errorf("unsupported impact plan format or language: format_version=%d language=%q", plan.FormatVersion, plan.Language)
	}
	if len(plan.ChangedFiles) == 0 {
		return fmt.Errorf("impact plan has no changed files")
	}
	return nil
}
