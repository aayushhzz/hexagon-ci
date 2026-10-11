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
	Name         string   `json:"name"`
	File         string   `json:"file"`
	Labels       []string `json:"labels"`
	Fields       []string `json:"fields_set"`
	Status       []int    `json:"statuses"`
	Success      bool     `json:"asserts_success"`
	AssertsError bool     `json:"asserts_error"`
	RespFields   []string `json:"response_fields_asserted"`
}

// opFacts aggregates all tests that call one API operation.
type opFacts struct {
	Operation string
	Tests     []testFact
	fieldsSet map[string]map[string]bool // leaf field name -> set of test names
	statuses  map[int]bool
	success   bool                       // some test asserted a non-error (success) response
	respPaths map[string]map[string]bool // canonical response path -> set of test names
}

// CoverageReport is the per-RPC coverage result emitted as JSON.
type CoverageReport struct {
	RPC          string             `json:"rpc"`
	Operation    string             `json:"operation"`
	Tests        int                `json:"tests"`
	Status       StatusCoverage     `json:"status"`
	Fields       FieldCoverage      `json:"fields"`
	Branches     BranchCoverage     `json:"branches"`
	Dependencies DependencyCoverage `json:"dependencies"`
	Anomalies    Anomalies          `json:"anomalies"`
	TestFacts    []testFact         `json:"test_facts"`
	Unmapped     bool               `json:"unmapped,omitempty"`
}

// DependencyCoverage surfaces the downstream work an RPC does beyond validating
// its own request: calls to other services and to the datastore. Input and
// branch coverage are blind to this, so a read/enrich RPC (for example one that
// fetches a schema from schemaregistry and builds the response) can look
// "uncovered" while its real behaviour is these calls. We can enumerate the
// surface statically; we cannot prove a downstream path executed without a
// runtime trace, so ReachedByTest only means a passing test invoked the RPC.
type DependencyCoverage struct {
	Services       []ServiceDep `json:"services"`        // external service calls, grouped
	DatastoreCalls []string     `json:"datastore_calls"` // db operation names
	ServiceCount   int          `json:"service_count"`
	CallCount      int          `json:"call_count"`      // external service calls only
	ReachedByTest  bool         `json:"reached_by_test"` // a passing test invoked this RPC
	// Result verification over external dependencies whose result flows into the
	// response (data enrichment). UsedInResponse counts dependency->response
	// field links; ResultChecked counts those a test actually asserts.
	UsedInResponse int            `json:"deps_result_used_in_response"`
	ResultChecked  int            `json:"deps_result_checked_by_test"`
	ResponseLinks  []ResponseLink `json:"response_links"`
}

// ResponseLink is one downstream dependency whose result lands in a response
// field, and whether a linked test asserts on that field.
type ResponseLink struct {
	Dep       string   `json:"dep"`        // service:Method
	Path      string   `json:"path"`       // response field path, e.g. out.Total
	CheckedBy []string `json:"checked_by"` // tests asserting this path
	Checked   bool     `json:"checked"`
}

// ServiceDep is one downstream service and the methods this RPC calls on it.
type ServiceDep struct {
	Service string   `json:"service"`
	Calls   []string `json:"calls"`
}

// StatusCoverage compares documented HTTP statuses (from the generated client
// spec) against those the tests assert.
type StatusCoverage struct {
	Documented  []int   `json:"documented"`
	Asserted    []int   `json:"asserted"`
	Missing     []int   `json:"missing"`
	Infra       []int   `json:"infra_statuses"`
	CoveragePct float64 `json:"coverage_pct"`
}

// infraStatuses are response codes the gateway documents for essentially every
// operation (payload too large, rate limiting, server/unavailable) but that are
// cross-cutting infrastructure rather than the endpoint's own behaviour. They
// are excluded from the response-code denominator and reported separately.
var infraStatuses = map[int]bool{
	413: true, // Request Entity Too Large
	429: true, // Too Many Requests
	500: true, // Internal Server Error
	502: true, // Bad Gateway
	503: true, // Service Unavailable
	504: true, // Gateway Timeout
}

type FieldCoverage struct {
	LeafFieldsReadByRPC int            `json:"leaf_fields_read_by_rpc"`
	CoveredCount        int            `json:"covered_count"`
	CoveragePct         float64        `json:"coverage_pct"`
	ExercisedByTests    int            `json:"request_fields_set_by_tests"`
	SetByPositiveTests  int            `json:"set_by_positive_tests"`
	SetByNegativeTests  int            `json:"set_by_negative_tests"`
	ByDepth             DepthBands     `json:"by_depth"`
	Representative      RepCoverage    `json:"representative"`
	Covered             []CoveredField `json:"covered"`
	Gaps                []string       `json:"gaps"`
}

// RepCoverage is "representative" field coverage: shared mixin subtrees
// (devrevcopy, custom_fields, ids, ...) collapse to a single coverable unit, so
// one test for a subtree counts rather than demanding every nested leaf. This
// is the default, less-noisy lens; strict per-leaf numbers remain above.
type RepCoverage struct {
	Units          int      `json:"units"`
	Covered        int      `json:"covered"`
	Pct            float64  `json:"pct"`
	CollapsedTypes []string `json:"collapsed_types"`
}

type CoveredField struct {
	Path    string   `json:"path"`
	Match   string   `json:"match"` // exact | alias
	ByTests []string `json:"by_tests"`
}

// DepthBand is field coverage within one request-tree depth band.
type DepthBand struct {
	Total int     `json:"total"`
	Set   int     `json:"set"`
	Pct   float64 `json:"pct"`
}

