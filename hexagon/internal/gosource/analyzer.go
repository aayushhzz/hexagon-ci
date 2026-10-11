// Package gosource parses Go source into call graph facts without executing
// or type-checking repository code.
package gosource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	gobuild "go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type node struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Qualname string `json:"qualname"`
	Line     int    `json:"line"`
	EndLine  int    `json:"end_line,omitempty"`
	IsTest   bool   `json:"is_test"`
}

type edge struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
}

type unresolvedCall struct {
	Caller     string `json:"caller"`
	Expression string `json:"expression"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
}

type output struct {
	Nodes           []node           `json:"nodes"`
	Edges           []edge           `json:"edges"`
	UnresolvedCalls []unresolvedCall `json:"unresolved_calls"`
	Files           []fileFact       `json:"files"`
	BuildContext    buildContext     `json:"build_context"`
}

type fileFact struct {
	Path    string   `json:"path"`
	Package string   `json:"package"`
	Imports []string `json:"imports"`
}

type buildContext struct {
	GOOS       string   `json:"goos"`
	GOARCH     string   `json:"goarch"`
	GoVersion  string   `json:"go_version"`
	Compiler   string   `json:"compiler"`
	CgoEnabled bool     `json:"cgo_enabled"`
	BuildTags  []string `json:"build_tags"`
}

type sourceFile struct {
	Path    string
	Package string
	Imports map[string]string
	AST     *ast.File
}

type function struct {
	Node         node
	Package      string
	ReceiverType string
	ReceiverVar  string
	Shadowed     map[string]bool
	Bindings     map[string]typeRef
	File         *sourceFile
	Decl         *ast.FuncDecl
}

type typeRef struct {
	Package string
	Name    string
}

type resolver struct {
	index      map[string][]string
	packages   map[string]map[string]bool
	types      map[string]map[string]bool
	module     string
	returns    map[string][]typeRef
	rpcServers map[typeRef][]typeRef
}

func packageKey(filename, packageName string) string {
	return path.Dir(filename) + "::" + packageName
}

func modulePath(mod []byte) string {
	for _, line := range strings.Split(string(mod), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), "\"")
		}
	}
	return ""
}

func receiver(decl *ast.FuncDecl) (string, string) {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return "", ""
	}
	field := decl.Recv.List[0]
	name := ""
	if len(field.Names) > 0 {
		name = field.Names[0].Name
	}
	typ := field.Type
	for {
		switch value := typ.(type) {
		case *ast.StarExpr:
			typ = value.X
		case *ast.IndexExpr:
			typ = value.X
		case *ast.IndexListExpr:
			typ = value.X
		default:
			ident, ok := typ.(*ast.Ident)
			if ok {
				return ident.Name, name
			}
			return "", name
		}
	}
}

func isTest(decl *ast.FuncDecl, filename string, imports map[string]string) bool {
	name := decl.Name.Name
	if !strings.HasSuffix(filename, "_test.go") || decl.Recv != nil ||
		!strings.HasPrefix(name, "Test") || len(name) <= len("Test") {
		return false
	}
	first, _ := utf8.DecodeRuneInString(strings.TrimPrefix(name, "Test"))
	if unicode.IsLower(first) || decl.Type.Params == nil || len(decl.Type.Params.List) != 1 {
		return false
	}
	parameter := decl.Type.Params.List[0]
	if len(parameter.Names) != 1 {
		return false
	}
	pointer, ok := parameter.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "T" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	return ok && imports[qualifier.Name] == "testing"
}

func importsFor(file *ast.File) map[string]string {
	imports := make(map[string]string)
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		alias := path.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if alias != "_" && alias != "." {
			imports[alias] = importPath
		}
	}
	return imports
}

func unique(index map[string][]string, packageName, name string) string {
	targets := index[packageName+"|"+name]
	if len(targets) == 1 {
		return targets[0]
	}
	return ""
}

func localImport(module, importPath string, packages map[string]map[string]bool) string {
	directory := ""
	switch {
	case module == "" || importPath == "":
		return ""
	case importPath == module:
		directory = "."
	case strings.HasPrefix(importPath, module+"/"):
		directory = strings.TrimPrefix(importPath, module+"/")
	default:
		return ""
	}
	var selected string
	for candidate := range packages[directory] {
		if strings.HasSuffix(candidate, "_test") {
			continue
		}
		if selected != "" {
			return "" // Multiple package variants: do not guess a target.
		}
		selected = candidate
	}
	if selected == "" {
		return ""
	}
	return directory + "::" + selected
}

func unwrap(expr ast.Expr) ast.Expr {
	for {
		switch value := expr.(type) {
		case *ast.IndexExpr:
			expr = value.X
		case *ast.IndexListExpr:
			expr = value.X
		case *ast.ParenExpr:
			expr = value.X
		default:
			return expr
		}
	}
}

// Treat any local declaration as a possible shadow throughout the function.
// This may leave an otherwise resolvable call unresolved, but avoids inventing
// an edge to a package symbol when a local binding has the same name.
func shadowedNames(decl *ast.FuncDecl) map[string]bool {
	names := make(map[string]bool)
	addFields := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			for _, identifier := range field.Names {
				names[identifier.Name] = true
			}
		}
	}
	addIdent := func(expr ast.Expr) {
		if identifier, ok := expr.(*ast.Ident); ok && identifier.Name != "_" {
			names[identifier.Name] = true
		}
	}
	addFields(decl.Type.Params)
	addFields(decl.Type.Results)
	if decl.Body != nil {
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			switch item := n.(type) {
			case *ast.AssignStmt:
				if item.Tok == token.DEFINE {
					for _, expr := range item.Lhs {
						addIdent(expr)
					}
				}
			case *ast.RangeStmt:
				if item.Tok == token.DEFINE {
					addIdent(item.Key)
					addIdent(item.Value)
				}
			case *ast.ValueSpec:
				for _, identifier := range item.Names {
					names[identifier.Name] = true
				}
			case *ast.FuncLit:
				addFields(item.Type.Params)
				addFields(item.Type.Results)
			}
			return true
		})
	}
	return names
}

func (r resolver) declaredType(expr ast.Expr, fn function) typeRef {
	for {
		switch value := expr.(type) {
		case *ast.StarExpr:
			expr = value.X
		case *ast.IndexExpr:
			expr = value.X
		case *ast.IndexListExpr:
			expr = value.X
		default:
			goto base
		}
	}
base:
	switch value := expr.(type) {
	case *ast.Ident:
		if r.types[fn.Package][value.Name] {
			return typeRef{fn.Package, value.Name}
		}
	case *ast.SelectorExpr:
		if ident, ok := value.X.(*ast.Ident); ok && !fn.Shadowed[ident.Name] {
			importPath := fn.File.Imports[ident.Name]
			pkg := localImport(r.module, importPath, r.packages)
			if r.types[pkg][value.Sel.Name] {
				return typeRef{pkg, value.Sel.Name}
			}
			if importPath != "" && pkg == "" && importPath != r.module && !strings.HasPrefix(importPath, r.module+"/") {
				return typeRef{"@" + importPath, value.Sel.Name}
			}
		}
	}
	return typeRef{}
}

func (r resolver) expressionType(expr ast.Expr, fn function) typeRef {
	expr = unwrap(expr)
	switch value := expr.(type) {
	case *ast.Ident:
		return fn.Bindings[value.Name]
	case *ast.UnaryExpr:
		if value.Op == token.AND {
			return r.expressionType(value.X, fn)
		}
	case *ast.CompositeLit:
		return r.declaredType(value.Type, fn)
	case *ast.CallExpr:
		if target := r.resolve(value.Fun, fn); target != "" {
			if results := r.returns[target]; len(results) == 1 {
				return results[0]
			}
			return typeRef{}
		}
		return r.declaredType(value.Fun, fn) // A conversion to a known local type.
	}
	return typeRef{}
}

func (r resolver) localBindings(fn function) map[string]typeRef {
	bindings := make(map[string]typeRef)
	seen := make(map[string]bool)
	add := func(name string, typ typeRef) {
		if name == "_" {
			return
		}
		if seen[name] {
			bindings[name] = typeRef{} // Shadowed in another scope: do not guess.
			return
		}
		seen[name] = true
		bindings[name] = typ
	}
	for _, fields := range []*ast.FieldList{fn.Decl.Type.Params, fn.Decl.Type.Results} {
		if fields == nil {
			continue
		}
		for _, field := range fields.List {
			for _, ident := range field.Names {
				add(ident.Name, r.declaredType(field.Type, fn))
			}
		}
	}
	if fn.ReceiverVar != "" {
		add(fn.ReceiverVar, typeRef{fn.Package, fn.ReceiverType})
	}
	fn.Bindings = bindings
	if fn.Decl.Body != nil {
		ast.Inspect(fn.Decl.Body, func(n ast.Node) bool {
			switch item := n.(type) {
			case *ast.AssignStmt:
				if item.Tok == token.DEFINE {
					var results []typeRef
					if len(item.Rhs) == 1 {
						if call, ok := item.Rhs[0].(*ast.CallExpr); ok {
							results = r.returns[r.resolve(call.Fun, fn)]
						}
					}
					for i, lhs := range item.Lhs {
						if ident, ok := lhs.(*ast.Ident); ok {
							typ := typeRef{}
							if len(results) == len(item.Lhs) {
								typ = results[i]
							} else if i < len(item.Rhs) && len(item.Lhs) == len(item.Rhs) {
								typ = r.expressionType(item.Rhs[i], fn)
							}
							add(ident.Name, typ)
						}
					}
				}
			case *ast.RangeStmt:
				if item.Tok == token.DEFINE {
					for _, expr := range []ast.Expr{item.Key, item.Value} {
						if ident, ok := expr.(*ast.Ident); ok {
							add(ident.Name, typeRef{})
						}
					}
				}
			case *ast.ValueSpec:
				for i, ident := range item.Names {
					typ := r.declaredType(item.Type, fn)
					if typ == (typeRef{}) && i < len(item.Values) {
						typ = r.expressionType(item.Values[i], fn)
					}
					add(ident.Name, typ)
				}
			case *ast.FuncLit:
				for _, fields := range []*ast.FieldList{item.Type.Params, item.Type.Results} {
					if fields != nil {
						for _, field := range fields.List {
							for _, ident := range field.Names {
								add(ident.Name, typeRef{})
							}
						}
					}
				}
			}
			return true
		})
	}
	return bindings
}

func (r resolver) resolve(expr ast.Expr, fn function) string {
	expr = unwrap(expr)
	switch value := expr.(type) {
	case *ast.Ident:
		if fn.Shadowed[value.Name] {
			return ""
		}
		return unique(r.index, fn.Package, value.Name)
	case *ast.SelectorExpr:
		if typ := r.expressionType(value.X, fn); typ != (typeRef{}) {
			if strings.HasPrefix(typ.Package, "@") && strings.HasSuffix(typ.Name, "Client") {
				servers := r.rpcServers[typ]
				if len(servers) == 1 {
					server := servers[0]
					return unique(r.index, server.Package, server.Name+"."+value.Sel.Name)
				}
			}
			return unique(r.index, typ.Package, typ.Name+"."+value.Sel.Name)
		}
		if qualifier, ok := value.X.(*ast.Ident); ok {
			if fn.Shadowed[qualifier.Name] {
				return ""
			}
			if qualifier.Name == fn.ReceiverVar && fn.ReceiverType != "" {
				return unique(r.index, fn.Package, fn.ReceiverType+"."+value.Sel.Name)
			}
			if importPath, ok := fn.File.Imports[qualifier.Name]; ok {
				pkg := localImport(r.module, importPath, r.packages)
				return unique(r.index, pkg, value.Sel.Name)
			}
			if r.types[fn.Package][qualifier.Name] {
				return unique(r.index, fn.Package, qualifier.Name+"."+value.Sel.Name)
			}
			return ""
		}
		if left, ok := value.X.(*ast.SelectorExpr); ok {
			if qualifier, ok := left.X.(*ast.Ident); ok {
				if importPath, ok := fn.File.Imports[qualifier.Name]; ok && !fn.Shadowed[qualifier.Name] {
					pkg := localImport(r.module, importPath, r.packages)
					if r.types[pkg][left.Sel.Name] {
						return unique(r.index, pkg, left.Sel.Name+"."+value.Sel.Name)
					}
				}
			}
		}
	}
	return ""
}

func build(sources map[string][]byte, root string, tags []string) (output, error) {
	result := output{Nodes: []node{}, Edges: []edge{}, UnresolvedCalls: []unresolvedCall{}, Files: []fileFact{}}
	context := gobuild.Default
	context.BuildTags = append([]string{}, tags...)
	sort.Strings(context.BuildTags)
	result.BuildContext = buildContext{
		GOOS: context.GOOS, GOARCH: context.GOARCH,
		GoVersion: runtime.Version(), Compiler: context.Compiler,
		CgoEnabled: context.CgoEnabled, BuildTags: context.BuildTags,
	}
	fset := token.NewFileSet()
	files := []sourceFile{}
	packages := make(map[string]map[string]bool)
	types := make(map[string]map[string]bool)
	paths := make([]string, 0, len(sources))
	for filename := range sources {
		if strings.HasSuffix(filename, ".go") {
			paths = append(paths, filename)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return result, fmt.Errorf("no Go source files found in the repository")
	}
	for _, filename := range paths {
		if root != "" {
			matched, err := context.MatchFile(
				filepath.Join(root, filepath.FromSlash(path.Dir(filename))), path.Base(filename),
			)
			if err != nil {
				return result, fmt.Errorf("check Go build constraints for %s: %w", filename, err)
			}
			if !matched {
				continue
			}
		}
		file, err := parser.ParseFile(fset, filename, sources[filename], parser.AllErrors)
		if err != nil {
			return result, fmt.Errorf("parse %s: %w", filename, err)
		}
		pkg := packageKey(filename, file.Name.Name)
		files = append(files, sourceFile{filename, pkg, importsFor(file), file})
		dir := path.Dir(filename)
		if packages[dir] == nil {
			packages[dir] = make(map[string]bool)
		}
		packages[dir][file.Name.Name] = true
		if types[pkg] == nil {
			types[pkg] = make(map[string]bool)
		}
		for _, decl := range file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range general.Specs {
				if typ, ok := spec.(*ast.TypeSpec); ok {
					types[pkg][typ.Name.Name] = true
				}
			}
		}
	}

	module := modulePath(sources["go.mod"])
	for _, file := range files {
		imports := make(map[string]bool)
		for _, spec := range file.AST.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err == nil {
				if pkg := localImport(module, importPath, packages); pkg != "" {
					imports[pkg] = true
				}
			}
		}
		dependencies := make([]string, 0, len(imports))
		for pkg := range imports {
			dependencies = append(dependencies, pkg)
		}
		sort.Strings(dependencies)
		result.Files = append(result.Files, fileFact{file.Path, file.Package, dependencies})
	}
	index := make(map[string][]string)
	functions := []function{}
	occurrences := make(map[string]int)
	for i := range files {
		file := &files[i]
		for _, decl := range file.AST.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			receiverType, receiverVar := receiver(fn)
			qualname := fn.Name.Name
			if receiverType != "" {
				qualname = receiverType + "." + qualname
			}
			baseID := file.Path + "::" + qualname
			occurrences[baseID]++
			id := baseID
			if qualname == "init" || occurrences[baseID] > 1 {
				id = fmt.Sprintf("%s#%d", baseID, occurrences[baseID])
			}
			item := node{
				ID: id, Path: file.Path,
				Qualname: qualname, Line: fset.Position(fn.Pos()).Line, EndLine: fset.Position(fn.End()).Line,
				IsTest: isTest(fn, file.Path, file.Imports),
			}
			result.Nodes = append(result.Nodes, item)
			index[file.Package+"|"+qualname] = append(index[file.Package+"|"+qualname], item.ID)
			functions = append(functions, function{
				Node: item, Package: file.Package, ReceiverType: receiverType,
				ReceiverVar: receiverVar, Shadowed: shadowedNames(fn), File: file, Decl: fn,
			})
		}
	}

	r := resolver{index: index, packages: packages, types: types, module: module,
		returns: make(map[string][]typeRef), rpcServers: make(map[typeRef][]typeRef)}
	// A compile-time server interface assertion identifies which local receiver
	// handles calls on the generated client interface from the same import path.
	for i := range files {
		file := &files[i]
		for _, decl := range file.AST.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || value.Names[0].Name != "_" || len(value.Values) != 1 {
					continue
				}
				iface, ok := value.Type.(*ast.SelectorExpr)
				if !ok || !strings.HasSuffix(iface.Sel.Name, "Server") {
					continue
				}
				alias, ok := iface.X.(*ast.Ident)
				if !ok || file.Imports[alias.Name] == "" {
					continue
				}
				conversion, ok := value.Values[0].(*ast.CallExpr)
				if !ok || len(conversion.Args) != 1 {
					continue
				}
				nilValue, ok := conversion.Args[0].(*ast.Ident)
				if !ok || nilValue.Name != "nil" {
					continue
				}
				pointer, ok := unwrap(conversion.Fun).(*ast.StarExpr)
				if !ok {
					continue
				}
				receiver, ok := pointer.X.(*ast.Ident)
				if !ok || !types[file.Package][receiver.Name] {
					continue
				}
				client := typeRef{"@" + file.Imports[alias.Name], strings.TrimSuffix(iface.Sel.Name, "Server") + "Client"}
				server := typeRef{file.Package, receiver.Name}
				found := false
				for _, existing := range r.rpcServers[client] {
					found = found || existing == server
				}
				if !found {
					r.rpcServers[client] = append(r.rpcServers[client], server)
				}
			}
		}
	}
	for _, fn := range functions {
		fields := fn.Decl.Type.Results
		if fields != nil {
			for _, field := range fields.List {
				count := len(field.Names)
				if count == 0 {
					count = 1
				}
				for range count {
					r.returns[fn.Node.ID] = append(r.returns[fn.Node.ID], r.declaredType(field.Type, fn))
				}
			}
		}
	}
	for i := range functions {
		functions[i].Bindings = r.localBindings(functions[i])
	}
	for _, fn := range functions {
		if fn.Decl.Body == nil {
			continue
		}
		ast.Inspect(fn.Decl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			line := fset.Position(call.Pos()).Line
			target := r.resolve(call.Fun, fn)
			if target != "" {
				result.Edges = append(result.Edges, edge{fn.Node.ID, target, fn.File.Path, line})
			} else {
				var expression bytes.Buffer
				_ = printer.Fprint(&expression, fset, call.Fun)
				result.UnresolvedCalls = append(result.UnresolvedCalls,
					unresolvedCall{fn.Node.ID, expression.String(), fn.File.Path, line})
			}
			return true
		})
	}
	return result, nil
}

// Analyze returns the source facts needed to build a per-test call graph.
func Analyze(sources map[string][]byte, root string, tags []string) ([]byte, error) {
	facts, err := build(sources, root, tags)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil, fmt.Errorf("encode graph facts: %w", err)
	}
	return encoded, nil
}
