package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aayushhzz/hexagon-ci/hexagon"
)

func TestRunCandidatesWithReportPublishesSelectionBeforeExecution(t *testing.T) {
	plan := &hexagon.ImpactPlan{
		CandidateTests: []string{
			"cache/cache_test.go::TestCacheGet",
			"cache/cache_test.go::TestCacheSet",
		},
		RunFullSuite: true,
		Reason:       "diagnostic selection",
	}
	var stdout bytes.Buffer
	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	var executed bool

	runDuration, err := runCandidatesWithReport(plan, 9, 10, time.Millisecond, &stdout, summaryPath, func() error {
		executed = true
		if !strings.Contains(stdout.String(), "**2 / 10** (selected / total)") {
			t.Error("selection count was not printed before candidate execution")
		}
		if !strings.Contains(stdout.String(), "cache/cache_test.go::TestCacheGet") || !strings.Contains(stdout.String(), "cache/cache_test.go::TestCacheSet") {
			t.Error("selected test roots were not printed before candidate execution")
		}
		if strings.Contains(stdout.String(), "Candidate runner: **passed**") {
			t.Error("final result was printed before candidate execution")
		}
		if _, err := os.Stat(summaryPath); err != nil {
			t.Errorf("Actions summary was not created before candidate execution: %v", err)
		} else if contents, err := os.ReadFile(summaryPath); err != nil {
			t.Errorf("read Actions summary before candidate execution: %v", err)
		} else if !strings.Contains(string(contents), "**2 / 10** (selected / total)") {
			t.Error("Actions summary did not contain the planned count before candidate execution")
		}
		_, _ = stdout.WriteString("=== RUN   TestCacheGet\n")
		return nil
	})
	if err != nil {
		t.Fatalf("runCandidatesWithReport() error = %v", err)
	}
	if !executed {
		t.Fatal("candidate runner was not called")
	}
	if runDuration < 0 {
		t.Fatalf("run duration = %s, want non-negative", runDuration)
	}

	stdoutText := stdout.String()
	if strings.Index(stdoutText, "**2 / 10**") > strings.Index(stdoutText, "=== RUN   TestCacheGet") {
		t.Fatal("selection report appeared after candidate test output")
	}
	if !strings.Contains(stdoutText, "Candidate runner: **passed**") {
		t.Fatal("final candidate result was not printed")
	}
	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read Actions summary: %v", err)
	}
	if !strings.Contains(string(summary), "Candidate runner: **passed**") {
		t.Fatal("final candidate result was not appended to the Actions summary")
	}
}

func TestRunCandidatesWithReportAppendsFailureResult(t *testing.T) {
	wantErr := errors.New("candidate test failed")
	var stdout bytes.Buffer
	_, err := runCandidatesWithReport(
		&hexagon.ImpactPlan{CandidateTests: []string{"pkg_test.go::TestExample"}},
		1, 1, 0, &stdout, "", func() error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runCandidatesWithReport() error = %v, want %v", err, wantErr)
	}
	if !strings.Contains(stdout.String(), "Candidate runner: **failed**") {
		t.Fatal("failed candidate result was not printed")
	}
}
