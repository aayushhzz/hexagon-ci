package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Cross-package request-object tracing.
//
// A test frequently builds its request object outside the It body and hands it
// to a helper (often in another package) that eventually issues the MakeRequest.
// Example: an It calls workflows/works.EventuallyVerifyWorksCount(ctx, req, ...),
// which calls handler.CountWorks(ctx, req), which calls
// MakeRequest(ctx, client.WorksCountPost, req, ...).
//
// The tracer follows THAT object only: it resolves each call (same package,
// cross package via imports, and single-implementation methods), binds the
// callee's parameter to the object, and records (a) fields assigned to the
// object and (b) the operation of any MakeRequest whose request argument is the
// object. Fields set on some other object, and operations reached without the
// object, are never attributed. When a call cannot be resolved unambiguously the
// trace stops (conservative under-approximation).

const reqTraceMaxDepth = 10

// scope is a block together with the file it came from (for import resolution).
type scope struct {
	body *ast.BlockStmt
	file *ast.File
}

// repoIndex indexes the test module so same-package and cross-package calls can
// be resolved from the AST alone.
type repoIndex struct {
	moduleRoot  string
	modulePath  string
	pkgByDir    map[string]*pkgInfo
	dirByImport map[string]string
	fileDir     map[*ast.File]string
	fileImports map[*ast.File]map[string]string
	fileByFunc  map[*ast.FuncDecl]*ast.File
	fileByPath  map[string]*ast.File
	methods     map[string][]*ast.FuncDecl
}

type pkgInfo struct {
	dir   string
	funcs map[string]*ast.FuncDecl
}

// buildRepoIndex parses every .go file under moduleRoot and indexes packages,
// functions, methods and imports. Returns nil when there is nothing to index.
func buildRepoIndex(moduleRoot, modulePath string) *repoIndex {
	if moduleRoot == "" {
		return nil
	}
	ri := &repoIndex{
		moduleRoot:  moduleRoot,
		modulePath:  modulePath,
		pkgByDir:    map[string]*pkgInfo{},
		dirByImport: map[string]string{},
		fileDir:     map[*ast.File]string{},
		fileImports: map[*ast.File]map[string]string{},
		fileByFunc:  map[*ast.FuncDecl]*ast.File{},
		fileByPath:  map[string]*ast.File{},
		methods:     map[string][]*ast.FuncDecl{},
	}
	fset := token.NewFileSet()
	_ = filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if path != moduleRoot && (base == "vendor" || base == "testdata" ||
				base == "_build" || base == "node_modules" || strings.HasPrefix(base, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		dir := filepath.Dir(path)
		ri.fileByPath[path] = f
		ri.fileDir[f] = dir
		ri.fileImports[f] = fileImportAliases(f)
		if modulePath != "" {
			if rel, e := filepath.Rel(moduleRoot, dir); e == nil {
				imp := modulePath
				if rel != "." {
					imp = modulePath + "/" + filepath.ToSlash(rel)
				}
				ri.dirByImport[imp] = dir
			}
		}
		pi := ri.pkgByDir[dir]
		if pi == nil {
			pi = &pkgInfo{dir: dir, funcs: map[string]*ast.FuncDecl{}}
			ri.pkgByDir[dir] = pi
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ri.fileByFunc[fd] = f
			if fd.Recv != nil {
				ri.methods[fd.Name.Name] = append(ri.methods[fd.Name.Name], fd)
			} else if _, exists := pi.funcs[fd.Name.Name]; !exists {
				pi.funcs[fd.Name.Name] = fd
			}
		}
		return nil
	})
	return ri
}

// findModuleRoot walks up from root to the directory holding go.mod and returns
// (root, module path). When no go.mod is found it returns (root, ""), which still
// allows same-package resolution.
func findModuleRoot(root string) (string, string) {
	dir := root
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "module ") {
					return dir, strings.TrimSpace(strings.TrimPrefix(line, "module "))
				}
			}
			return dir, ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return root, ""
		}
		dir = parent
	}
}

func fileImportAliases(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		alias := ""
		if spec.Name != nil {
			alias = spec.Name.Name
		} else if i := strings.LastIndex(p, "/"); i >= 0 {
			alias = p[i+1:]
		} else {
			alias = p
		}
		if alias == "_" || alias == "." || alias == "" {
			continue
		}
		out[alias] = p
	}
	return out
}

