package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Coverage mapping (Goal A): intersect per-RPC provenance with lifejacket test
// facts. Everything here is syntactic and deterministic. We never claim runtime
// branch coverage. We report which request fields and validation-branch inputs
// the tests actually exercise, and which the RPC reads but no test touches.

// testFact is one Ginkgo It block that issues a request.
type testFact struct {
	Name    string   `json:"name"`
	File    string   `json:"file"`
	Labels  []string `json:"labels"`
	Fields  []string `json:"fields_set"`
	Status  []int    `json:"statuses"`
	Success bool     `json:"asserts_success"`
}

// opFacts aggregates all tests that call one API operation.
type opFacts struct {
	Operation string
	Tests     []testFact
	fieldsSet map[string]map[string]bool // leaf field name -> set of test names
	statuses  map[int]bool
	success   bool // some test asserted a non-error (success) response
}

// CoverageReport is the per-RPC coverage result emitted as JSON.
type CoverageReport struct {
	RPC       string         `json:"rpc"`
	Operation string         `json:"operation"`
	Tests     int            `json:"tests"`
	Status    StatusCoverage `json:"status"`
	Fields    FieldCoverage  `json:"fields"`
	Branches  BranchCoverage `json:"branches"`
	Anomalies Anomalies      `json:"anomalies"`
	TestFacts []testFact     `json:"test_facts"`
	Unmapped  bool           `json:"unmapped,omitempty"`
}

// StatusCoverage compares documented HTTP statuses (from the generated client
// spec) against those the tests assert.
type StatusCoverage struct {
	Documented  []int   `json:"documented"`
	Asserted    []int   `json:"asserted"`
	Missing     []int   `json:"missing"`
	CoveragePct float64 `json:"coverage_pct"`
}

type FieldCoverage struct {
	LeafFieldsReadByRPC int            `json:"leaf_fields_read_by_rpc"`
	CoveredCount        int            `json:"covered_count"`
	CoveragePct         float64        `json:"coverage_pct"`
	ExercisedByTests    int            `json:"request_fields_set_by_tests"`
	Covered             []CoveredField `json:"covered"`
	Gaps                []string       `json:"gaps"`
}

type CoveredField struct {
	Path    string   `json:"path"`
	Match   string   `json:"match"` // exact | alias
	ByTests []string `json:"by_tests"`
}

type BranchCoverage struct {
	Total          int      `json:"total"`
	InputExercised int      `json:"input_exercised"`
	CoveragePct    float64  `json:"coverage_pct"`
	Gaps           []Branch `json:"gaps"`
}

// Anomalies surfaces HTTP-body vs RPC-request field mismatches for review.
type Anomalies struct {
	// UnmatchedTestFields are request-body fields set by tests that match no RPC
	// read path (likely a gateway rename or a field the RPC ignores).
	UnmatchedTestFields []UnmatchedField `json:"unmatched_test_fields"`
}

type UnmatchedField struct {
	Field      string   `json:"field"`
	Suggestion []string `json:"fuzzy_suggestions"`
}

// extractLifejacket parses every *_test.go under root and aggregates per-operation facts.
func extractLifejacket(root string) (map[string]*opFacts, error) {
	fset := token.NewFileSet()
	ops := map[string]*opFacts{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // skip unparseable files, stay deterministic on the rest
		}
		rel := path
		if r, e := filepath.Rel(root, path); e == nil {
			rel = r
		}
		collectTests(f, rel, ops)
		return nil
	})
	return ops, err
}

// collectTests walks a file for Ginkgo It blocks that issue requests.
func collectTests(f *ast.File, file string, ops map[string]*opFacts) {
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isIdent(call.Fun, "It") || len(call.Args) < 2 {
			return true
		}
		name := stringLit(call.Args[0])
		var body *ast.FuncLit
		var labels []string
		for _, a := range call.Args[1:] {
			switch e := a.(type) {
			case *ast.FuncLit:
				body = e
			case *ast.CallExpr:
				if isIdent(e.Fun, "Label") || isSelName(e.Fun, "Label") {
					labels = append(labels, labelArgs(e)...)
				}
			}
		}
		if body == nil {
			return true
		}
		op, fields, statuses, success := scanItBody(body)
		if op == "" {
			return true // not a request-issuing test
		}
		of := ops[op]
		if of == nil {
			of = &opFacts{Operation: op, fieldsSet: map[string]map[string]bool{}, statuses: map[int]bool{}}
			ops[op] = of
		}
		if success {
			of.success = true
		}
		tf := testFact{Name: name, File: file, Labels: labels, Fields: sortedUniq(fields), Status: sortedUniqInt(statuses), Success: success}
		of.Tests = append(of.Tests, tf)
		for _, fld := range fields {
			leaf := strings.ToLower(fld)
			if of.fieldsSet[leaf] == nil {
				of.fieldsSet[leaf] = map[string]bool{}
			}
			of.fieldsSet[leaf][name] = true
		}
		for _, s := range statuses {
			of.statuses[s] = true
		}
		return true
	})
}

