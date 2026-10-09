package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

const planFormatVersion = 1

const fullSuiteReason = "AST call and package graphs cannot prove all runtime or non-call dependencies"

// ImpactPlan reports known affected tests and records whether skipping the rest is safe.
type ImpactPlan struct {
	FormatVersion     int          `json:"format_version"`
	Language          string       `json:"language"`
	SourceFingerprint string       `json:"source_fingerprint"`
	ChangedFiles      []string     `json:"changed_files"`
	CandidateTests    []string     `json:"candidate_tests"`
	BuildTags         []string     `json:"build_tags"`
	AnalysisContext   BuildContext `json:"analysis_context"`
	RunFullSuite      bool         `json:"run_full_suite"`
	SelectionComplete bool         `json:"selection_complete,omitempty"`
	Reason            string       `json:"reason"`
}

// PlanFileChanges reports diagnostic test candidates and always retains the full-suite safety gate.
func PlanFileChanges(current *Graph, changedFiles []string, previous *Graph) (*ImpactPlan, error) {
	if current == nil || current.FormatVersion != graphFormatVersion || current.Language != "go" {
		return nil, fmt.Errorf("current graph must be a supported Go format v2 graph")
	}
	changed := make(map[string]bool, len(changedFiles))
	for _, filename := range changedFiles {
		if filename != "" {
			changed[filename] = true
		}
	}
	if len(changed) == 0 {
		return nil, fmt.Errorf("at least one changed file is required")
	}
	currentRoots := make(map[string]bool, len(current.Tests))
	for root := range current.Tests {
		currentRoots[root] = true
	}
	candidates := make(map[string]bool)
	for _, snapshot := range []*Graph{current, previous} {
		if snapshot == nil {
			continue
		}
		files := make(map[string]FileMetadata, len(snapshot.Files))
		nodePaths := make(map[string]string, len(snapshot.Nodes))
		reverseImports := make(map[string]map[string]bool)
		for _, file := range snapshot.Files {
			files[file.Path] = file
			for _, imported := range file.Imports {
				if reverseImports[imported] == nil {
					reverseImports[imported] = make(map[string]bool)
				}
				reverseImports[imported][file.Package] = true
			}
		}
		for _, node := range snapshot.Nodes {
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
		for root, view := range snapshot.Tests {
			if !currentRoots[root] {
				continue
			}
			testFile, hasTestFile := files[nodePaths[root]]
			if hasTestFile && affected[testFile.Package] {
				candidates[root] = true
				continue
			}
			for _, nodeID := range view.Nodes {
				if changed[nodePaths[nodeID]] {
					candidates[root] = true
					break
				}
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
	tags := append([]string{}, current.AnalysisContext.BuildTags...)
	return &ImpactPlan{
		FormatVersion: planFormatVersion, Language: "go",
		SourceFingerprint: current.SourceFingerprint, ChangedFiles: changedList,
		CandidateTests: candidateList, BuildTags: tags, AnalysisContext: current.AnalysisContext, RunFullSuite: true,
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
