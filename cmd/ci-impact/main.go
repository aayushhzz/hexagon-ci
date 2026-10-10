package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aayushhzz/hexagon-ci/hexagon"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "ci-impact: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ci-impact", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repository := flags.String("repository", "", "head checkout containing the tests to execute")
	baseRepository := flags.String("base-repository", "", "base checkout used to build the previous graph")
	diffPath := flags.String("diff", "", "zero-context Git unified diff path")
	summaryPath := flags.String("summary", "", "optional GitHub Actions summary file")
	outputPath := flags.String("output", "", "optional GitHub Actions step output file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *repository == "" || *baseRepository == "" || *diffPath == "" {
		return fmt.Errorf("usage: ci-impact --repository <head> --base-repository <base> --diff <unified.diff> [--summary <file>]")
	}

	diffFile, err := os.Open(*diffPath)
	if err != nil {
		return fmt.Errorf("open diff: %w", err)
	}
	parsedDiff, parseErr := parseUnifiedDiff(diffFile)
	closeErr := diffFile.Close()
	if parseErr != nil {
		return parseErr
	}
	if closeErr != nil {
		return fmt.Errorf("close diff: %w", closeErr)
	}

	analysisStart := time.Now()
	baseGraph, err := hexagon.BuildGraph(*baseRepository, nil)
	if err != nil {
		return fmt.Errorf("build base graph: %w", err)
	}
	headGraph, err := hexagon.BuildGraph(*repository, nil)
	if err != nil {
		return fmt.Errorf("build head graph: %w", err)
	}
	plan, err := hexagon.PlanDiff(headGraph, parsedDiff, baseGraph)
	if err != nil {
		return fmt.Errorf("plan diff: %w", err)
	}
	analysisDuration := time.Since(analysisStart)

	runStart := time.Now()
	runErr := hexagon.RunCandidateTests(context.Background(), *repository, headGraph, plan, stdout, stderr)
	runDuration := time.Since(runStart)

	summary := formatSummary(plan, len(baseGraph.Tests), len(headGraph.Tests), analysisDuration, runDuration, runErr)
	if *outputPath != "" {
		if err := appendOutputs(*outputPath, formatOutputs(plan, len(headGraph.Tests), analysisDuration, runDuration, runErr)); err != nil {
			return err
		}
	}
	if *summaryPath == "" {
		if _, err := io.WriteString(stdout, summary); err != nil {
			return err
		}
	} else if err := appendSummary(*summaryPath, summary); err != nil {
		return err
	}
	return runErr
}

func formatSummary(plan *hexagon.ImpactPlan, baseTests, headTests int, analysisDuration, runDuration time.Duration, runErr error) string {
	packageCount := candidatePackageCount(plan.CandidateTests)
	status := "passed"
	if runErr != nil {
		status = "failed"
	}
	var builder strings.Builder
	builder.WriteString("## Candidate subset comparison\n\n")
	fmt.Fprintf(&builder, "- Candidate runner: **%s**\n", status)
	fmt.Fprintf(&builder, "- Base graph test roots: **%d**\n", baseTests)
	fmt.Fprintf(&builder, "- Head graph test roots (planner universe): **%d**\n", headTests)
	fmt.Fprintf(&builder, "- Candidate test roots executed: **%d** across **%d** packages\n", len(plan.CandidateTests), packageCount)
	fmt.Fprintf(&builder, "- Planning and graph-build time: **%s**\n", analysisDuration.Round(time.Millisecond))
	fmt.Fprintf(&builder, "- Candidate runner time: **%s**\n", runDuration.Round(time.Millisecond))
	fmt.Fprintf(&builder, "- Candidate pipeline core time (graph builds, planning, and candidate runner): **%s**\n", (analysisDuration + runDuration).Round(time.Millisecond))
	fmt.Fprintf(&builder, "- Plan safety flag: `run_full_suite=%t`; this job explicitly runs candidates for comparison, while `Unit tests` runs the full suite.\n", plan.RunFullSuite)
	fmt.Fprintf(&builder, "- Selection note: %s\n\n", plan.Reason)
	if len(plan.CandidateTests) > 0 {
		fmt.Fprintf(&builder, "<details><summary>Selected test roots (%d)</summary>\n\n", len(plan.CandidateTests))
		for _, candidate := range plan.CandidateTests {
			fmt.Fprintf(&builder, "- `%s`\n", candidate)
		}
		builder.WriteString("\n</details>\n")
	} else {
		builder.WriteString("No test roots were connected to the mapped changes; no `go test` command was needed for this candidate set.\n")
	}
	return builder.String()
}

func formatOutputs(plan *hexagon.ImpactPlan, headTests int, analysisDuration, runDuration time.Duration, runErr error) string {
	status := 0
	if runErr != nil {
		status = 1
	}
	return fmt.Sprintf(
		"head_test_roots=%d\ncandidate_test_roots=%d\nanalysis_milliseconds=%d\ncandidate_runner_milliseconds=%d\ncandidate_status=%d\n",
		headTests, len(plan.CandidateTests), analysisDuration.Milliseconds(), runDuration.Milliseconds(), status,
	)
}

func candidatePackageCount(candidates []string) int {
	packages := make(map[string]bool)
	for _, candidate := range candidates {
		file, _, ok := strings.Cut(candidate, "::")
		if !ok {
			continue
		}
		packages[filepath.ToSlash(filepath.Dir(filepath.FromSlash(file)))] = true
	}
	return len(packages)
}

func appendOutputs(filename, outputs string) error {
	return appendFile(filename, outputs, "step output")
}

func appendSummary(filename, summary string) error {
	return appendFile(filename, summary, "summary")
}

func appendFile(filename, content, kind string) error {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s file: %w", kind, err)
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s file: %w", kind, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s file: %w", kind, err)
	}
	return nil
}