// scanItBody extracts the operation called, request fields set, asserted statuses, and success assertions.
func scanItBody(body *ast.FuncLit) (op string, fields []string, statuses []int, success bool) {
	hasStatus := false
	sawBeNil := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			// MakeRequest(ctx, Client.<Op>, req, &resp)
			if isSelName(e.Fun, "MakeRequest") {
				for _, a := range e.Args {
					if sel, ok := a.(*ast.SelectorExpr); ok && isClientRecv(sel.X) {
						op = sel.Sel.Name
					}
				}
			}
			switch callName(e.Fun) {
			case "Equal":
				if len(e.Args) == 1 {
					if n, ok := intLit(e.Args[0]); ok && n >= 100 && n < 600 {
						statuses = append(statuses, n)
					}
				}
			case "BeNil":
				sawBeNil = true
			}
		case *ast.SelectorExpr:
			if e.Sel.Name == "StatusCode" {
				hasStatus = true
			}
		case *ast.CompositeLit:
			if isRequestBodyType(e.Type) {
				fields = append(fields, compositeKeys(e)...)
			}
		}
		return true
	})
	// Explicit StatusCode assertions win, otherwise no status is recorded.
	if !hasStatus {
		statuses = nil
	}
	return op, fields, statuses, sawBeNil
}

// buildCoverage intersects provenance for one RPC with aggregated op facts.
func buildCoverage(p *Provenance, of *opFacts, op string, aliases map[string]string, statusSpec map[string][]int) CoverageReport {
	rep := CoverageReport{RPC: p.RPC, Operation: op}
	leaves := leafReadPaths(p)
	rep.Fields.LeafFieldsReadByRPC = len(leaves)
	rep.Branches.Total = len(p.Branches)
	if of == nil {
		rep.Unmapped = true
		rep.Fields.Gaps = leaves
		rep.Branches.Gaps = append([]Branch{}, p.Branches...)
		documented := statusSpec[op]
		rep.Status = statusCoverage(documented, map[int]bool{})
		return rep
	}
	rep.Tests = len(of.Tests)
	rep.TestFacts = of.Tests
	asserted := map[int]bool{}
	for status := range of.statuses {
		asserted[status] = true
	}
	if of.success {
		for _, status := range successCodes(statusSpec[op]) {
			asserted[status] = true
		}
	}
	rep.Status = statusCoverage(statusSpec[op], asserted)
	rep.Fields.ExercisedByTests = len(of.fieldsSet)

	// Normalized index of test-set fields: canonical name -> original -> tests.
	normFields := map[string]map[string]map[string]bool{}
	for leaf, tests := range of.fieldsSet {
		c := canon(leaf)
		if normFields[c] == nil {
			normFields[c] = map[string]map[string]bool{}
		}
		normFields[c][leaf] = tests
	}
	// Alias map keyed by canonical test-field name -> canonical RPC leaf name.
	canonAlias := map[string]string{}
	for k, v := range aliases {
		canonAlias[canon(k)] = canon(v)
	}

	// Field coverage over RPC leaf paths.
	for _, path := range leaves {
		leaf := canon(lastComponent(path))
		if tests, ok := matchField(leaf, normFields, canonAlias); ok {
			rep.Fields.Covered = append(rep.Fields.Covered, CoveredField{Path: path, Match: tests.kind, ByTests: tests.tests})
		} else {
			rep.Fields.Gaps = append(rep.Fields.Gaps, path)
		}
	}
	rep.Fields.CoveredCount = len(rep.Fields.Covered)
	rep.Fields.CoveragePct = pct(rep.Fields.CoveredCount, rep.Fields.LeafFieldsReadByRPC)

	// Branch coverage.
	leafCanon := map[string]bool{}
	for _, path := range leaves {
		leafCanon[canon(lastComponent(path))] = true
	}
	for _, b := range p.Branches {
		if branchInputsExercised(b, normFields, canonAlias) {
			rep.Branches.InputExercised++
		} else {
			rep.Branches.Gaps = append(rep.Branches.Gaps, b)
		}
	}
	rep.Branches.CoveragePct = pct(rep.Branches.InputExercised, rep.Branches.Total)

	// Anomaly detection: test fields that match no RPC leaf (likely renames).
	rpcLeaves := map[string][]string{} // canonical leaf -> full paths
	for _, path := range leaves {
		c := canon(lastComponent(path))
		rpcLeaves[c] = append(rpcLeaves[c], path)
	}
	for leaf := range of.fieldsSet {
		c := canon(leaf)
		if _, ok := rpcLeaves[c]; ok {
			continue
		}
		if tgt, ok := canonAlias[c]; ok {
			if _, ok := rpcLeaves[tgt]; ok {
				continue
			}
		}
		rep.Anomalies.UnmatchedTestFields = append(rep.Anomalies.UnmatchedTestFields,
			UnmatchedField{Field: leaf, Suggestion: fuzzySuggest(c, rpcLeaves)})
	}
	sort.Slice(rep.Anomalies.UnmatchedTestFields, func(i, j int) bool {
		return rep.Anomalies.UnmatchedTestFields[i].Field < rep.Anomalies.UnmatchedTestFields[j].Field
	})
	return rep
}