// DepthBands splits leaf request fields by nesting depth under the request:
// top-level (in.X), nested (in.X.Y), deep (in.X.Y.Z...). Depth also encodes
// validator guard nesting, which the branch metrics use.
type DepthBands struct {
	TopLevel DepthBand `json:"top_level"`
	Nested   DepthBand `json:"nested"`
	Deep     DepthBand `json:"deep"`
}

// BranchCoverage reports outcome-aware coverage of input-gated validation
// branches. Each branch has two arms: present (gating field set, validation
// runs, can fail) and absent (gating field nil, skipped on the happy path).
type BranchCoverage struct {
	Total            int           `json:"total"`
	EnumSwitch       int           `json:"enum_switch_branches"`
	ServerGated      int           `json:"server_gated_branches"`
	HappyPathReached bool          `json:"happy_path_reached"`
	ReachableOnHappy int           `json:"reachable_on_happy_path"`
	PresentArm       int           `json:"present_arm_exercised"`
	PresentArmPct    float64       `json:"present_arm_pct"`
	AbsentArm        int           `json:"absent_arm_exercised"`
	AbsentArmPct     float64       `json:"absent_arm_pct"`
	NegativeArm      int           `json:"negative_arm_exercised"`
	NegativeArmPct   float64       `json:"negative_arm_pct"`
	BothArms         int           `json:"both_arms_exercised"`
	CoveragePct      float64       `json:"coverage_pct"` // present arm over total (headline)
	ByOrigin         BranchOrigins `json:"by_origin"`
	Gaps             []Branch      `json:"gaps"`
}

// BranchArmCounts is outcome-aware branch coverage for one origin bucket.
type BranchArmCounts struct {
	Total            int     `json:"total"`
	ReachableOnHappy int     `json:"reachable_on_happy_path"`
	PresentArm       int     `json:"present_arm_exercised"`
	PresentArmPct    float64 `json:"present_arm_pct"`
	NegativeArm      int     `json:"negative_arm_exercised"`
	NegativeArmPct   float64 `json:"negative_arm_pct"`
	AbsentArm        int     `json:"absent_arm_exercised"`
}

// BranchOrigins splits branches by where the code lives: service (the analyzed
// module's own logic, what we care about most), generated (proto .pb /
// .pb.validator nil-guards), and library (other dependency code). This keeps
// generated validator noise from drowning out real service-logic branches.
type BranchOrigins struct {
	Service   BranchArmCounts `json:"service"`
	Generated BranchArmCounts `json:"generated"`
	Library   BranchArmCounts `json:"library"`
}

func (o *BranchOrigins) bucket(origin string) *BranchArmCounts {
	switch origin {
	case "service":
		return &o.Service
	case "generated":
		return &o.Generated
	default:
		return &o.Library
	}
}

func (c *BranchArmCounts) finalize() {
	c.PresentArmPct = pct(c.PresentArm, c.Total)
	c.NegativeArmPct = pct(c.NegativeArm, c.Total)
}

// branchOrigin classifies a branch by its source location.
func branchOrigin(at string) string {
	if strings.Contains(at, ".pb.validator.go") || strings.Contains(at, ".pb.go") {
		return "generated"
	}
	if strings.Contains(at, "/pkg/mod/") {
		return "library"
	}
	return "service"
}

// httpStatusCodes maps net/http StatusXxx constants to their numeric codes, so
// assertions written as Equal(http.StatusBadRequest) are captured like
// Equal(400).
var httpStatusCodes = map[string]int{
	"StatusContinue":              100,
	"StatusOK":                    200,
	"StatusCreated":               201,
	"StatusAccepted":              202,
	"StatusNoContent":             204,
	"StatusMovedPermanently":      301,
	"StatusFound":                 302,
	"StatusNotModified":           304,
	"StatusBadRequest":            400,
	"StatusUnauthorized":          401,
	"StatusForbidden":             403,
	"StatusNotFound":              404,
	"StatusMethodNotAllowed":      405,
	"StatusConflict":              409,
	"StatusGone":                  410,
	"StatusRequestEntityTooLarge": 413,
	"StatusUnsupportedMediaType":  415,
	"StatusUnprocessableEntity":   422,
	"StatusTooManyRequests":       429,
	"StatusInternalServerError":   500,
	"StatusNotImplemented":        501,
	"StatusBadGateway":            502,
	"StatusServiceUnavailable":    503,
	"StatusGatewayTimeout":        504,
}

func httpStatusCode(name string) (int, bool) {
	code, ok := httpStatusCodes[name]
	return code, ok
}

// isEnumSwitchBranch reports whether a branch's only request-tied conditions are
// operator/enum fields (*Op). These are filter-serialization switches over enum
// values, not input validations, and are reported separately from the validation
// branch denominator.
func isEnumSwitchBranch(b Branch) bool {
	if len(b.PredicatePaths) == 0 {
		return false
	}
	for _, p := range b.PredicatePaths {
		if !strings.HasSuffix(p, "Op") {
			return false
		}
	}
	return true
}

// serverInjectedLeaves are request field names the gateway fills from the auth
// context or internal state; a client never sets them directly.
var serverInjectedLeaves = map[string]bool{
	"DevOid":    true,
	"DevUid":    true,
	"DevUserId": true,
	"DevOrgId":  true,
	"RevOid":    true,
	"RevOrgId":  true,
	"RevUserId": true,
	"ActorId":   true,
}

