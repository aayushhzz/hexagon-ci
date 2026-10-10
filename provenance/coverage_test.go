package main

import (
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
