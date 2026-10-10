package main

import (
	"fmt"
	"go/types"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// loaded holds the typed+SSA program plus a path->package index.
type loaded struct {
	prog     *ssa.Program
	pkgs     map[string]*packages.Package // keyed by PkgPath
	ssaByPkg map[string]*ssa.Package
}

// load type-checks the target module at dir and builds SSA. Dependencies are
// loaded as types-only (export data); only initial packages get SSA bodies.
func load(dir string, patterns []string) (*loaded, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedModule,
		Dir:   dir,
		Tests: false,
	}
	initial, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}
	if packages.PrintErrors(initial) > 0 {
		return nil, fmt.Errorf("packages contained errors (see above)")
	}

	prog, _ := ssautil.AllPackages(initial, ssa.InstantiateGenerics)
	prog.Build()

	l := &loaded{
		prog:     prog,
		pkgs:     map[string]*packages.Package{},
		ssaByPkg: map[string]*ssa.Package{},
	}
	packages.Visit(initial, nil, func(p *packages.Package) {
		if p.PkgPath != "" {
			l.pkgs[p.PkgPath] = p
		}
	})
	for _, sp := range prog.AllPackages() {
		if sp != nil && sp.Pkg != nil {
			l.ssaByPkg[sp.Pkg.Path()] = sp
		}
	}
	return l, nil
}

// lookupType finds a named type by package path and name.
func (l *loaded) lookupType(pkgPath, name string) (*types.Named, error) {
	p, ok := l.pkgs[pkgPath]
	if !ok || p.Types == nil {
		return nil, fmt.Errorf("package not loaded: %s", pkgPath)
	}
	obj := p.Types.Scope().Lookup(name)
	if obj == nil {
		return nil, fmt.Errorf("%s.%s not found", pkgPath, name)
	}
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return nil, fmt.Errorf("%s.%s is not a named type", pkgPath, name)
	}
	return named, nil
}