// isServerGatedBranch reports whether every request-tied condition of a branch is
// a server-injected field. Such a branch can never be satisfied by a test that
// supplies inputs, so it is excluded from the validation branch denominator.
func isServerGatedBranch(b Branch) bool {
	sawNonRoot := false
	for _, p := range b.PredicatePaths {
		if p == "in" {
			continue // the request root is always provided
		}
		sawNonRoot = true
		if !serverInjectedLeaves[lastComponent(p)] {
			return false
		}
	}
	return sawNonRoot
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

// extractLifejacket parses every *_test.go under root and aggregates per-operation
// facts. The enclosing test module is indexed once so request objects handed to
// helpers in other packages can be traced.
func extractLifejacket(root string) (map[string]*opFacts, error) {
	moduleRoot, modulePath := findModuleRoot(root)
	ri := buildRepoIndex(moduleRoot, modulePath)
	fset := token.NewFileSet()
	ops := map[string]*opFacts{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f := (*ast.File)(nil)
		if ri != nil {
			f = ri.fileByPath[path]
		}
		if f == nil {
			var perr error
			f, perr = parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // skip unparseable files, stay deterministic on the rest
			}
		}
		rel := path
		if r, e := filepath.Rel(root, path); e == nil {
			rel = r
		}
		collectTests(f, rel, ops, ri)
		return nil
	})
	return ops, err
}

// collectTests walks a file for Ginkgo It blocks that issue requests.
func collectTests(f *ast.File, file string, ops map[string]*opFacts, ri *repoIndex) {
	parents := buildParents(f)
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
		bodyOps, bodyFields, statuses, success, assertsError := scanItBody(body)
		setup := enclosingSetup(body, parents)
		// Fields are attributed per operation: the fields of the request object
		// that operation's MakeRequest actually sends, traced across the test
		// module (enclosing setup, same-package and cross-package helpers).
		var calledOps []string
		var opFields map[string][]string
		if ri != nil {
			calledOps, opFields = ri.analyzeIt(body, setup, f)
			calledOps = mergeOps(calledOps, bodyOps)
		} else {
			calledOps = bodyOps
			opFields = map[string][]string{}
			for _, op := range bodyOps {
				opFields[op] = bodyFields
			}
		}
		if len(calledOps) == 0 {
			return true // not a request-issuing test
		}
		respFields := scanResponseFields(body)
		sortedResp := sortedUniq(respFields)
		for _, op := range calledOps {
			of := ops[op]
			if of == nil {
				of = &opFacts{Operation: op, fieldsSet: map[string]map[string]bool{}, statuses: map[int]bool{}, respPaths: map[string]map[string]bool{}}
				ops[op] = of
			}
			if success {
				of.success = true
			}
			of.Tests = append(of.Tests, testFact{
				Name: name, File: file, Labels: labels,
				Fields: sortedUniq(opFields[op]), Status: sortedUniqInt(statuses),
				Success: success, AssertsError: assertsError, RespFields: sortedResp,
			})
			for _, fld := range opFields[op] {
				leaf := strings.ToLower(fld)
				if of.fieldsSet[leaf] == nil {
					of.fieldsSet[leaf] = map[string]bool{}
				}
				of.fieldsSet[leaf][name] = true
			}
			for _, rp := range respFields {
				c := canonComponents(rp)
				key := strings.Join(c, ".")
				if key == "" {
					continue
				}
				if of.respPaths[key] == nil {
					of.respPaths[key] = map[string]bool{}
				}
				of.respPaths[key][name] = true
			}
			for _, s := range statuses {
				of.statuses[s] = true
			}
		}
		return true
	})
}