func statusCoverage(documented []int, assertedSet map[int]bool) StatusCoverage {
	missing := make([]int, 0)
	covered := 0
	for _, status := range documented {
		if assertedSet[status] {
			covered++
		} else {
			missing = append(missing, status)
		}
	}
	return StatusCoverage{
		Documented:  documented,
		Asserted:    sortedKeysInt(assertedSet),
		Missing:     missing,
		CoveragePct: pct(covered, len(documented)),
	}
}

type fieldMatch struct {
	kind  string
	tests []string
}

// matchField resolves an RPC leaf against test-set fields: exact-normalized, then alias.
func matchField(leaf string, normFields map[string]map[string]map[string]bool, canonAlias map[string]string) (fieldMatch, bool) {
	if byOrig, ok := normFields[leaf]; ok {
		return fieldMatch{kind: "exact", tests: mergeTests(byOrig)}, true
	}
	for testCanon, tgt := range canonAlias {
		if tgt == leaf {
			if byOrig, ok := normFields[testCanon]; ok {
				return fieldMatch{kind: "alias", tests: mergeTests(byOrig)}, true
			}
		}
	}
	return fieldMatch{}, false
}

func mergeTests(byOrig map[string]map[string]bool) []string {
	seen := map[string]bool{}
	for _, tests := range byOrig {
		for t := range tests {
			seen[t] = true
		}
	}
	return sortedKeys(seen)
}

func branchInputsExercised(b Branch, normFields map[string]map[string]map[string]bool, canonAlias map[string]string) bool {
	if len(b.PredicatePaths) == 0 {
		return false
	}
	for _, pth := range b.PredicatePaths {
		if _, ok := matchField(canon(lastComponent(pth)), normFields, canonAlias); !ok {
			return false
		}
	}
	return true
}

// fuzzySuggest returns RPC leaf paths heuristically related to an unmatched
// test field (substring containment or shared stem), for human/agent review.
func fuzzySuggest(c string, rpcLeaves map[string][]string) []string {
	var out []string
	for leaf, paths := range rpcLeaves {
		if fuzzyRelated(c, leaf) {
			out = append(out, paths...)
		}
	}
	sort.Strings(out)
	return out
}

