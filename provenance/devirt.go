package main

import (
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// Single-implementation devirtualization.
//
// SSA leaves interface (invoke) calls unresolved: the dynamic receiver type is
// not known statically. Many devrev services route real work through an
// interface that has exactly one concrete implementation in the analyzed
// program (for example db.Database -> *dynamoDatabase, or api.Timeline ->
// *Service). When an interface has a single concrete implementer we can safely
// bind the call to that method and keep following the data flow, which recovers
// the DB/RPC sinks and dependency edges that would otherwise read as opaque
// dispatch. This is a conservative class-hierarchy resolution: if two or more
// concrete types implement the interface we leave the call unresolved.

// soleImpl resolves an interface (invoke) call to the method of its only
// concrete implementer in the loaded program, or nil when the interface is
// empty, unresolvable, or implemented by more than one concrete type.
func (l *loaded) soleImpl(c *ssa.CallCommon) *ssa.Function {
	if c == nil || !c.IsInvoke() || c.Method == nil {
		return nil
	}
	iface, ok := c.Value.Type().Underlying().(*types.Interface)
	if !ok || iface.Empty() {
		return nil
	}
	key := c.Value.Type().String() + "#" + methodKey(c.Method)
	if l.devirtSeen[key] {
		return l.devirtCache[key]
	}
	if l.devirtSeen == nil {
		l.devirtSeen = map[string]bool{}
		l.devirtCache = map[string]*ssa.Function{}
	}
	l.ensureConcreteIndex()

	var impl *types.Named
	count := 0
	for _, nt := range l.concreteTypes {
		if !types.Implements(types.NewPointer(nt), iface) {
			continue
		}
		impl = nt
		count++
		if count > 1 {
			break
		}
	}

	var fn *ssa.Function
	if count == 1 && impl != nil {
		sel := l.prog.MethodSets.MethodSet(types.NewPointer(impl)).Lookup(c.Method.Pkg(), c.Method.Name())
		if sel != nil {
			fn = l.prog.MethodValue(sel)
		}
	}
	l.devirtSeen[key] = true
	l.devirtCache[key] = fn
	return fn
}

// ensureConcreteIndex collects every concrete (non-interface) named type in the
// program once, so soleImpl can scan implementers without rebuilding the list.
func (l *loaded) ensureConcreteIndex() {
	if l.concreteIndexed {
		return
	}
	l.concreteIndexed = true
	seen := map[*types.Named]bool{}
	for _, p := range l.prog.AllPackages() {
		if p == nil {
			continue
		}
		for _, m := range p.Members {
			t, ok := m.(*ssa.Type)
			if !ok {
				continue
			}
			nt, ok := t.Type().(*types.Named)
			if !ok {
				continue
			}
			if _, isIface := nt.Underlying().(*types.Interface); isIface {
				continue
			}
			if isTestDouble(nt) {
				continue
			}
			// Only count implementers defined in the analyzed module. Devrev
			// client packages implement the same *-client/api interface as the
			// server, so a global scan would see two real implementers.
			if obj := nt.Obj(); obj == nil || obj.Pkg() == nil || !l.localPkgs[obj.Pkg().Path()] {
				continue
			}
			if !seen[nt] {
				seen[nt] = true
				l.concreteTypes = append(l.concreteTypes, nt)
			}
		}
	}
}

// methodKey identifies an interface method for the devirtualization cache.
func methodKey(m *types.Func) string {
	pkg := ""
	if m.Pkg() != nil {
		pkg = m.Pkg().Path()
	}
	return pkg + "." + m.Name()
}

// isTestDouble reports whether a named type is a generated mock/fake/stub, so
// it is not counted as a real implementer. gomock and similar generators are a
// common reason a production interface appears to have more than one impl.
func isTestDouble(nt *types.Named) bool {
	obj := nt.Obj()
	if obj == nil {
		return false
	}
	name := obj.Name()
	for _, p := range []string{"Mock", "Fake", "Stub", "Spy"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	if obj.Pkg() == nil {
		return false
	}
	path := strings.ToLower(obj.Pkg().Path())
	for _, frag := range []string{"/mock", "mocks", "/testutil", "/testdata", "/testfixture", "/fake"} {
		if strings.Contains(path, frag) {
			return true
		}
	}
	return false
}