// mergeOps returns the union of two operation lists, preserving first-seen order.
func mergeOps(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, group := range [][]string{a, b} {
		for _, s := range group {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// scanItBody extracts the operations called, request fields set, asserted
// statuses, and success assertions. A single It may issue several requests (an
// action plus verification calls), so every operation invoked is returned, in
// source order and deduped. Request fields come from struct literals AND from
// field assignments on the request variable (req.Field = ..., req.Nested.Field =
// ...), since many tests build nested requests imperatively. Commented-out
// request calls are not part of the AST and so are never seen here; disabled
// specs (XIt/PIt) are not matched as Its.
func scanItBody(body *ast.FuncLit) (ops []string, fields []string, statuses []int, success, assertsError bool) {
	// Pass 1: identify request variables (declared with a request-body type, or
	// used as the request argument of a MakeRequest call). Done first so field
	// assignments are captured whether they precede or follow the call.
	reqVars := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			if isSelName(e.Fun, "MakeRequest") && len(e.Args) >= 3 {
				if root := rootIdent(e.Args[2]); root != "" {
					reqVars[root] = true
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range e.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && requestBodyLit(rhsAt(e, i)) != nil {
					reqVars[id.Name] = true
				}
			}
		case *ast.DeclStmt:
			registerDeclaredReqvars(e, reqVars)
		}
		return true
	})

	// Pass 2: operations, request fields, asserted statuses, success/error.
	hasStatus := false
	sawBeNil := false
	sawError := false
	seen := map[string]bool{}
	// A matcher wrapped in Not/ToNot/NotTo is a negation: Not(BeNil()) asserts an
	// error occurred, Not(HaveOccurred()) asserts success.
	negatedBeNil := map[ast.Node]bool{}
	negatedHaveOccurred := map[ast.Node]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			// MakeRequest(ctx, Client.<Op>, req, &resp)
			if isSelName(e.Fun, "MakeRequest") {
				for _, a := range e.Args {
					if sel, ok := a.(*ast.SelectorExpr); ok && isClientRecv(sel.X) {
						if !seen[sel.Sel.Name] {
							seen[sel.Sel.Name] = true
							ops = append(ops, sel.Sel.Name)
						}
					}
				}
			}
			name := callName(e.Fun)
			if (name == "Not" || name == "ToNot" || name == "NotTo") && len(e.Args) == 1 {
				if inner, ok := e.Args[0].(*ast.CallExpr); ok {
					switch callName(inner.Fun) {
					case "BeNil":
						negatedBeNil[inner] = true
						sawError = true
					case "HaveOccurred":
						negatedHaveOccurred[inner] = true
						sawBeNil = true // Not(HaveOccurred()) asserts success
					}
				}
			}
			switch name {
			case "Equal":
				if len(e.Args) == 1 {
					if n, ok := intLit(e.Args[0]); ok && n >= 100 && n < 600 {
						statuses = append(statuses, n)
					} else if sel, ok := e.Args[0].(*ast.SelectorExpr); ok {
						if code, ok := httpStatusCode(sel.Sel.Name); ok {
							statuses = append(statuses, code)
						}
					}
				}
			case "BeNil":
				if !negatedBeNil[e] {
					sawBeNil = true
				}
			case "HaveOccurred":
				if !negatedHaveOccurred[e] {
					sawError = true
				}
			}
		case *ast.AssignStmt:
			captureAssignedFields(e, reqVars, &fields)
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
	return ops, fields, statuses, sawBeNil && !sawError, sawError
}

// rhsAt returns the right-hand side of an assignment for LHS index i.
func rhsAt(as *ast.AssignStmt, i int) ast.Expr {
	if i < len(as.Rhs) {
		return as.Rhs[i]
	}
	if len(as.Rhs) == 1 {
		return as.Rhs[0]
	}
	return nil
}

// captureAssignedFields records fields assigned to a request variable
// (req.Field = value, including nested req.Nested.Field). Empty values (nil, "",
// 0, false) are skipped, matching the literal path: assigning an empty value does
// not exercise that input.
func captureAssignedFields(as *ast.AssignStmt, reqVars map[string]bool, fields *[]string) {
	for i, lhs := range as.Lhs {
		root, leaf, ok := assignmentTargetLeaf(lhs)
		if !ok || !reqVars[root] {
			continue
		}
		if rhs := rhsAt(as, i); rhs != nil && !isEmptyValue(rhs) {
			*fields = append(*fields, leaf)
		}
	}
}

// registerDeclaredReqvars records `var x RequestBodyType` and
// `var x = RequestBodyType{...}` so later field assignments to x are captured.
func registerDeclaredReqvars(ds *ast.DeclStmt, reqVars map[string]bool) {
	gd, ok := ds.Decl.(*ast.GenDecl)
	if !ok || gd.Tok != token.VAR {
		return
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		isReq := isRequestBodyType(vs.Type)
		if !isReq {
			for _, v := range vs.Values {
				if requestBodyLit(v) != nil {
					isReq = true
					break
				}
			}
		}
		if isReq {
			for _, n := range vs.Names {
				reqVars[n.Name] = true
			}
		}
	}
}

// requestBodyLit unwraps &expr and returns the composite literal when its type
// is a request-body type.
func requestBodyLit(e ast.Expr) *ast.CompositeLit {
	if e == nil {
		return nil
	}
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	if cl, ok := e.(*ast.CompositeLit); ok && isRequestBodyType(cl.Type) {
		return cl
	}
	return nil
}

// assignmentTargetLeaf returns the root identifier and leaf field name for an
// assignment target like req.Field or req.Nested.Field.
func assignmentTargetLeaf(e ast.Expr) (root, leaf string, ok bool) {
	sel, isSel := e.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	path := []string{sel.Sel.Name}
	cur := sel.X
	for {
		switch x := cur.(type) {
		case *ast.SelectorExpr:
			path = append([]string{x.Sel.Name}, path...)
			cur = x.X
		case *ast.ParenExpr:
			cur = x.X
		case *ast.StarExpr:
			cur = x.X
		case *ast.IndexExpr:
			cur = x.X
		case *ast.Ident:
			return x.Name, path[len(path)-1], true
		default:
			return "", "", false
		}
	}
}

// scanResponseFields collects response field paths a test asserts on. The
// response variable is MakeRequest's last argument (&resp); we then gather
// selector chains rooted at it, e.g. resp.Accounts[0].DisplayName -> "Accounts.DisplayName".
func scanResponseFields(body *ast.FuncLit) []string {
	respVars := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !(isSelName(call.Fun, "MakeRequest") || isIdent(call.Fun, "MakeRequest")) || len(call.Args) == 0 {
			return true
		}
		if v := rootIdent(call.Args[len(call.Args)-1]); v != "" {
			respVars[v] = true
		}
		return true
	})
	if len(respVars) == 0 {
		return nil
	}
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if fields, ok := rootedFields(sel, respVars); ok && len(fields) > 0 {
			out = append(out, strings.Join(fields, "."))
		}
		return true
	})
	return out
}