func fuzzyRelated(a, b string) bool {
	if a == b {
		return false
	}
	short := a
	if len(b) < len(a) {
		short = b
	}
	if len(short) < 4 { // avoid noise from trivial tokens like "id"
		return false
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	return stem(a) == stem(b) && len(stem(a)) >= 4
}

// stem strips common plural/id suffixes so e.g. artifacts ~ artifactids.
func stem(s string) string {
	for _, suf := range []string{"ids", "id", "s"} {
		if strings.HasSuffix(s, suf) && len(s) > len(suf)+2 {
			return s[:len(s)-len(suf)]
		}
	}
	return s
}

// canon normalizes a field/leaf name: lowercase, drop underscores and non-alnum,
// so snake_case (display_name) and PascalCase (DisplayName) collapse to one key.
func canon(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func pct(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(int(float64(num)/float64(den)*1000+0.5)) / 10 // one decimal place
}

// mergeOpFacts folds src into dst (several API operations can map to one RPC,
// e.g. AccountsGet and AccountsGetPost both -> GetAccount).
func mergeOpFacts(dst, src *opFacts) {
	dst.Tests = append(dst.Tests, src.Tests...)
	for leaf, tests := range src.fieldsSet {
		if dst.fieldsSet[leaf] == nil {
			dst.fieldsSet[leaf] = map[string]bool{}
		}
		for t := range tests {
			dst.fieldsSet[leaf][t] = true
		}
	}
	for s := range src.statuses {
		dst.statuses[s] = true
	}
	if src.success {
		dst.success = true
	}
}

// leafReadPaths returns request access paths the RPC reads that are not a prefix
// of a deeper read path (the settable leaf fields), sorted and deduped.
func leafReadPaths(p *Provenance) []string {
	read := map[string]bool{}
	for _, ip := range p.InputPaths {
		for _, k := range ip.Kinds {
			if k == "read" || k == "predicate" {
				read[ip.Path] = true
				break
			}
		}
	}
	var out []string
	for path := range read {
		if path == "in" {
			continue
		}
		leaf := true
		for other := range read {
			if other != path && strings.HasPrefix(other, path+".") {
				leaf = false
				break
			}
		}
		if leaf {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// mapOpToRPC derives the janus RPC name for an API operation, honoring overrides
// and preferring a candidate that actually exists in the service's RPC set.
// janus naming is inconsistent (CreateAccount but ListAccounts), so we try both
// the plural resource and its singular form.
func mapOpToRPC(op string, overrides map[string]string, rpcSet map[string]bool) string {
	if r, ok := overrides[op]; ok {
		return r
	}
	base := strings.TrimSuffix(op, "Post")
	base = strings.TrimSuffix(base, "WithBody")
	words := camelSplit(base)
	if len(words) < 2 {
		return pickExisting([]string{op}, rpcSet)
	}
	verbs := map[string]bool{"Create": true, "Get": true, "List": true, "Update": true,
		"Delete": true, "Merge": true, "Count": true, "Export": true, "Import": true,
		"Search": true, "Add": true, "Remove": true, "Set": true, "Group": true,
		"Archive": true, "Unarchive": true, "Move": true, "Scan": true, "Join": true,
		"Activate": true, "Deactivate": true, "Link": true, "Unlink": true, "Deploy": true}
	last := words[len(words)-1]
	if !verbs[last] {
		return pickExisting([]string{op}, rpcSet)
	}
	resource := strings.Join(words[:len(words)-1], "")
	cands := []string{last + resource, last + singularize(resource)}
	return pickExisting(cands, rpcSet)
}

// pickExisting returns the first candidate present in the RPC set, or "" if none.
func pickExisting(cands []string, rpcSet map[string]bool) string {
	for _, c := range cands {
		if rpcSet[c] {
			return c
		}
	}
	return ""
}

func singularize(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "ses"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s"):
		return s[:len(s)-1]
	}
	return s
}

// --- small AST helpers ---

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isSelName(e ast.Expr, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}

func isClientRecv(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && strings.HasPrefix(id.Name, "Client")
}

// callName returns the function name for an Ident (dot-imported) or SelectorExpr call.
func callName(e ast.Expr) string {
	switch f := e.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func isRequestBodyType(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && strings.HasSuffix(sel.Sel.Name, "RequestBody")
}

func compositeKeys(c *ast.CompositeLit) []string {
	var out []string
	for _, el := range c.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok {
				out = append(out, id.Name)
			}
		}
	}
	return out
}

func labelArgs(c *ast.CallExpr) []string {
	var out []string
	for _, a := range c.Args {
		switch e := a.(type) {
		case *ast.SelectorExpr:
			out = append(out, e.Sel.Name)
		case *ast.Ident:
			out = append(out, e.Name)
		}
	}
	return out
}

func stringLit(e ast.Expr) string {
	if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
		if s, err := strconv.Unquote(b.Value); err == nil {
			return s
		}
	}
	return ""
}

func intLit(e ast.Expr) (int, bool) {
	if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.INT {
		if n, err := strconv.Atoi(b.Value); err == nil {
			return n, true
		}
	}
	return 0, false
}

func lastComponent(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

func camelSplit(s string) []string {
	var words []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			words = append(words, s[start:i])
			start = i
		}
	}
	words = append(words, s[start:])
	return words
}

func sortedUniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedUniqInt(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range in {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysInt(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
