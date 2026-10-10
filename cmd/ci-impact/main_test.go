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
