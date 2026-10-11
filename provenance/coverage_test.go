package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCanon(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "pascal case", in: "DisplayName", want: "displayname"},
		{name: "snake case", in: "display_name", want: "displayname"},
		{name: "mixed case snake", in: "External_Refs", want: "externalrefs"},
		{name: "empty", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canon(tt.in); got != tt.want {
				t.Errorf("canon(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonComponents(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "dotted path", in: "in.AccountIdentifier.AccountDon", want: []string{"in", "accountidentifier", "accountdon"}},
		{name: "empty segments are dropped", in: "in..Account", want: []string{"in", "account"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonComponents(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("canonComponents(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestHasFragment(t *testing.T) {
	comps := []string{"in", "accountidentifier", "accountdon", "components"}
	tests := []struct {
		name  string
		comps []string
		frag  []string
		want  bool
	}{
		{name: "middle fragment", comps: comps, frag: []string{"accountidentifier", "accountdon"}, want: true},
		{name: "trailing fragment", comps: comps, frag: []string{"accountdon", "components"}, want: true},
		{name: "non contiguous fragment", comps: comps, frag: []string{"accountidentifier", "components"}, want: false},
		{name: "single fragment", comps: []string{"in", "account", "artifactids"}, frag: []string{"artifactids"}, want: true},
		{name: "empty fragment", comps: comps, frag: []string{}, want: false},
		{name: "fragment longer than components", comps: comps, frag: []string{"in", "accountidentifier", "accountdon", "components", "extra"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasFragment(tt.comps, tt.frag); got != tt.want {
				t.Errorf("hasFragment(%#v, %#v) = %v, want %v", tt.comps, tt.frag, got, tt.want)
			}
		})
	}
}

func TestCamelSplit(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "two words", in: "AccountsCreate", want: []string{"Accounts", "Create"}},
		{name: "four words", in: "DevUsersGetPost", want: []string{"Dev", "Users", "Get", "Post"}},
		{name: "single word", in: "Get", want: []string{"Get"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := camelSplit(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("camelSplit(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSingularize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "regular plural", in: "Accounts", want: "Account"},
		{name: "ies plural", in: "Companies", want: "Company"},
		{name: "ses plural", in: "Addresses", want: "Address"},
		{name: "already singular", in: "Account", want: "Account"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := singularize(tt.in); got != tt.want {
				t.Errorf("singularize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMapOpToRPC(t *testing.T) {
	rpcSet := map[string]bool{
		"CreateAccount":      true,
		"GetAccount":         true,
		"ListAccounts":       true,
		"CountAccounts":      true,
		"CreateAccountsBulk": true,
		"GetDevUser":         true,
		"ListDevUsers":       true,
	}
	tests := []struct {
		name      string
		op        string
		overrides map[string]string
		want      string
	}{
		{name: "create uses singular resource", op: "AccountsCreate", want: "CreateAccount"},
		{name: "list uses plural resource", op: "AccountsList", want: "ListAccounts"},
		{name: "count uses plural resource", op: "AccountsCount", want: "CountAccounts"},
		{name: "bulk create strips body and moves verb", op: "AccountsBulkCreateWithBody", want: "CreateAccountsBulk"},
		{name: "get post strips post", op: "DevUsersGetPost", want: "GetDevUser"},
		{name: "list post strips post", op: "DevUsersListPost", want: "ListDevUsers"},
		{name: "override", op: "Foo", overrides: map[string]string{"Foo": "Bar"}, want: "Bar"},
		{name: "unknown operation", op: "WidgetsFrobnicate", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapOpToRPC(tt.op, tt.overrides, rpcSet); got != tt.want {
				t.Errorf("mapOpToRPC(%q) = %q, want %q", tt.op, got, tt.want)
			}
		})
	}
}

func TestMatchPath(t *testing.T) {
	normFields := map[string]map[string]map[string]bool{
		"displayname": {"DisplayName": {"t1": true}},
		"id":          {"Id": {"t1": true}},
	}
	tests := []struct {
		name      string
		path      string
		aliases   map[string][]string
		wantKind  string
		wantTests []string
		wantOK    bool
	}{
		{name: "exact leaf", path: "in.Account.DisplayName", wantKind: "exact", wantTests: []string{"t1"}, wantOK: true},
		{name: "unset leaf", path: "in.Account.Unset", wantOK: false},
		{name: "alias fragment", path: "in.AccountIdentifier.AccountDon.Components", aliases: map[string][]string{"id": canonComponents("AccountIdentifier.AccountDon")}, wantKind: "alias", wantTests: []string{"t1"}, wantOK: true},
		{name: "alias does not match other path", path: "in.Other.Field", aliases: map[string][]string{"id": canonComponents("AccountIdentifier.AccountDon")}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchPath(tt.path, normFields, tt.aliases)
			if ok != tt.wantOK {
				t.Errorf("matchPath(%q) ok = %v, want %v", tt.path, ok, tt.wantOK)
			}
			if ok && got.kind != tt.wantKind {
				t.Errorf("matchPath(%q) kind = %q, want %q", tt.path, got.kind, tt.wantKind)
			}
			if ok && !reflect.DeepEqual(got.tests, tt.wantTests) {
				t.Errorf("matchPath(%q) tests = %#v, want %#v", tt.path, got.tests, tt.wantTests)
			}
		})
	}
}

func TestStemAndFuzzyRelated(t *testing.T) {
	if got, want := stem("artifacts"), stem("artifactids"); got != want {
		t.Errorf("stem artifacts = %q, stem artifactids = %q, want equal", got, want)
	}
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "substring", a: "schemafragmentids", b: "customschemafragmentids", want: true},
		{name: "short token is ignored", a: "id", b: "stageid", want: false},
		{name: "shared stem", a: "artifacts", b: "artifactids", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fuzzyRelated(tt.a, tt.b); got != tt.want {
				t.Errorf("fuzzyRelated(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestParseStatusSpecAndSuccessCodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.go")
	contents := `package gen

type AccountsGetResponse struct {
	JSON200 *X
	JSON400 *Y
	JSON403 *Z
}
type AccountsCreateResponse struct {
	JSON201 *X
	JSON400 *Y
}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write status spec: %v", err)
	}
	got, err := parseStatusSpec(path)
	if err != nil {
		t.Fatalf("parseStatusSpec: %v", err)
	}
	want := map[string][]int{
		"AccountsGet":    {200, 400, 403},
		"AccountsCreate": {201, 400},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseStatusSpec(%q) = %#v, want %#v", path, got, want)
	}

	tests := []struct {
		name  string
		codes []int
		want  []int
	}{
		{name: "get success status", codes: []int{200, 400, 403}, want: []int{200}},
		{name: "create success status", codes: []int{201, 400}, want: []int{201}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := successCodes(tt.codes); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("successCodes(%#v) = %#v, want %#v", tt.codes, got, tt.want)
			}
		})
	}
}

func TestPathDepth(t *testing.T) {
	cases := map[string]int{
		"in.SearchQuery":                   1,
		"in.PageParams.Page":               2,
		"in.UserFilter.DevUserFilter.Apps": 3,
		"in":                               0,
	}
	for path, want := range cases {
		if got := pathDepth(path); got != want {
			t.Errorf("pathDepth(%q)=%d want %d", path, got, want)
		}
	}
}

func TestDepthBandsBucketing(t *testing.T) {
	leaves := []string{"in.A", "in.B", "in.X.Y", "in.P.Q.R", "in.P.Q.S"}
	d := depthTotals(leaves)
	if d.TopLevel.Total != 2 || d.Nested.Total != 1 || d.Deep.Total != 2 {
		t.Fatalf("bands = top %d nested %d deep %d", d.TopLevel.Total, d.Nested.Total, d.Deep.Total)
	}
}

func TestPositiveNegativeTest(t *testing.T) {
	cases := []struct {
		name     string
		tf       testFact
		positive bool
		negative bool
	}{
		{"success bool", testFact{Success: true}, true, false},
		{"200", testFact{Status: []int{200}}, true, false},
		{"400", testFact{Status: []int{400}}, false, true},
		{"201 and 400", testFact{Status: []int{201, 400}}, true, true},
		{"none", testFact{}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPositiveTest(c.tf); got != c.positive {
				t.Errorf("isPositiveTest=%v want %v", got, c.positive)
			}
			if got := isNegativeTest(c.tf); got != c.negative {
				t.Errorf("isNegativeTest=%v want %v", got, c.negative)
			}
		})
	}
}

func TestBuildNormFilters(t *testing.T) {
	tests := []testFact{
		{Name: "ok", Fields: []string{"DisplayName"}, Status: []int{200}, Success: true},
		{Name: "bad", Fields: []string{"Id"}, Status: []int{400}},
	}
	neg := buildNorm(tests, isNegativeTest)
	if _, ok := neg["id"]; !ok {
		t.Errorf("expected negative index to contain id, got %v", neg)
	}
	if _, ok := neg["displayname"]; ok {
		t.Errorf("positive-only field displayname should not be in negative index")
	}
}

func TestClassifyBranch(t *testing.T) {
	// Id set by both a success (200) and an error (400) test at top level.
	tests := []testFact{
		{Name: "ok", Fields: []string{"Id"}, Status: []int{200}, Success: true},
		{Name: "bad", Fields: []string{"Id"}, Status: []int{400}},
	}
	normAll := buildNorm(tests, func(testFact) bool { return true })
	normNeg := buildNorm(tests, isNegativeTest)

	t.Run("top-level set by positive and negative", func(t *testing.T) {
		b := Branch{PredicatePaths: []string{"in.Id"}}
		oc := classifyBranch(b, normAll, normNeg, nil, 2)
		if !oc.reachableOnHappy || !oc.present || !oc.negative {
			t.Fatalf("got %+v", oc)
		}
	})
	t.Run("deep guard not reachable on happy and unset", func(t *testing.T) {
		b := Branch{PredicatePaths: []string{"in.Filter.Deep.Field"}}
		oc := classifyBranch(b, normAll, normNeg, nil, 2)
		if oc.reachableOnHappy || oc.present || oc.absent {
			t.Fatalf("deep unset guard should be inert, got %+v", oc)
		}
	})
	t.Run("top-level unset guard: absent arm on happy path", func(t *testing.T) {
		b := Branch{PredicatePaths: []string{"in.SkipTotal"}}
		oc := classifyBranch(b, normAll, normNeg, nil, 2)
		if !oc.reachableOnHappy || oc.present || !oc.absent {
			t.Fatalf("got %+v", oc)
		}
	})
	t.Run("no predicate paths", func(t *testing.T) {
		if oc := classifyBranch(Branch{}, normAll, normNeg, nil, 2); oc.present || oc.absent || oc.negative {
			t.Fatalf("empty branch should be inert, got %+v", oc)
		}
	})
}

// TestScanItBodyCollectsEveryOperation guards the fix for tests that issue an
// action plus verification requests: every operation the It calls must be
// returned (not just the last), deduped, and in source order.
func TestScanItBodyCollectsEveryOperation(t *testing.T) {
	src := `package x

func _() {
	It("activate then verify", func() {
		req := apiv2internal.DevUsersActivateJSONRequestBody{Id: "u1"}
		http_utils.MakeRequest(ctx, clientV2Internal.DevUsersActivate, req, &resp)
		http_utils.MakeRequest(ctx, clientV2Internal.DevUsersGetPost, getReq, &getResp)
		// verification is repeated in many tests; repeating is fine.
		http_utils.MakeRequest(ctx, clientV2Internal.DevUsersGetPost, getReq, &getResp)
	})
}`
	body := parseFirstItBody(t, src)
	ops, _, _, _ := scanItBody(body)
	want := []string{"DevUsersActivate", "DevUsersGetPost"}
	if !reflect.DeepEqual(ops, want) {
		t.Fatalf("scanItBody ops = %v, want %v", ops, want)
	}
}

// TestCollectTestsLinksAllCalledOperations is the end-to-end check: a test that
// acts then verifies must be linked to BOTH operations, and a commented-out
// request must not create a link.
func TestCollectTestsLinksAllCalledOperations(t *testing.T) {
	src := `package x

func _() {
	It("activate then verify", func() {
		req := apiv2internal.DevUsersActivateJSONRequestBody{Id: "u1"}
		http_utils.MakeRequest(ctx, clientV2Internal.DevUsersActivate, req, &resp)
		http_utils.MakeRequest(ctx, clientV2Internal.DevUsersGetPost, getReq, &getResp)
	})
	It("disabled", func() {
		// http_utils.MakeRequest(ctx, clientV2Internal.DevUsersDelete, req, &resp)
	})
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x_test.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]*opFacts{}
	collectTests(f, "x_test.go", ops, nil)

	for _, op := range []string{"DevUsersActivate", "DevUsersGetPost"} {
		of := ops[op]
		if of == nil {
			t.Fatalf("operation %s was not linked", op)
		}
		if len(of.Tests) != 1 {
			t.Fatalf("operation %s: got %d tests, want 1", op, len(of.Tests))
		}
	}
	if ops["DevUsersDelete"] != nil {
		t.Fatalf("commented-out request must not create a link")
	}
}

func parseFirstItBody(t *testing.T, src string) *ast.FuncLit {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x_test.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.FuncLit
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isIdent(call.Fun, "It") {
			return true
		}
		for _, a := range call.Args {
			if fl, ok := a.(*ast.FuncLit); ok && body == nil {
				body = fl
			}
		}
		return true
	})
	if body == nil {
		t.Fatal("no It body found")
	}
	return body
}

// TestScanItBodyCapturesAssignedFields guards capture of request fields set by
// assignment (req.Field = ...) rather than struct literals. Empty assignments
// (Stage = nil) must not count.
func TestScanItBodyCapturesAssignedFields(t *testing.T) {
	src := `package x

func _() {
	It("build then call", func() {
		req := apiv2internal.WorksCountPostJSONRequestBody{Type: &types}
		req.Stage = nil
		req.Ticket.Subtype = &sub
		req.Ticket.SubtypeOp = &op
		req.Filter.TaskFilter.Priority = &p
		http_utils.MakeRequest(ctx, ClientV2.WorksCountPost, req, &resp)
	})
}`
	body := parseFirstItBody(t, src)
	ops, fields, _, _ := scanItBody(body)
	if len(ops) != 1 || ops[0] != "WorksCountPost" {
		t.Fatalf("ops = %v, want [WorksCountPost]", ops)
	}
	got := map[string]bool{}
	for _, f := range fields {
		got[f] = true
	}
	for _, want := range []string{"Type", "Subtype", "SubtypeOp", "Priority"} {
		if !got[want] {
			t.Errorf("missing assigned field %q; got %v", want, fields)
		}
	}
	if got["Stage"] {
		t.Errorf("empty assignment (Stage = nil) must not count; got %v", fields)
	}
}

// TestTracedRequestFieldsFromSetupAndHelper covers tracing the request object
// passed to MakeRequest across the enclosing BeforeEach and a same-package
// helper that takes it by pointer, and confirms fields on a different object are
// not attributed.
func TestTracedRequestFieldsFromSetupAndHelper(t *testing.T) {
	dir := t.TempDir()
	src := `package x

func buildReq(req *apiv2.WorksCountPostJSONRequestBody) {
	req.Ticket.Subtype = &sub
	req.Filter.WorkType = &wt
}

var _ = Describe("count", func() {
	var req apiv2.WorksCountPostJSONRequestBody
	var other apiv2.WorksCountPostJSONRequestBody

	BeforeEach(func() {
		req = apiv2.WorksCountPostJSONRequestBody{Type: &types}
		other = apiv2.WorksCountPostJSONRequestBody{Unrelated: &u}
		buildReq(&req)
	})

	It("counts", func() {
		http_utils.MakeRequest(ctx, ClientV2.WorksCountPost, req, &resp)
	})
})
`
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ops, err := extractLifejacket(dir)
	if err != nil {
		t.Fatal(err)
	}
	of := ops["WorksCountPost"]
	if of == nil {
		t.Fatal("WorksCountPost was not linked")
	}
	got := map[string]bool{}
	for _, tf := range of.Tests {
		for _, f := range tf.Fields {
			got[f] = true
		}
	}
	for _, want := range []string{"Type", "Subtype", "WorkType"} {
		if !got[want] {
			t.Errorf("missing traced field %q; got %v", want, got)
		}
	}
	if got["Unrelated"] {
		t.Errorf("fields of an unrelated object must not be attributed; got %v", got)
	}
}

// TestTracedRequestFieldsAcrossPackages covers discovering the operation a
// cross-package helper issues and the fields set on the request object it is
// handed.
func TestTracedRequestFieldsAcrossPackages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/t\n\ngo 1.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	helperDir := filepath.Join(dir, "helper")
	if err := os.MkdirAll(helperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	helper := `package helper

func Do(ctx context.Context, req *pkg.WorksCountPostJSONRequestBody) {
	http_utils.MakeRequest(ctx, client.WorksCountPost, req, &resp)
}
`
	if err := os.WriteFile(filepath.Join(helperDir, "helper.go"), []byte(helper), 0o644); err != nil {
		t.Fatal(err)
	}
	test := `package t

import helper "example.com/t/helper"

var _ = Describe("d", func() {
	var req pkg.WorksCountPostJSONRequestBody

	BeforeEach(func() {
		req = pkg.WorksCountPostJSONRequestBody{Type: &types}
	})

	It("counts", func() {
		helper.Do(ctx, req)
	})
})
`
	if err := os.WriteFile(filepath.Join(dir, "t_test.go"), []byte(test), 0o644); err != nil {
		t.Fatal(err)
	}
	ops, err := extractLifejacket(dir)
	if err != nil {
		t.Fatal(err)
	}
	of := ops["WorksCountPost"]
	if of == nil {
		t.Fatal("WorksCountPost was not discovered via the cross-package helper")
	}
	got := map[string]bool{}
	for _, tf := range of.Tests {
		for _, f := range tf.Fields {
			got[f] = true
		}
	}
	if !got["Type"] {
		t.Errorf("missing traced field Type from enclosing setup; got %v", got)
	}
}
