package hexagon

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RunTests executes the plan. An empty or malformed selective list falls back to ./....
func RunTests(ctx context.Context, repository string, plan *ImpactPlan, stdout, stderr io.Writer) error {
	if err := validatePlan(plan); err != nil {
		return err
	}
	if stdout == nil || stderr == nil {
		return fmt.Errorf("test runner output streams are required")
	}
	if err := rejectUnsupportedGOFLAGS(); err != nil {
		return err
	}
	if plan.RunFullSuite || !plan.SelectionComplete || len(plan.CandidateTests) == 0 {
		return runGoTest(ctx, repository, testArgs(plan.BuildTags, "./..."), stdout, stderr)
	}
	graph, err := BuildGraph(repository, plan.BuildTags)
	if err != nil || graph.SourceFingerprint != plan.SourceFingerprint || !sameBuildContext(graph.AnalysisContext, plan.AnalysisContext) {
		return runGoTest(ctx, repository, testArgs(plan.BuildTags, "./..."), stdout, stderr)
	}
	packages, err := selectedTestPackages(plan.CandidateTests)
	if err != nil || len(packages) == 0 {
		return runGoTest(ctx, repository, testArgs(plan.BuildTags, "./..."), stdout, stderr)
	}
	for _, packageName := range sortedPackageNames(packages) {
		pattern := "^(" + strings.Join(packages[packageName], "|") + ")$"
		args := testArgs(plan.BuildTags, packageName)
		args = append(args, "-run", pattern)
		if err := runGoTest(ctx, repository, args, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

// RunCandidateTests executes the planner's candidate roots as an explicit
// diagnostic comparison. It intentionally ignores RunFullSuite; callers must
// retain RunTests or another full-suite check as the safety baseline. The graph
// must be freshly built from repository and must be the graph used to create plan.
func RunCandidateTests(ctx context.Context, repository string, graph *Graph, plan *ImpactPlan, stdout, stderr io.Writer) error {
	if err := validatePlan(plan); err != nil {
		return err
	}
	if stdout == nil || stderr == nil {
		return fmt.Errorf("test runner output streams are required")
	}
	if err := rejectUnsupportedGOFLAGS(); err != nil {
		return err
	}
	if graph == nil || graph.FormatVersion != graphFormatVersion || graph.Language != "go" ||
		graph.SourceFingerprint != plan.SourceFingerprint || !sameBuildContext(graph.AnalysisContext, plan.AnalysisContext) {
		return fmt.Errorf("candidate plan does not match the supplied current Go graph")
	}
	if len(plan.CandidateTests) == 0 {
		return nil
	}
	for _, candidate := range plan.CandidateTests {
		if _, exists := graph.Tests[candidate]; !exists {
			return fmt.Errorf("candidate test root %q is absent from the supplied current Go graph", candidate)
		}
	}
	packages, err := selectedTestPackages(plan.CandidateTests)
	if err != nil {
		return err
	}
	for _, packageName := range sortedPackageNames(packages) {
		pattern := "^(" + strings.Join(packages[packageName], "|") + ")$"
		args := []string{"test", "-count=1", "-v"}
		if normalized := normalizedTags(plan.BuildTags); len(normalized) > 0 {
			args = append(args, "-tags="+strings.Join(normalized, ","))
		}
		args = append(args, "-run", pattern, packageName)
		if err := runGoTest(ctx, repository, args, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

func sameBuildContext(left, right BuildContext) bool {
	if left.GOOS != right.GOOS || left.GOARCH != right.GOARCH || left.GoVersion != right.GoVersion ||
		left.Compiler != right.Compiler || left.CgoEnabled != right.CgoEnabled || left.AnalyzerSHA256 != right.AnalyzerSHA256 {
		return false
	}
	leftTags, rightTags := normalizedTags(left.BuildTags), normalizedTags(right.BuildTags)
	if len(leftTags) != len(rightTags) {
		return false
	}
	for index := range leftTags {
		if leftTags[index] != rightTags[index] {
			return false
		}
	}
	return true
}

func testArgs(tags []string, packageName string) []string {
	args := []string{"test"}
	if normalized := normalizedTags(tags); len(normalized) > 0 {
		args = append(args, "-tags="+strings.Join(normalized, ","))
	}
	return append(args, packageName)
}

func runGoTest(ctx context.Context, repository string, args []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, "go", args...)
	command.Dir = repository
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("go %s failed: %w", strings.Join(args, " "), err)
	}
	return nil
}

func selectedTestPackages(candidates []string) (map[string][]string, error) {
	packages := make(map[string][]string)
	for _, candidate := range candidates {
		file, name, found := strings.Cut(candidate, "::")
		if !found || !filepath.IsLocal(filepath.FromSlash(file)) || !strings.HasSuffix(file, "_test.go") || !validTestName(name) {
			return nil, fmt.Errorf("invalid Go test root %q", candidate)
		}
		directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(file)))
		packageName := "."
		if directory != "." {
			packageName = "./" + directory
		}
		packages[packageName] = append(packages[packageName], regexp.QuoteMeta(name))
	}
	for packageName := range packages {
		sort.Strings(packages[packageName])
		packages[packageName] = uniqueStrings(packages[packageName])
	}
	return packages, nil
}

func validTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") || len(name) <= len("Test") {
		return false
	}
	first, _ := utf8.DecodeRuneInString(strings.TrimPrefix(name, "Test"))
	return !unicode.IsLower(first)
}

func sortedPackageNames(packages map[string][]string) []string {
	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
