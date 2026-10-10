package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

func main() {
	// Large services (e.g. codex) build a multi-GB SSA program. Favour more
	// frequent GC to keep peak memory well below machine limits unless the user
	// has set GOGC explicitly.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(40)
	}
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
		opMapFile  = flag.String("op-map-file", "opmap.json", "reviewed operation->RPC map JSON (by_operation); missing file ignored")
		alias      = flag.String("field-alias", "", "HTTP->RPC field renames, e.g. Artifacts=ArtifactIds,SchemaFragmentIds=CustomSchemaFragmentIds")
		fieldMap   = flag.String("field-map", "fieldmap.json", "field-map JSON file (HTTP->RPC field names); missing file is ignored")
		scaffold   = flag.String("emit-fieldmap-scaffold", "", "write a field-map scaffold from anomalies to this path and exit coverage")
		statusSpec = flag.String("status-spec", "", "generated oapi-codegen client.go for documented HTTP statuses (auto-located under -cover if empty)")
		timing     = flag.Bool("timing", false, "print phase timings (load+ssa, enumerate, analyze) to stderr")

		discover     = flag.Bool("discover", false, "auto-detect gRPC server interface/impl specs in -dir and emit services.json entries")
		gatewayMap   = flag.Bool("gateway-map", false, "derive an operation->RPC map from the gateway in -dir for -backend-pkgs")
		backendPkgs  = flag.String("backend-pkgs", "", "comma-separated backend client package paths (for -gateway-map)")
		rpcWhitelist = flag.String("rpc-whitelist", "", "comma-separated valid backend RPC names to keep (for -gateway-map)")
		service      = flag.String("service", "", "service name to stamp into -gateway-map output")
	)
	flag.Parse()

	phase := func(label string, start time.Time) {
		if *timing {
			fmt.Fprintf(os.Stderr, "[timing] %-18s %v\n", label, time.Since(start).Round(time.Millisecond))
		}
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fail(err)
	}

	// Discovery modes load the whole module (./...) and exit early. They let a
	// service be onboarded with no hand-written interface/impl config or opmap.
	if *discover || *gatewayMap {
		l, err := loadTolerant(absDir, []string{"./..."})
		if err != nil {
			fail(err)
		}
		if *discover {
			ds := l.discoverServers(l.modulePath())
			if *out != "" {
				emit(*out, map[string]any{"services": ds})
			} else {
				fmt.Print(discoverFmt(ds))
			}
			return
		}
		m := l.buildGatewayMap(*service, splitCSV(*backendPkgs), splitCSV(*rpcWhitelist))
		if *out != "" {
			emit(*out, m)
		} else {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(m)
		}
		return
	}

	tLoad := time.Now()
	l, err := load(absDir, splitCSV(*patterns))
	if err != nil {
		fail(err)
	}
	phase("load+ssa", tLoad)
	spec := serverSpec{ifacePkg: *ifacePkg, ifaceName: *ifaceNm, implPkg: *implPkg, implName: *implNm}
	filter := map[string]bool{}
	for _, n := range splitCSV(*only) {
		filter[n] = true
	}
	tEnum := time.Now()
	eps, err := l.enumerate(spec, filter)
	if err != nil {
		fail(err)
	}
	phase("enumerate", tEnum)
	sort.Slice(eps, func(i, j int) bool { return eps[i].rpcName < eps[j].rpcName })

	tAnalyze := time.Now()
	results := make([]*Provenance, 0, len(eps))
	for _, ep := range eps {
		results = append(results, l.analyze(ep, absDir))
	}
	phase("analyze", tAnalyze)
	if *timing {
		fmt.Fprintf(os.Stderr, "[timing] %-18s %d\n", "rpcs", len(results))
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
		fileOverrides, oerr := loadOpMap(*opMapFile)
		if oerr != nil {
			fail(oerr)
		}
		// Reviewed file map is the base; inline -op-map wins per entry.
		overrides = mergeStringMaps(fileOverrides, overrides)
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
		// Index op facts by the RPC they map to, merging ops that map to the same
		// RPC. Iterate operations in sorted order for deterministic merge order and
		// operation labels.
		byRPC := map[string]*opFacts{}
		byRPCOp := map[string]string{}
		opNames := make([]string, 0, len(ops))
		for op := range ops {
			opNames = append(opNames, op)
		}
		sort.Strings(opNames)
		for _, op := range opNames {
			of := ops[op]
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
			respEff := effectiveResponseAliases(fm, p.RPC)
			reports = append(reports, buildCoverage(p, byRPC[p.RPC], byRPCOp[p.RPC], eff, respEff, statusMap))
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

// emit marshals v as indented JSON to path.
func emit(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
