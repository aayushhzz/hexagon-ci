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

	// Output-provenance caches, shared across all RPCs of one program run.
	outCache  map[string]map[int]depval // outSignature -> per-result dep paths
	depMemo   map[string]int            // fn.String() -> 0 unknown, 1 yes, 2 no
	outBudget int                       // shared step budget for the output pass

	// Single-impl devirtualization index, built lazily and shared.
	concreteTypes   []*types.Named
	concreteIndexed bool
	devirtSeen      map[string]bool
	devirtCache     map[string]*ssa.Function
	localPkgs       map[string]bool // package paths matched by the load patterns
}

// load type-checks the target module at dir and builds SSA. Dependencies are
// loaded as types-only (export data); only initial packages get SSA bodies.
func load(dir string, patterns []string) (*loaded, error) {
	return loadMode(dir, patterns, false)
}

// loadTolerant is load that does not bail when some packages fail to
// type-check (e.g. a cgo command elsewhere in the module). SSA is built
// per-package so a broken package is skipped instead of aborting the run.
// Used by the discovery modes, which only need the clean server/handler packages.
func loadTolerant(dir string, patterns []string) (*loaded, error) {
	return loadMode(dir, patterns, true)
}

func loadMode(dir string, patterns []string, tolerant bool) (*loaded, error) {
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
	if !tolerant && packages.PrintErrors(initial) > 0 {
		return nil, fmt.Errorf("packages contained errors (see above)")
	}

	var prog *ssa.Program
	if tolerant {
		// Create SSA for every package but build each independently, recovering
		// from any package whose incomplete types make the builder panic.
		prog = buildTolerant(initial)
	} else {
		prog, _ = ssautil.AllPackages(initial, ssa.InstantiateGenerics)
		prog.Build()
	}

	l := &loaded{
		prog:      prog,
		pkgs:      map[string]*packages.Package{},
		ssaByPkg:  map[string]*ssa.Package{},
		localPkgs: map[string]bool{},
	}
	for _, p := range initial {
		if p.PkgPath != "" {
			l.localPkgs[p.PkgPath] = true
		}
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

// buildTolerant creates and builds SSA packages one by one, skipping any that
// fail, so a single broken package does not abort discovery.
func buildTolerant(initial []*packages.Package) *ssa.Program {
	prog := ssa.NewProgram(initial[0].Fset, ssa.InstantiateGenerics)
	created := map[*types.Package]bool{}
	packages.Visit(initial, nil, func(p *packages.Package) {
		if p.Types == nil || p.IllTyped || created[p.Types] {
			return
		}
		if p.TypesInfo == nil || len(p.Syntax) == 0 {
			// Dependency without syntax: create from types only (no bodies).
			func() {
				defer func() { _ = recover() }()
				prog.CreatePackage(p.Types, nil, nil, true)
				created[p.Types] = true
			}()
			return
		}
		func() {
			defer func() { _ = recover() }()
			prog.CreatePackage(p.Types, p.Syntax, p.TypesInfo, true)
			created[p.Types] = true
		}()
	})
	for _, sp := range prog.AllPackages() {
		func() {
			defer func() { _ = recover() }()
			sp.Build()
		}()
	}
	return prog
}

// modulePath returns the main module's import path, used to tell the module's
// own packages apart from dependencies.
func (l *loaded) modulePath() string {
	for _, p := range l.pkgs {
		if p.Module != nil && p.Module.Main && p.Module.Path != "" {
			return p.Module.Path
		}
	}
	return ""
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
