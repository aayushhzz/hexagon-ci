package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	var (
		dir        = flag.String("dir", ".", "target module directory to analyze")
		patterns   = flag.String("patterns", "./internal/service/...", "comma-separated package patterns")
		ifacePkg   = flag.String("iface-pkg", "github.com/devrev/janus-client/api", "server interface package path")
		ifaceNm    = flag.String("iface", "JanusServer", "server interface name")
		implPkg    = flag.String("impl-pkg", "github.com/devrev/janus/internal/service", "concrete server package path")
		implNm     = flag.String("impl", "Janus", "concrete server type name")
		only       = flag.String("only", "", "comma-separated RPC names to analyze (default: all)")
		out        = flag.String("out", "", "output file (default: stdout)")
		cover      = flag.String("cover", "", "lifejacket testcases dir: emit coverage report instead of raw provenance")
		opMap      = flag.String("op-map", "", "operation->RPC overrides, e.g. AccountsCreate=CreateAccount,DevOrgsGet=GetDevOrg")
		alias      = flag.String("field-alias", "", "HTTP->RPC field renames, e.g. Artifacts=ArtifactIds,SchemaFragmentIds=CustomSchemaFragmentIds")
		fieldMap   = flag.String("field-map", "fieldmap.json", "field-map JSON file (HTTP->RPC field names); missing file is ignored")
		scaffold   = flag.String("emit-fieldmap-scaffold", "", "write a field-map scaffold from anomalies to this path and exit coverage")
		statusSpec = flag.String("status-spec", "", "generated oapi-codegen client.go for documented HTTP statuses (auto-located under -cover if empty)")
	)
	flag.Parse()

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fail(err)
	}
	l, err := load(absDir, splitCSV(*patterns))
	if err != nil {
		fail(err)
	}
	spec := serverSpec{ifacePkg: *ifacePkg, ifaceName: *ifaceNm, implPkg: *implPkg, implName: *implNm}
	filter := map[string]bool{}
	for _, n := range splitCSV(*only) {
		filter[n] = true
	}
	eps, err := l.enumerate(spec, filter)
	if err != nil {
		fail(err)
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].rpcName < eps[j].rpcName })

	results := make([]*Provenance, 0, len(eps))
	for _, ep := range eps {
		results = append(results, l.analyze(ep, absDir))
	}

	var payload any
	if *cover != "" {
		absCover, err := filepath.Abs(*cover)
		if err != nil {
			fail(err)
		}
		specPath := *statusSpec
		if specPath == "" {
			specPath = locateStatusSpec(absCover)
		}
		var statusMap map[string][]int
		if specPath != "" {
			statusMap, err = parseStatusSpec(specPath)
			if err != nil {
				fail(err)
			}
		}
		ops, err := extractLifejacket(absCover)
		if err != nil {
			fail(err)
		}
		overrides := parseOverrides(*opMap)
		aliases := parseOverrides(*alias)
		fm, _, ferr := loadFieldMap(*fieldMap)
		if ferr != nil {
			fail(ferr)
		}
		// Set of real RPC names for set-aware op->RPC mapping.
		rpcSet := map[string]bool{}
		for _, p := range results {
			rpcSet[p.RPC] = true
		}
		// Index op facts by the RPC they map to, merging ops that map to the same RPC.
		byRPC := map[string]*opFacts{}
		byRPCOp := map[string]string{}
		for op, of := range ops {
			rpc := mapOpToRPC(op, overrides, rpcSet)
			if rpc == "" {
				continue
			}
			if existing := byRPC[rpc]; existing != nil {
				mergeOpFacts(existing, of)
				byRPCOp[rpc] = byRPCOp[rpc] + "," + op
			} else {
				byRPC[rpc] = of
				byRPCOp[rpc] = op
			}
		}
		reports := make([]CoverageReport, 0, len(results))
		for _, p := range results {
			eff := effectiveAliases(fm, p.RPC, aliases)
			reports = append(reports, buildCoverage(p, byRPC[p.RPC], byRPCOp[p.RPC], eff, statusMap))
		}
		if *scaffold != "" {
			if err := writeScaffold(*scaffold, reports); err != nil {
				fail(err)
			}
			fmt.Fprintf(os.Stderr, "wrote field-map scaffold to %s\n", *scaffold)
			return
		}
		payload = map[string]any{
			"service":  *implNm,
			"rpcs":     len(reports),
			"coverage": reports,
		}
	} else {
		payload = map[string]any{
			"service":     *implNm,
			"entrypoints": len(results),
			"provenance":  results,
		}
	}

	enc, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fail(err)
	}
	if *out == "" {
		fmt.Println(string(enc))
	} else if err := os.WriteFile(*out, enc, 0o644); err != nil {
		fail(err)
	} else {
		fmt.Fprintf(os.Stderr, "wrote %d entrypoints to %s\n", len(results), *out)
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseOverrides(s string) map[string]string {
	m := map[string]string{}
	for _, kv := range splitCSV(s) {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
		}
	}
	return m
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