// resolveCall maps a call expression to a function/method declaration.
//   - bare identifier: a function in the same package as the file
//   - alias.Name: a function in the imported package
//   - expr.Name: a method, resolved only when exactly one implementation exists
//
// Ambiguous or unknown callees return nil (the trace stops there).
func (ri *repoIndex) resolveCall(call *ast.CallExpr, file *ast.File) *ast.FuncDecl {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		if dir := ri.fileDir[file]; dir != "" {
			if pi := ri.pkgByDir[dir]; pi != nil {
				return pi.funcs[f.Name]
			}
		}
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			if imp := ri.fileImports[file][id.Name]; imp != "" {
				if dir, ok := ri.dirByImport[imp]; ok {
					if pi := ri.pkgByDir[dir]; pi != nil {
						return pi.funcs[f.Sel.Name]
					}
				}
				return nil
			}
		}
		if cands := ri.methods[f.Sel.Name]; len(cands) == 1 {
			return cands[0]
		}
	}
	return nil
}

// traceIt returns the operations discovered for an It by following the request
// objects it starts (identifiers it passes to calls and sends to MakeRequest),
// together with the fields set on those objects. Each object is traced from the
// test scope, so fields set in the It or its enclosing setup are captured, and
// followed through same-package and cross-package helpers, which both discovers
// the operation a helper issues and any fields it sets on the object.
//
// A root object's fields are kept only when that object actually reaches a
// MakeRequest, so an object that leads nowhere (a context, a handler, an unused
// variable) contributes neither fields nor attribution.
func (ri *repoIndex) traceIt(itBody *ast.FuncLit, setup []*ast.BlockStmt, file *ast.File) (ops, fields []string) {
	if ri == nil {
		return nil, nil
	}
	scopes := make([]scope, 0, len(setup)+1)
	scopes = append(scopes, scope{body: itBody.Body, file: file})
	for _, b := range setup {
		scopes = append(scopes, scope{body: b, file: file})
	}

	opsSet := map[string]bool{}
	// Direct MakeRequest calls record their operation.
	for _, call := range findMakeRequests(itBody) {
		if op := makeRequestOp(call); op != "" {
			opsSet[op] = true
		}
	}

	// Root objects: the request argument of every direct MakeRequest, and every
	// identifier argument passed to a call in the test scope (a helper may be
	// sent the request object and issue the MakeRequest itself).
	roots := map[string]bool{}
	for _, call := range findMakeRequests(itBody) {
		if base := requestArgIdent(call); base != "" {
			roots[base] = true
		}
	}
	for _, sc := range scopes {
		ast.Inspect(sc.body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, a := range call.Args {
				if nm := rootIdent(a); nm != "" {
					roots[nm] = true
				}
			}
			return true
		})
	}
	names := make([]string, 0, len(roots))
	for r := range roots {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, name := range names {
		tr := &reqTracer{ri: ri, visited: map[string]bool{}, ops: map[string]bool{}}
		tr.traceObject(name, scopes, 0)
		if len(tr.ops) == 0 {
			continue // object leads to no request; its fields are not attributed
		}
		for op := range tr.ops {
			opsSet[op] = true
		}
		fields = append(fields, tr.fields...)
	}

	ops = make([]string, 0, len(opsSet))
	for op := range opsSet {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return ops, fields
}

type reqTracer struct {
	ri      *repoIndex
	visited map[string]bool
	ops     map[string]bool
	fields  []string
}

// traceObject gathers fields set on the named object across the given scopes and,
// transitively, helpers it is passed to. A MakeRequest whose request argument is
// the object records the operation.
func (t *reqTracer) traceObject(name string, scopes []scope, depth int) {
	if depth > reqTraceMaxDepth {
		return
	}
	for _, sc := range scopes {
		ast.Inspect(sc.body, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.AssignStmt:
				t.assignFields(e, name)
			case *ast.DeclStmt:
				t.declFields(e, name)
			case *ast.CallExpr:
				if isSelName(e.Fun, "MakeRequest") {
					if requestArgIdent(e) == name {
						if op := makeRequestOp(e); op != "" {
							t.ops[op] = true
						}
					}
					return true
				}
				fd := t.ri.resolveCall(e, sc.file)
				if fd == nil {
					return true
				}
				for i, a := range e.Args {
					if rootIdent(a) != name {
						continue
					}
					p := paramNameAt(fd, i)
					if p == "" {
						continue
					}
					key := fnKey(fd) + "|" + p
					if t.visited[key] {
						continue
					}
					t.visited[key] = true
					t.traceObject(p, []scope{{body: fd.Body, file: t.ri.fileByFunc[fd]}}, depth+1)
				}
			}
			return true
		})
	}
}

