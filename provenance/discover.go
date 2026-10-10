package main

import (
	"fmt"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// discovered is one auto-detected gRPC server registration in a module: the
// generated server interface plus the concrete type that implements it. It maps
// directly onto a services.json manifest entry, so onboarding a service needs
// no hand-written interface/impl config.
type discovered struct {
	Name     string `json:"name"`
	IfacePkg string `json:"iface_pkg"`
	Iface    string `json:"iface"`
	ImplPkg  string `json:"impl_pkg"`
	Impl     string `json:"impl"`
	Patterns string `json:"patterns"`
}

// discoverServers scans the loaded module for calls to grpc Register<X>Server
// helpers and reports each (interface, impl) pair whose implementation lives in
// the module at modPath. The module filter drops infra interfaces (grpc health,
// reflection) whose impl is in a dependency.
func (l *loaded) discoverServers(modPath string) []discovered {
	seen := map[string]discovered{}
	for _, sp := range l.prog.AllPackages() {
		if sp == nil || sp.Pkg == nil {
			continue
		}
		for _, mem := range sp.Members {
			fn, ok := mem.(*ssa.Function)
			if !ok {
				continue
			}
			for _, d := range scanRegisterCalls(fn) {
				if modPath != "" && !strings.HasPrefix(d.ImplPkg, modPath) {
					continue
				}
				key := d.IfacePkg + "." + d.Iface
				if _, dup := seen[key]; !dup {
					seen[key] = d
				}
			}
		}
	}
	out := make([]discovered, 0, len(seen))
	for _, d := range seen {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// scanRegisterCalls walks fn (and its anonymous functions) for Register*Server
// calls and extracts the server spec from each.
func scanRegisterCalls(fn *ssa.Function) []discovered {
	var out []discovered
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			callee := call.Call.StaticCallee()
			if callee == nil {
				continue
			}
			name := callee.Name()
			if !strings.HasPrefix(name, "Register") || !strings.HasSuffix(name, "Server") {
				continue
			}
			sig := callee.Signature
			if sig.Params().Len() < 2 || len(call.Call.Args) < 2 {
				continue
			}
			ifacePkg, ifaceName, ok := namedTypeOf(sig.Params().At(1).Type())
			if !ok {
				continue
			}
			implPkg, implName, ok := concreteTypeOf(call.Call.Args[1])
			if !ok {
				continue
			}
			out = append(out, discovered{
				Name:     strings.TrimSuffix(ifaceName, "Server"),
				IfacePkg: ifacePkg,
				Iface:    ifaceName,
				ImplPkg:  implPkg,
				Impl:     implName,
				Patterns: implPkg,
			})
		}
	}
	for _, anon := range fn.AnonFuncs {
		out = append(out, scanRegisterCalls(anon)...)
	}
	return out
}

// namedTypeOf returns the package path and name of a (possibly pointer) named type.
func namedTypeOf(t types.Type) (pkg, name string, ok bool) {
	if p, isPtr := t.(*types.Pointer); isPtr {
		t = p.Elem()
	}
	named, isNamed := t.(*types.Named)
	if !isNamed || named.Obj().Pkg() == nil {
		return "", "", false
	}
	return named.Obj().Pkg().Path(), named.Obj().Name(), true
}

// concreteTypeOf unwraps a MakeInterface (concrete->interface conversion at the
// call site) and returns the concrete implementation's package and type name.
func concreteTypeOf(v ssa.Value) (pkg, name string, ok bool) {
	if mi, isMI := v.(*ssa.MakeInterface); isMI {
		return namedTypeOf(mi.X.Type())
	}
	return namedTypeOf(v.Type())
}

// discoverFmt renders discovered specs as a services.json-style fragment.
func discoverFmt(ds []discovered) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "  %-28s iface=%s.%s impl=%s.%s\n", d.Name, d.IfacePkg, d.Iface, d.ImplPkg, d.Impl)
	}
	return b.String()
}