// rootIdent extracts the identifier name from &x or x.
func rootIdent(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.UnaryExpr:
		return rootIdent(x.X)
	case *ast.ParenExpr:
		return rootIdent(x.X)
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// rootedFields returns the field path of a selector chain if it is rooted at a
// response variable, dropping index/deref/paren steps.
func rootedFields(e ast.Expr, roots map[string]bool) ([]string, bool) {
	switch x := e.(type) {
	case *ast.Ident:
		if roots[x.Name] || nameLooksResponse(x.Name) {
			return nil, true
		}
		return nil, false
	case *ast.SelectorExpr:
		f, ok := rootedFields(x.X, roots)
		if !ok {
			return nil, false
		}
		return append(f, x.Sel.Name), true
	case *ast.IndexExpr:
		return rootedFields(x.X, roots)
	case *ast.StarExpr:
		return rootedFields(x.X, roots)
	case *ast.ParenExpr:
		return rootedFields(x.X, roots)
	}
	return nil, false
}

// nameLooksResponse matches the lifejacket convention for response variables
// (listAccountsResp, exportAccountsResp, devUsersListResp), so field accesses
// are captured even when MakeRequest is wrapped in a test helper.
func nameLooksResponse(name string) bool {
	return strings.Contains(strings.ToLower(name), "resp")
}

// buildCoverage intersects provenance for one RPC with aggregated op facts.
func buildCoverage(p *Provenance, of *opFacts, op string, aliases map[string]string, responseAliases map[string]string, statusSpec map[string][]int, sharedTypes map[string]bool) CoverageReport {
	rep := CoverageReport{RPC: p.RPC, Operation: op}
	leaves := leafReadPaths(p)
	rep.Fields.LeafFieldsReadByRPC = len(leaves)
	// Separate operator/enum switch arms from input-validation branches: a branch
	// whose only request-tied conditions are operator fields (*Op) is a filter
	// serialization switch over enum values, not an input validation. One test
	// setting the operator satisfies one arm; counting every arm would drown the
	// validation branches in mechanical noise. They are counted separately.
	branches := make([]Branch, 0, len(p.Branches))
	for _, b := range p.Branches {
		if isEnumSwitchBranch(b) {
			rep.Branches.EnumSwitch++
			continue
		}
		// A branch gated only by server-injected fields (identity/context values
		// the gateway fills, never the client) can never be exercised by a test
		// supplying inputs, so it is excluded from the validation denominator and
		// reported separately.
		if isServerGatedBranch(b) {
			rep.Branches.ServerGated++
			continue
		}
		branches = append(branches, b)
	}
	rep.Branches.Total = len(branches)
	rep.Dependencies = dependencyCoverage(p, of, responseAliases)
	if of == nil {
		rep.Unmapped = true
		rep.Fields.Gaps = leaves
		rep.Fields.ByDepth = depthTotals(leaves)
		rep.Fields.Representative = repCoverage(p, leaves, map[string]bool{}, sharedTypes)
		rep.Branches.Gaps = append([]Branch{}, branches...)
		rep.Branches.ReachableOnHappy = reachableOnHappyCount(branches)
		for _, b := range branches {
			rep.Branches.ByOrigin.bucket(branchOrigin(b.At)).Total++
		}
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

	// Normalized indexes of test-set fields: canonical name -> original -> tests.
	// normAll = every test; normPos = success-asserting tests; normNeg =
	// error-asserting (4xx/5xx) tests. These drive positive vs negative coverage.
	normAll := buildNorm(of.Tests, func(testFact) bool { return true })
	normPos := buildNorm(of.Tests, isPositiveTest)
	normNeg := buildNorm(of.Tests, isNegativeTest)
	// Alias fragments: canonical test-field name -> canonical path components of
	// the mapped RPC field. A value may be a dotted path (e.g.
	// AccountIdentifier.AccountDon) that credits any leaf beneath it.
	aliasFrags := map[string][]string{}
	for k, v := range aliases {
		aliasFrags[canon(k)] = canonComponents(v)
	}

	// Field coverage over RPC leaf paths, bucketed by request-tree depth and
	// split into positive (set by a success test) and negative (set by an error
	// test) input coverage.
	for _, path := range leaves {
		band := bandFor(path, &rep.Fields.ByDepth)
		band.Total++
		if m, ok := matchPath(path, normAll, aliasFrags); ok {
			rep.Fields.Covered = append(rep.Fields.Covered, CoveredField{Path: path, Match: m.kind, ByTests: m.tests})
			band.Set++
			if _, ok := matchPath(path, normPos, aliasFrags); ok {
				rep.Fields.SetByPositiveTests++
			}
			if _, ok := matchPath(path, normNeg, aliasFrags); ok {
				rep.Fields.SetByNegativeTests++
			}
		} else {
			rep.Fields.Gaps = append(rep.Fields.Gaps, path)
		}
	}
	rep.Fields.CoveredCount = len(rep.Fields.Covered)
	rep.Fields.CoveragePct = pct(rep.Fields.CoveredCount, rep.Fields.LeafFieldsReadByRPC)
	finalizeBands(&rep.Fields.ByDepth)
	coveredSet := map[string]bool{}
	for _, cf := range rep.Fields.Covered {
		coveredSet[cf.Path] = true
	}
	rep.Fields.Representative = repCoverage(p, leaves, coveredSet, sharedTypes)

	// Branch coverage (outcome-aware). happy_path_reached is an RPC-level signal:
	// if a success test invoked the RPC at all, the top-level validator guards ran
	// their absent (skip) arm.
	rep.Branches.HappyPathReached = of.success
	for _, b := range branches {
		oc := classifyBranch(b, normAll, normNeg, aliasFrags, rep.Tests)
		ob := rep.Branches.ByOrigin.bucket(branchOrigin(b.At))
		ob.Total++
		if oc.reachableOnHappy {
			rep.Branches.ReachableOnHappy++
			ob.ReachableOnHappy++
		}
		if oc.present {
			rep.Branches.PresentArm++
			ob.PresentArm++
		} else {
			rep.Branches.Gaps = append(rep.Branches.Gaps, b)
		}
		if oc.absent {
			rep.Branches.AbsentArm++
			ob.AbsentArm++
		}
		if oc.negative {
			rep.Branches.NegativeArm++
			ob.NegativeArm++
		}
		if oc.present && oc.absent {
			rep.Branches.BothArms++
		}
	}
	rep.Branches.ByOrigin.Service.finalize()
	rep.Branches.ByOrigin.Generated.finalize()
	rep.Branches.ByOrigin.Library.finalize()
	rep.Branches.PresentArmPct = pct(rep.Branches.PresentArm, rep.Branches.Total)
	rep.Branches.AbsentArmPct = pct(rep.Branches.AbsentArm, rep.Branches.ReachableOnHappy)
	rep.Branches.NegativeArmPct = pct(rep.Branches.NegativeArm, rep.Branches.Total)
	rep.Branches.CoveragePct = rep.Branches.PresentArmPct

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
		if frag, ok := aliasFrags[c]; ok && aliasMatchesAnyLeaf(frag, leaves) {
			continue
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
	infra := make([]int, 0)
	doc := make([]int, 0, len(documented))
	covered := 0
	for _, status := range documented {
		if infraStatuses[status] {
			infra = append(infra, status)
			continue
		}
		doc = append(doc, status)
		if assertedSet[status] {
			covered++
		} else {
			missing = append(missing, status)
		}
	}
	return StatusCoverage{
		Documented:  doc,
		Asserted:    sortedKeysInt(assertedSet),
		Missing:     missing,
		Infra:       infra,
		CoveragePct: pct(covered, len(doc)),
	}
}

type fieldMatch struct {
	kind  string
	tests []string
}

// matchPath resolves an RPC leaf path against test-set fields: exact match on
// the leaf name, then any alias whose mapped path fragment occurs in the path.
func matchPath(path string, normFields map[string]map[string]map[string]bool, aliasFrags map[string][]string) (fieldMatch, bool) {
	if byOrig, ok := normFields[canon(lastComponent(path))]; ok {
		return fieldMatch{kind: "exact", tests: mergeTests(byOrig)}, true
	}
	comps := canonComponents(path)
	for testCanon, frag := range aliasFrags {
		if byOrig, ok := normFields[testCanon]; ok && hasFragment(comps, frag) {
			return fieldMatch{kind: "alias", tests: mergeTests(byOrig)}, true
		}
	}
	return fieldMatch{}, false
}

// aliasMatchesAnyLeaf reports whether an alias fragment occurs in any leaf path.
func aliasMatchesAnyLeaf(frag []string, leaves []string) bool {
	for _, path := range leaves {
		if hasFragment(canonComponents(path), frag) {
			return true
		}
	}
	return false
}

// canonComponents splits a dotted path and canonicalizes each component.
func canonComponents(path string) []string {
	parts := strings.Split(path, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if c := canon(p); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// hasFragment reports whether frag occurs as a contiguous subsequence of comps.
func hasFragment(comps, frag []string) bool {
	if len(frag) == 0 || len(frag) > len(comps) {
		return false
	}
	for i := 0; i+len(frag) <= len(comps); i++ {
		match := true
		for j := range frag {
			if comps[i+j] != frag[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
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

// branchOutcome captures which arms of an input-gated branch tests exercise.
type branchOutcome struct {
	reachableOnHappy bool // guard is top-level, so its absent arm runs on an empty request
	present          bool // a test set every gating field (validation arm runs)
	absent           bool // the skip arm is reached on the happy path
	negative         bool // a gating field is set by an error-asserting test
}

// classifyBranch determines the exercised arms of one branch. A branch is
// reachable-on-happy only when its gating field is top-level (depth 1): deeper
// guards live inside submessage validators that run only if the parent is set,
// which our top-level test-field extraction cannot yet confirm (conservative).
func classifyBranch(b Branch, normAll, normNeg map[string]map[string]map[string]bool, aliasFrags map[string][]string, tests int) branchOutcome {
	if len(b.PredicatePaths) == 0 {
		return branchOutcome{}
	}
	// The request root ("in") is always provided by any test that calls the RPC,
	// so a nil-check on the request itself (in != nil) is satisfied by every
	// linked test. Drop the root and classify on the remaining paths.
	hasRoot := false
	preds := make([]string, 0, len(b.PredicatePaths))
	for _, pth := range b.PredicatePaths {
		if pth == "in" {
			hasRoot = true
			continue
		}
		preds = append(preds, pth)
	}
	if len(preds) == 0 {
		if hasRoot && tests > 0 {
			return branchOutcome{reachableOnHappy: true, present: true}
		}
		return branchOutcome{}
	}
	maxDepth := 0
	present, negative := true, true
	for _, pth := range preds {
		if d := pathDepth(pth); d > maxDepth {
			maxDepth = d
		}
		if _, ok := matchPath(pth, normAll, aliasFrags); !ok {
			present = false
		}
		if _, okn := matchPath(pth, normNeg, aliasFrags); !okn {
			negative = false
		}
	}
	oc := branchOutcome{reachableOnHappy: maxDepth == 1, present: present, negative: negative}
	if oc.reachableOnHappy && tests > 0 {
		// The absent (skip) arm is taken whenever a test invokes the RPC without
		// setting the gating field. If some gating field is left unset by at least
		// one test, the absent arm ran.
		for _, pth := range preds {
			if leafSetCount(pth, normAll, aliasFrags) < tests {
				oc.absent = true
				break
			}
		}
	}
	return oc
}

// leafSetCount returns how many distinct tests set the field at path.
func leafSetCount(path string, normAll map[string]map[string]map[string]bool, aliasFrags map[string][]string) int {
	if m, ok := matchPath(path, normAll, aliasFrags); ok {
		return len(m.tests)
	}
	return 0
}

// buildNorm indexes test-set fields (canonical -> original -> test names) over
// the tests passing keep.
func buildNorm(tests []testFact, keep func(testFact) bool) map[string]map[string]map[string]bool {
	idx := map[string]map[string]map[string]bool{}
	for _, t := range tests {
		if !keep(t) {
			continue
		}
		for _, fld := range t.Fields {
			c := canon(fld)
			orig := strings.ToLower(fld)
			if idx[c] == nil {
				idx[c] = map[string]map[string]bool{}
			}
			if idx[c][orig] == nil {
				idx[c][orig] = map[string]bool{}
			}
			idx[c][orig][t.Name] = true
		}
	}
	return idx
}

func isNegativeTest(t testFact) bool {
	if t.AssertsError {
		return true
	}
	for _, s := range t.Status {
		if s >= 400 {
			return true
		}
	}
	return false
}

func isPositiveTest(t testFact) bool {
	if t.Success {
		return true
	}
	for _, s := range t.Status {
		if s >= 200 && s < 300 {
			return true
		}
	}
	return false
}

// pathDepth is the number of components below the request root (in.X -> 1).
func pathDepth(path string) int {
	if d := len(canonComponents(path)) - 1; d > 0 {
		return d
	}
	return 0
}

// bandFor returns the depth band a leaf path belongs to.
func bandFor(path string, d *DepthBands) *DepthBand {
	switch pathDepth(path) {
	case 1:
		return &d.TopLevel
	case 2:
		return &d.Nested
	default:
		return &d.Deep
	}
}

func finalizeBands(d *DepthBands) {
	d.TopLevel.Pct = pct(d.TopLevel.Set, d.TopLevel.Total)
	d.Nested.Pct = pct(d.Nested.Set, d.Nested.Total)
	d.Deep.Pct = pct(d.Deep.Set, d.Deep.Total)
}

// depthTotals buckets leaves by depth without any coverage (unmapped RPCs).
func depthTotals(leaves []string) DepthBands {
	var d DepthBands
	for _, path := range leaves {
		bandFor(path, &d).Total++
	}
	finalizeBands(&d)
	return d
}

// reachableOnHappyCount counts top-level (depth-1) input-gated branches.
func reachableOnHappyCount(branches []Branch) int {
	n := 0
	for _, b := range branches {
		maxDepth := 0
		for _, pth := range b.PredicatePaths {
			if d := pathDepth(pth); d > maxDepth {
				maxDepth = d
			}
		}
		if maxDepth == 1 {
			n++
		}
	}
	return n
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
// dependencyCoverage summarises the downstream service and datastore calls an
// RPC makes, derived from provenance sinks. of may be nil (no linked test).
func dependencyCoverage(p *Provenance, of *opFacts, responseAliases map[string]string) DependencyCoverage {
	dc := DependencyCoverage{}
	byService := map[string]map[string]bool{}
	db := map[string]bool{}
	for _, s := range p.Sinks {
		switch s.Kind {
		case "rpc":
			svc := serviceFromLocation(s.At)
			if byService[svc] == nil {
				byService[svc] = map[string]bool{}
			}
			if s.Target != "" {
				byService[svc][s.Target] = true
			}
		case "db":
			if s.Target != "" {
				db[s.Target] = true
			}
		}
	}
	for svc, calls := range byService {
		dc.Services = append(dc.Services, ServiceDep{Service: svc, Calls: sortedKeys(calls)})
		dc.CallCount += len(calls)
	}
	sort.Slice(dc.Services, func(i, j int) bool { return dc.Services[i].Service < dc.Services[j].Service })
	dc.DatastoreCalls = sortedKeys(db)
	dc.ServiceCount = len(dc.Services)
	dc.ReachedByTest = of != nil && of.success

	// Response links: dependency results that flow into response fields, and
	// whether a linked test asserts on that field (field-precise verification).
	var respPaths map[string]map[string]bool
	if of != nil {
		respPaths = of.respPaths
	}
	for _, op := range p.OutputPaths {
		link := ResponseLink{Dep: op.Dep, Path: op.Path}
		want := aliasApply(relComponents(op.Path), responseAliases)
		for tp, tests := range respPaths {
			if responsePathMatch(want, canonComponents(tp)) {
				for t := range tests {
					link.CheckedBy = append(link.CheckedBy, t)
				}
			}
		}
		if len(link.CheckedBy) > 0 {
			link.CheckedBy = sortedUniq(link.CheckedBy)
			link.Checked = true
			dc.ResultChecked++
		}
		dc.UsedInResponse++
		dc.ResponseLinks = append(dc.ResponseLinks, link)
	}
	return dc
}

// aliasApply renames each component through a canonical proto->HTTP response
// rename map, so a test asserting the HTTP field name matches a dependency's
// proto response field (e.g. proto "total" vs HTTP "count").
func aliasApply(comps []string, aliases map[string]string) []string {
	if len(aliases) == 0 {
		return comps
	}
	out := make([]string, len(comps))
	for i, c := range comps {
		if r, ok := aliases[c]; ok {
			out[i] = canon(r)
		} else {
			out[i] = c
		}
	}
	return out
}

// relComponents canonicalizes a response path and drops the leading "out" root.
func relComponents(path string) []string {
	c := canonComponents(path)
	if len(c) > 0 && c[0] == "out" {
		c = c[1:]
	}
	return c
}

// responsePathMatch reports whether a test-asserted response path and a
// dependency's response path refer to the same field or one contains the
// other. An empty want (coarse "whole response") matches any assertion.
func responsePathMatch(want, test []string) bool {
	if len(want) == 0 {
		return len(test) > 0
	}
	n := len(want)
	if len(test) < n {
		n = len(test)
	}
	for i := 0; i < n; i++ {
		if want[i] != test[i] {
			return false
		}
	}
	return true
}

// serviceFromLocation derives a downstream service name from a sink source
// location. External calls land in a dependency module path like
// ".../schemaregistry-client@v.../lib/foo.go"; we take the module's base name
// without the "-client" suffix. Calls inside the analyzed service itself fall
// back to "this service".
func serviceFromLocation(at string) string {
	const mod = "/pkg/mod/"
	i := strings.LastIndex(at, mod)
	if i < 0 {
		return "this service"
	}
	rest := at[i+len(mod):]
	// rest looks like "github.com/devrev/schemaregistry-client@ver/lib/..".
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[:at]
	}
	seg := rest
	if j := strings.LastIndex(seg, "/"); j >= 0 {
		seg = seg[j+1:]
	}
	seg = strings.TrimSuffix(seg, "-client")
	if seg == "" {
		return "this service"
	}
	return seg
}

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

// detectSharedTypes flags proto message types reused as internal nodes across
// many RPCs in one run. These are the mixin/shared types (devrevcopy,
// custom_fields, id types, ...) that representative coverage collapses. A
// per-RPC payload object (used by one or few RPCs) stays expanded.
func detectSharedTypes(all []*Provenance) map[string]bool {
	rpcsByType := map[string]map[string]bool{}
	for _, p := range all {
		if p == nil {
			continue
		}
		for t := range internalNodeTypes(p) {
			if rpcsByType[t] == nil {
				rpcsByType[t] = map[string]bool{}
			}
			rpcsByType[t][p.RPC] = true
		}
	}
	k := len(all) * 2 / 100
	if k < 3 {
		k = 3
	}
	shared := map[string]bool{}
	for t, rpcs := range rpcsByType {
		if len(rpcs) >= k {
			shared[t] = true
		}
	}
	return shared
}

// internalNodeTypes returns the types of non-root nodes that have at least one
// deeper read path (i.e. message nodes, not scalar leaves) for one RPC.
func internalNodeTypes(p *Provenance) map[string]bool {
	typeByPath := map[string]string{}
	paths := make([]string, 0, len(p.InputPaths))
	for _, ip := range p.InputPaths {
		typeByPath[ip.Path] = ip.Type
		paths = append(paths, ip.Path)
	}
	out := map[string]bool{}
	for _, pa := range paths {
		if pa == "in" || typeByPath[pa] == "" {
			continue
		}
		for _, q := range paths {
			if len(q) > len(pa) && strings.HasPrefix(q, pa+".") {
				out[typeByPath[pa]] = true
				break
			}
		}
	}
	return out
}

// repCoverage computes representative field coverage: each leaf collapses to the
// outermost ancestor whose type is a shared mixin; non-mixin leaves stay
// individual. A unit is covered if any of its leaves is covered.
func repCoverage(p *Provenance, leaves []string, coveredSet, sharedTypes map[string]bool) RepCoverage {
	typeByPath := map[string]string{}
	for _, ip := range p.InputPaths {
		typeByPath[ip.Path] = ip.Type
	}
	unitCovered := map[string]bool{}
	unitType := map[string]string{}
	for _, leaf := range leaves {
		key, typ := repUnit(leaf, typeByPath, sharedTypes)
		if _, seen := unitCovered[key]; !seen {
			unitCovered[key] = false
			unitType[key] = typ
		}
		if coveredSet[leaf] {
			unitCovered[key] = true
		}
	}
	collapsed := map[string]bool{}
	covered := 0
	for key, ok := range unitCovered {
		if ok {
			covered++
		}
		if t := unitType[key]; t != "" {
			collapsed[t] = true
		}
	}
	return RepCoverage{
		Units:          len(unitCovered),
		Covered:        covered,
		Pct:            pct(covered, len(unitCovered)),
		CollapsedTypes: sortedKeys(collapsed),
	}
}

// repUnit returns the representative unit key for a leaf: the outermost ancestor
// path (closest to the root, excluding the root) whose type is a shared mixin,
// or the leaf itself when none of its ancestors is shared.
func repUnit(leaf string, typeByPath map[string]string, sharedTypes map[string]bool) (string, string) {
	comps := strings.Split(leaf, ".")
	for i := 2; i <= len(comps); i++ { // i=2 skips the bare root "in"
		prefix := strings.Join(comps[:i], ".")
		if i < len(comps) && sharedTypes[typeByPath[prefix]] {
			return prefix, typeByPath[prefix]
		}
	}
	return leaf, ""
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

// isClientRecv reports whether an identifier is a lifejacket API client handle.
// Receivers vary by role and auth (ClientV2Internal, UserClientInternal,
// AdminClientInternal, revUserClient, client, ...), so match any identifier
// whose name contains "client" rather than a single prefix.
func isClientRecv(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && strings.Contains(strings.ToLower(id.Name), "client")
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
	if !ok {
		return false
	}
	// JSON bodies (<Op>JSONRequestBody / <Op>Request) and query/path params
	// (<Op>Params) both carry request fields a test sets.
	n := sel.Sel.Name
	return strings.HasSuffix(n, "RequestBody") || strings.HasSuffix(n, "Request") || strings.HasSuffix(n, "Params")
}

func compositeKeys(c *ast.CompositeLit) []string {
	var out []string
	for _, el := range c.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && !isEmptyValue(kv.Value) {
				out = append(out, id.Name)
			}
		}
	}
	return out
}

// isEmptyValue reports whether a struct-field value is an obviously-empty
// literal ("", 0, false, nil). Listing a field with an empty value does not
// exercise that input, so it must not count toward input coverage.
func isEmptyValue(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		switch v.Kind {
		case token.STRING:
			s, err := strconv.Unquote(v.Value)
			return err == nil && s == ""
		case token.INT, token.FLOAT:
			return v.Value == "0" || v.Value == "0.0"
		}
	case *ast.Ident:
		return v.Name == "nil" || v.Name == "false"
	}
	return false
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
