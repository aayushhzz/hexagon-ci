package main

import (
	"strings"
	"testing"

	"github.com/aayushhzz/hexagon-ci/hexagon"
)

func TestCheckStrictSubset(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		roots      int
		wantError  bool
	}{
		{name: "empty", roots: 10, wantError: true},
		{name: "all roots", candidates: []string{"one", "two"}, roots: 2, wantError: true},
		{name: "inventory too small", candidates: []string{"one", "two"}, roots: 1, wantError: true},
		{name: "strict subset", candidates: []string{"one"}, roots: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkStrictSubset(&hexagon.ImpactPlan{CandidateTests: test.candidates}, test.roots)
			if (err != nil) != test.wantError {
				t.Fatalf("checkStrictSubset() error = %v, want error %t", err, test.wantError)
			}
			if test.wantError && !strings.Contains(err.Error(), "no candidate tests run") {
				t.Fatalf("missing no-execution reason: %v", err)
			}
		})
	}
}

func TestCheckRequiredCandidatePackage(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		required   string
		wantError  bool
	}{
		{
			name:       "required package is selected",
			candidates: []string{"test/workflows_test/list_workflows_test.go::TestListWorkflowsWithSearchQuery"},
			required:   "test/workflows_test",
		},
		{
			name:       "package prefix does not count as a match",
			candidates: []string{"test/workflows_test_helpers/helpers_test.go::TestHelper"},
			required:   "test/workflows_test",
			wantError:  true,
		},
		{
			name:       "missing package fails before tests run",
			candidates: []string{"internal/workflowserver/list_workflows_test.go::TestWorkflowsRegistry_ListWorkflows"},
			required:   "test/workflows_test",
			wantError:  true,
		},
		{
			name:       "empty requirement is disabled",
			candidates: []string{"internal/workflowserver/list_workflows_test.go::TestWorkflowsRegistry_ListWorkflows"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkRequiredCandidatePackage(&hexagon.ImpactPlan{CandidateTests: test.candidates}, test.required)
			if (err != nil) != test.wantError {
				t.Fatalf("checkRequiredCandidatePackage() error = %v, want error %t", err, test.wantError)
			}
			if test.wantError && !strings.Contains(err.Error(), "no candidate tests run") {
				t.Fatalf("missing no-execution reason: %v", err)
			}
		})
	}
}
