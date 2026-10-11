package main

import (
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// gatewayMapper derives an operation->RPC map from gateway source by SSA call
// reachability: a gateway handler method's name is the HTTP operation id, and
// any backend-client method it reaches (directly or through helpers) is a
// candidate backend RPC. gRPC client and server method names are identical, so
// the reached client method name is the backend RPC name.
type gatewayMapper struct {
	l           *loaded
	backendPkgs map[string]bool // client package paths that denote the target backend
	whitelist   map[string]bool // valid backend RPC names (empty = accept any)
	memo        map[*ssa.Function]map[string]int
}

// rpcHit is a reached backend RPC and the shallowest call depth at which it was
// seen from the handler. Shallower calls are more likely the operation's main
// RPC rather than an incidental auth/resolver lookup.
type rpcHit struct {
	rpc   string
	depth int
}

const gatewayMaxDepth = 8

// buildGatewayMap scans every gateway handler method and resolves it to one
// backend RPC. Ambiguous handlers (several equally-shallow RPCs) are reported
// as unresolved rather than guessed.
func (l *loaded) buildGatewayMap(service string, backendPkgs, whitelist []string) opMapOut {
	gm := &gatewayMapper{
		l:           l,
		backendPkgs: toSet(backendPkgs),
		whitelist:   toSet(whitelist),
		memo:        map[*ssa.Function]map[string]int{},
	}
	byOp := map[string]string{}
	unresolved := map[string]bool{}

	for _, sp := range l.prog.AllPackages() {
		if sp == nil || sp.Pkg == nil {
			continue
		}
		for _, mem := range sp.Members {
			named, ok := mem.(*ssa.Type)
			if !ok {
				continue
			}
			mset := l.prog.MethodSets.MethodSet(types.NewPointer(named.Type()))
			for i := 0; i < mset.Len(); i++ {
				fn := l.prog.MethodValue(mset.At(i))
				if fn == nil || !isHandler(fn) {
					continue
				}
				op := fn.Name()
				hits := gm.reachedRPCs(fn)
				if rpc, amb := pickRPC(op, hits); amb {
					unresolved[op] = true
				} else if rpc != "" {
					byOp[op] = rpc
					// oapi exposes a POST alias for GET-able operations; map it too.
					byOp[op+"Post"] = rpc
				}
			}
		}
	}
	return opMapOut{Service: service, ByOperation: byOp, Unresolved: sortedSetKeys(unresolved)}
}

// isHandler reports whether fn looks like an operation handler:
// func (recv) Name(ctx, *Request) (*Response, error), exported.
func isHandler(fn *ssa.Function) bool {
	if fn.Signature.Recv() == nil || !ast_isExported(fn.Name()) {
		return false
	}
	p := fn.Signature.Params()
	if p.Len() != 2 {
		return false
	}
	if _, _, ok := namedTypeOf(p.At(1).Type()); !ok {
		return false
	}
	r := fn.Signature.Results()
	if r.Len() != 2 {
		return false
	}
	return r.At(1).Type().String() == "error"
}

// reachedRPCs returns every backend RPC reachable from fn with its shallowest depth.
func (gm *gatewayMapper) reachedRPCs(fn *ssa.Function) map[string]int {
	return gm.walk(fn, 0, map[*ssa.Function]bool{})
}

func (gm *gatewayMapper) walk(fn *ssa.Function, depth int, onPath map[*ssa.Function]bool) map[string]int {
	if fn == nil || depth > gatewayMaxDepth || onPath[fn] {
		return nil
	}
	onPath[fn] = true
	defer delete(onPath, fn)

	out := map[string]int{}
	record := func(rpc string, d int) {
		if gm.whitelist != nil && len(gm.whitelist) > 0 && !gm.whitelist[rpc] {
			return
		}
		if cur, ok := out[rpc]; !ok || d < cur {
			out[rpc] = d
		}
	}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			cc := call.Call
			// Invoke (interface) call: the backend clients are interfaces.
			if cc.IsInvoke() {
				if m := cc.Method; m != nil && m.Pkg() != nil && gm.backendPkgs[m.Pkg().Path()] {
					record(m.Name(), depth)
				}
				continue
			}
			// Static call into a helper in the same program: recurse.
			if callee := cc.StaticCallee(); callee != nil {
				for rpc, d := range gm.walk(callee, depth+1, onPath) {
					record(rpc, d)
				}
			}
		}
	}
	return out
}

// pickRPC chooses the operation's primary RPC: the shallowest reached RPC,
// tie-broken by name similarity to the operation. Returns amb=true when two or
// more RPCs tie at the shallowest depth with equal similarity.
func pickRPC(op string, hits map[string]int) (rpc string, amb bool) {
	if len(hits) == 0 {
		return "", false
	}
	minDepth := gatewayMaxDepth + 1
	for _, d := range hits {
		if d < minDepth {
			minDepth = d
		}
	}
	var cands []string
	for r, d := range hits {
		if d == minDepth {
			cands = append(cands, r)
		}
	}
	sort.Strings(cands)
	if len(cands) == 1 {
		return cands[0], false
	}
	best, bestScore, tie := "", -1, false
	for _, c := range cands {
		s := tokenOverlap(op, c)
		if s > bestScore {
			best, bestScore, tie = c, s, false
		} else if s == bestScore {
			tie = true
		}
	}
	if tie {
		return "", true
	}
	return best, false
}

// tokenOverlap counts shared lowercased camel tokens between an operation and
// an RPC name (e.g. WorksCreate vs CreateWork share work+create).
func tokenOverlap(a, b string) int {
	sa := toSet(lowerTokens(camelSplit(a)))
	n := 0
	for _, t := range lowerTokens(camelSplit(b)) {
		if sa[t] {
			n++
		}
	}
	return n
}

func lowerTokens(ts []string) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, strings.ToLower(t))
	}
	return out
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		if s != "" {
			m[s] = true
		}
	}
	return m
}

func sortedSetKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ast_isExported(name string) bool {
	return name != "" && name[0] >= 'A' && name[0] <= 'Z'
}