func (t *reqTracer) assignFields(as *ast.AssignStmt, name string) {
	for i, lhs := range as.Lhs {
		if id, ok := lhs.(*ast.Ident); ok {
			if id.Name == name {
				if lit := requestBodyLit(rhsAt(as, i)); lit != nil {
					t.fields = append(t.fields, compositeKeys(lit)...)
				}
			}
			continue
		}
		root, leaf, ok := assignmentTargetLeaf(lhs)
		if !ok || root != name {
			continue
		}
		if rhs := rhsAt(as, i); rhs != nil && !isEmptyValue(rhs) {
			t.fields = append(t.fields, leaf)
		}
	}
}

func (t *reqTracer) declFields(ds *ast.DeclStmt, name string) {
	gd, ok := ds.Decl.(*ast.GenDecl)
	if !ok {
		return
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, n := range vs.Names {
			if n.Name != name {
				continue
			}
			var rhs ast.Expr
			if i < len(vs.Values) {
				rhs = vs.Values[i]
			} else if len(vs.Values) == 1 {
				rhs = vs.Values[0]
			}
			if lit := requestBodyLit(rhs); lit != nil {
				t.fields = append(t.fields, compositeKeys(lit)...)
			}
		}
	}
}

// findMakeRequests returns every MakeRequest call inside a function literal.
func findMakeRequests(body *ast.FuncLit) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && isSelName(c.Fun, "MakeRequest") {
			out = append(out, c)
		}
		return true
	})
	return out
}

// requestArgIdent returns the base identifier of the request argument of a
// MakeRequest(ctx, Client.Op, req, ...) call, or "" when it is not a local
// identifier (inline literal, external call, etc.).
func requestArgIdent(call *ast.CallExpr) string {
	if len(call.Args) < 3 {
		return ""
	}
	return rootIdent(call.Args[2])
}

// makeRequestOp returns the operation name of a MakeRequest call: the last
// component of the client selector passed as the second argument.
func makeRequestOp(call *ast.CallExpr) string {
	if len(call.Args) < 2 {
		return ""
	}
	if sel, ok := call.Args[1].(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	return ""
}

// paramNameAt returns the index-th parameter name of a function declaration,
// counting grouped names (a, b T) in order. Unnamed parameters yield "".
func paramNameAt(fd *ast.FuncDecl, idx int) string {
	i := 0
	for _, f := range fd.Type.Params.List {
		if len(f.Names) == 0 {
			if i == idx {
				return ""
			}
			i++
			continue
		}
		for _, n := range f.Names {
			if i == idx {
				return n.Name
			}
			i++
		}
	}
	return ""
}

func fnKey(fd *ast.FuncDecl) string {
	if fd == nil {
		return ""
	}
	return fd.Name.Name
}

// buildParents maps every AST node to its parent within a file, used to find the
// enclosing Describe scopes of an It.
func buildParents(f *ast.File) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return true
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return parents
}

// enclosingSetup returns the BeforeEach/JustBeforeEach bodies of every enclosing
// Describe/Context of an It, since those run before the It and may build the
// request object it uses.
func enclosingSetup(it *ast.FuncLit, parents map[ast.Node]ast.Node) []*ast.BlockStmt {
	var out []*ast.BlockStmt
	for n := parents[it]; n != nil; n = parents[n] {
		fl, ok := n.(*ast.FuncLit)
		if !ok {
			continue
		}
		for _, st := range fl.Body.List {
			expr, ok := st.(*ast.ExprStmt)
			if !ok {
				continue
			}
			c, ok := expr.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			if !(isIdent(c.Fun, "BeforeEach") || isIdent(c.Fun, "JustBeforeEach")) {
				continue
			}
			for _, a := range c.Args {
				if b, ok := a.(*ast.FuncLit); ok {
					out = append(out, b.Body)
				}
			}
		}
	}
	return out
}
