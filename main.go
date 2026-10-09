package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if err := runCLI(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "hexagon-ci: %v\n", err)
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	command := args[0]
	if command != "graph" && command != "plan" && command != "test" {
		// Preserve the original Python CLI's positional-repository graph invocation.
		return runGraphCommand(args, stdout)
	}
	args = args[1:]
	switch command {
	case "graph":
		return runGraphCommand(args, stdout)
	case "plan":
		return runPlanCommand(args, stdout)
	case "test":
		return runTestCommand(ctx, args, stdout, stderr)
	default:
		return usageError()
	}
}

func runGraphCommand(args []string, stdout io.Writer) error {
	positionals, options, err := parseOptions(args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return fmt.Errorf("graph requires one repository path; %s", usageText)
	}
	if language := options.one("language"); language != "" && language != "go" {
		return fmt.Errorf("the native CLI analyzes Go repositories; the archived Python reference is in python/")
	}
	output := options.one("output")
	if output == "" {
		return fmt.Errorf("--output is required")
	}
	graph, err := RebuildToFile(positionals[0], output, splitTags(options.one("go-tags")))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Rebuilt %d test graphs from %d Go files\n", len(graph.Tests), graph.FileCount)
	return err
}

func runPlanCommand(args []string, stdout io.Writer) error {
	positionals, options, err := parseOptions(args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return fmt.Errorf("plan requires one graph path; %s", usageText)
	}
	changed := options.all("changed-file")
	changed = append(changed, options.all("changed-files")...)
	for i := range changed {
		changed[i] = strings.TrimSpace(changed[i])
	}
	current, err := ReadGraph(positionals[0])
	if err != nil {
		return err
	}
	var previous *Graph
	if filename := options.one("previous-graph"); filename != "" {
		previous, err = ReadGraph(filename)
		if err != nil {
			return err
		}
	}
	plan, err := PlanFileChanges(current, changed, previous)
	if err != nil {
		return err
	}
	if output := options.one("output"); output != "" {
		if err := WritePlan(output, plan); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Planned %d candidate tests; full suite required\n", len(plan.CandidateTests))
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(plan)
}

func runTestCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	positionals, options, err := parseOptions(args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return fmt.Errorf("test requires one repository path; %s", usageText)
	}
	planPath := options.one("plan")
	if planPath == "" {
		return fmt.Errorf("--plan is required")
	}
	plan, err := ReadPlan(planPath)
	if err != nil {
		return err
	}
	return RunTests(ctx, positionals[0], plan, stdout, stderr)
}

type optionValues map[string][]string

func (options optionValues) one(name string) string {
	values := options[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func (options optionValues) all(name string) []string {
	return options[name]
}

func parseOptions(args []string) ([]string, optionValues, error) {
	positionals := make([]string, 0, 1)
	options := make(optionValues)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}
		nameValue := strings.TrimLeft(arg, "-")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if !hasValue {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
				return nil, nil, fmt.Errorf("option --%s needs a value", name)
			}
			index++
			value = args[index]
		}
		switch name {
		case "output", "language", "go-tags", "go-helper", "previous-graph", "changed-file", "changed-files", "plan":
			options[name] = append(options[name], value)
		default:
			return nil, nil, fmt.Errorf("unknown option --%s", name)
		}
	}
	return positionals, options, nil
}

func splitTags(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

const usageText = "usage: hexagon-ci graph <repo> --output graph.json [--go-tags tags] | hexagon-ci plan <graph> --changed-file path [--previous-graph graph.json] [--output plan.json] | hexagon-ci test <repo> --plan plan.json"

func usageError() error { return fmt.Errorf("%s", usageText) }
