package main

import (
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// Output provenance: the mirror of input taint. We seed taint at the results of
// downstream dependency calls (external services, classified "rpc") and follow
// it forward to the RPC response, recording which response field paths carry
// that dependency's data. Datastore/cache reads are deliberately ignored: a
// test need not assert on plumbing, only on data an external service produced.
//
// The result is field-precise where the response builders are analyzable (we
// descend into converter bodies and compose field paths), and falls back to a
// coarse "reaches the response" signal (path "out") when a builder is opaque.

// depval maps a response-relative sub-path ("" for the whole value, ".A.B" for
// a nested field) to the set of dependency ids whose data lands there.
type depval map[string]map[string]bool

// Memory bounds for the output pass: deep call graphs can otherwise accumulate
// unboundedly long and numerous response paths. Beyond these caps we collapse
// to coarser paths, trading field precision for bounded memory.
const (
	maxRelDepth = 6
	maxRelKeys  = 32
	maxCache    = 150000 // hard cap on cached function summaries
)

// truncRel caps the component depth of a response-relative key (".a.b.c").
func truncRel(rel string) string {
	if rel == "" {
		return rel
	}
	parts := strings.Split(rel, ".")
	if len(parts)-1 <= maxRelDepth {
		return rel
	}
	return strings.Join(parts[:maxRelDepth+1], ".")
}

func (d depval) add(rel, dep string) {
	rel = truncRel(rel)
	if d[rel] == nil {
		if len(d) >= maxRelKeys {
			rel = "" // too many distinct paths: collapse to the whole response
		}
		if d[rel] == nil {
			d[rel] = map[string]bool{}
		}
	}
	d[rel][dep] = true
}

func (d depval) merge(o depval) {
	for rel, deps := range o {
		for dep := range deps {
			d.add(rel, dep)
		}
	}
}

// prefixed returns a copy of d with field prefix applied: "" -> prefix,
// ".x" -> prefix+".x".
func (d depval) prefixed(prefix string) depval {
	out := depval{}
	for rel, deps := range d {
		key := prefix + rel
		for dep := range deps {
			out.add(key, dep)
		}
	}
	return out
}

func (d depval) empty() bool { return len(d) == 0 }

// collapse flattens any sub-paths to the whole value (""), used when data flows
// through a collection (map/slice/append) where field precision is lost.
func collapse(d depval) depval {
	if d.empty() {
		return d
	}
	out := depval{}
	for _, deps := range d {
		for dep := range deps {
			out.add("", dep)
		}
	}
	return out
}

// outAnalyzer runs the output taint for one entrypoint, reusing the loaded SSA.
type outAnalyzer struct {
	l       *loaded
	visited map[string]bool
	cache   map[string]map[int]depval
	depth   int
	budget  int
}

// analyzeOutput populates p.OutputPaths for one entrypoint.
func (l *loaded) analyzeOutput(ep entrypoint, p *Provenance) {
	if os.Getenv("PROV_NO_OUTPUT") != "" {
		return
	}
	if l.outCache == nil {
		l.outCache = map[string]map[int]depval{}
		l.depMemo = map[string]int{}
		l.outBudget = 1500000 // shared across all entrypoints of one run
	}
	if l.outBudget <= 0 {
		return
	}
	oa := &outAnalyzer{
		l:       l,
		visited: map[string]bool{},
		cache:   l.outCache,
		budget:  l.outBudget,
	}
	defer func() { l.outBudget = oa.budget }()
	res := oa.fn(ep.fn, nil)
	dv := res[0] // result index 0 is the response message
	if dv == nil {
		return
	}
	seen := map[string]bool{}
	for rel, deps := range dv {
		comps := splitRel(rel)
		valid := validateResponsePath(ep.responseType, comps)
		path := "out"
		if len(valid) > 0 {
			path = "out." + strings.Join(valid, ".")
		}
		for dep := range deps {
			key := path + "|" + dep
			if seen[key] {
				continue
			}
			seen[key] = true
			p.OutputPaths = append(p.OutputPaths, OutputPath{Path: path, Dep: dep})
		}
	}
	sort.Slice(p.OutputPaths, func(i, j int) bool {
		if p.OutputPaths[i].Path != p.OutputPaths[j].Path {
			return p.OutputPaths[i].Path < p.OutputPaths[j].Path
		}
		return p.OutputPaths[i].Dep < p.OutputPaths[j].Dep
	})
}

// splitRel turns a response-relative key ("", ".A.B") into components.
func splitRel(rel string) []string {
	rel = strings.TrimPrefix(rel, ".")
	if rel == "" {
		return nil
	}
	return strings.Split(rel, ".")
}

// validateResponsePath walks comps against the response proto type, keeping the
// longest prefix that corresponds to real fields and truncating at scalars or
// unknown fields. This drops impossible paths (e.g. Total.FilterDoc where Total
// is an int32) that interprocedural tracing can otherwise produce.
func validateResponsePath(responseType types.Type, comps []string) []string {
	cur := structOf(responseType)
	if cur == nil {
		return nil
	}
	var kept []string
	for _, c := range comps {
		ft, ok := fieldTypeByName(cur, c)
		if !ok {
			break
		}
		kept = append(kept, c)
		cur = structOf(ft)
		if cur == nil {
			break // scalar/opaque leaf: cannot descend further
		}
	}
	return kept
}

// fieldTypeByName returns the type of the named Go field in a struct.
func fieldTypeByName(st *types.Struct, name string) (types.Type, bool) {
	for i := 0; i < st.NumFields(); i++ {
		if st.Field(i).Name() == name {
			return st.Field(i).Type(), true
		}
	}
	return nil, false
}

// structOf peels pointer/slice/array/map/named wrappers to reach a struct, so a
// repeated message field still descends into its element fields.
func structOf(t types.Type) *types.Struct {
	for i := 0; i < 10 && t != nil; i++ {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Slice:
			t = x.Elem()
		case *types.Array:
			t = x.Elem()
		case *types.Map:
			t = x.Elem()
		case *types.Named:
			if s, ok := x.Underlying().(*types.Struct); ok {
				return s
			}
			t = x.Underlying()
		case *types.Struct:
			return x
		default:
			return nil
		}
	}
	return nil
}

// fn returns, per result index, the response-relative paths that carry
// dependency data, given dep-tainted parameters (seed may be nil).
func (oa *outAnalyzer) fn(fn *ssa.Function, seed map[int]depval) map[int]depval {
	if fn == nil || len(fn.Blocks) == 0 {
		return nil
	}
	sig := outSignature(fn, seed)
	if r, ok := oa.cache[sig]; ok {
		return r
	}
	if oa.visited[sig] || oa.depth > 40 || oa.budget <= 0 {
		return nil
	}
	oa.visited[sig] = true
	oa.depth++
	defer func() { oa.depth-- }()

	taint := map[ssa.Value]depval{}
	tupleOut := map[ssa.Value]map[int]depval{}
	store := map[ssa.Value]depval{} // alloc root -> composed field paths

	get := func(v ssa.Value) depval {
		oa.budget--
		dv := depval{}
		if t := taint[v]; t != nil {
			dv.merge(t)
		}
		if root := allocRoot(v); root != nil {
			if s := store[root]; s != nil {
				dv.merge(s)
			}
		}
		return dv
	}
	setV := func(v ssa.Value, dv depval) bool {
		if dv.empty() {
			return false
		}
		if taint[v] == nil {
			taint[v] = depval{}
		}
		before := len(taint[v])
		taint[v].merge(dv)
		return len(taint[v]) != before
	}

	for i, dv := range seed {
		if i < len(fn.Params) {
			taint[fn.Params[i]] = dv
		}
	}

	maxIters := len(fn.Blocks)*2 + 4
	for iter := 0; iter < maxIters && oa.budget > 0; iter++ {
		changed := false
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if oa.step(fn, instr, taint, tupleOut, store, get, setV) {
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	// Build per-result-index return paths.
	out := map[int]depval{}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}
			for i, r := range ret.Results {
				dv := get(r)
				if dv.empty() {
					continue
				}
				if out[i] == nil {
					out[i] = depval{}
				}
				out[i].merge(dv)
			}
		}
	}
	if len(oa.cache) < maxCache {
		oa.cache[sig] = out
	}
	return out
}

func (oa *outAnalyzer) step(fn *ssa.Function, instr ssa.Instruction, taint map[ssa.Value]depval,
	tupleOut map[ssa.Value]map[int]depval, store map[ssa.Value]depval,
	get func(ssa.Value) depval, setV func(ssa.Value, depval) bool) bool {
	switch v := instr.(type) {
	case *ssa.UnOp, *ssa.ChangeType, *ssa.Convert, *ssa.MakeInterface, *ssa.TypeAssert, *ssa.Field, *ssa.FieldAddr, *ssa.IndexAddr, *ssa.Index, *ssa.Slice:
		if val, ok := v.(ssa.Value); ok {
			if src := operandDep(v, get); !src.empty() {
				return setV(val, src)
			}
		}
	case *ssa.Phi:
		dv := depval{}
		for _, e := range v.Edges {
			dv.merge(get(e))
		}
		return setV(v, dv)
	case *ssa.Lookup:
		return setV(v, get(v.X))
	case *ssa.Range:
		return setV(v, get(v.X))
	case *ssa.Next:
		return setV(v, get(v.Iter))
	case *ssa.MapUpdate:
		// Building a map with a dep-derived value makes the whole map
		// dep-derived (coarse, so custom-field maps keep the signal).
		if src := get(v.Value); !src.empty() {
			return setV(v.Map, collapse(src))
		}
		return false
	case *ssa.Extract:
		if tt, ok := tupleOut[v.Tuple]; ok {
			if dv, ok := tt[v.Index]; ok {
				return setV(v, dv)
			}
			return false
		}
		return setV(v, get(v.Tuple))
	case *ssa.Store:
		src := get(v.Val)
		if src.empty() {
			return false
		}
		root, fields := addrPath(v.Addr)
		if root == nil {
			return false
		}
		prefix := ""
		if len(fields) > 0 {
			prefix = "." + strings.Join(fields, ".")
		}
		if store[root] == nil {
			store[root] = depval{}
		}
		before := len(store[root])
		store[root].merge(src.prefixed(prefix))
		return len(store[root]) != before
	case *ssa.Call:
		return oa.stepCall(v, &v.Call, taint, tupleOut, get, setV)
	}
	return false
}

// operandDep carries dependency taint through value-forwarding operators and
// field reads (reading a field of a dep-derived struct is still dep-derived).
func operandDep(instr ssa.Instruction, get func(ssa.Value) depval) depval {
	switch v := instr.(type) {
	case *ssa.UnOp:
		return get(v.X)
	case *ssa.ChangeType:
		return get(v.X)
	case *ssa.Convert:
		return get(v.X)
	case *ssa.MakeInterface:
		return get(v.X)
	case *ssa.TypeAssert:
		return get(v.X)
	case *ssa.Field:
		return get(v.X)
	case *ssa.FieldAddr:
		return get(v.X)
	case *ssa.IndexAddr:
		return get(v.X)
	case *ssa.Index:
		return get(v.X)
	case *ssa.Slice:
		return get(v.X)
	}
	return depval{}
}

func (oa *outAnalyzer) stepCall(callVal ssa.Value, c *ssa.CallCommon, taint map[ssa.Value]depval,
	tupleOut map[ssa.Value]map[int]depval, get func(ssa.Value) depval, setV func(ssa.Value, depval) bool) bool {
	oa.budget--
	// Builtins (append, copy): the result carries any dep-derived argument.
	if b, ok := c.Value.(*ssa.Builtin); ok {
		if b.Name() == "append" || b.Name() == "copy" {
			dv := depval{}
			for _, arg := range c.Args {
				dv.merge(collapse(get(arg)))
			}
			return setV(callVal, dv)
		}
		return false
	}
	// Interface dispatch to an external service: seed the result as dep data.
	if c.IsInvoke() {
		if dep := invokeDep(c); dep != "" {
			dv := depval{}
			dv.add("", dep)
			return setV(callVal, dv)
		}
		return false
	}
	callee := c.StaticCallee()
	if callee == nil {
		return false
	}
	// Proto getter: a field read, carry dep taint from the receiver.
	if _, isGetter := protoGetter(callee); isGetter && len(c.Args) >= 1 {
		return setV(callVal, get(c.Args[0]))
	}
	// Bodyless external call: an rpc dependency result is the seed origin.
	if len(callee.Blocks) == 0 {
		if classifySink(callee) == "rpc" {
			dv := depval{}
			dv.add("", staticDep(callee))
			return setV(callVal, dv)
		}
		return false
	}
	if isPlumbing(callee) || !analyzableForOutput(callee) {
		return false
	}
	// Follow repo-local callees: seed any dep-tainted args onto params, and
	// recurse even with no tainted args so internally produced dependency data
	// is discovered. Result taint comes back as the callee's return paths.
	calleeSeed := map[int]depval{}
	for i, arg := range c.Args {
		if dv := get(arg); !dv.empty() && i < len(callee.Params) {
			calleeSeed[i] = dv
		}
	}
	// Prune: only recurse into a callee that either receives dependency-tainted
	// data or can transitively reach an external call itself. Pure helpers are
	// skipped, which keeps the whole-call-graph walk bounded.
	if len(calleeSeed) == 0 && !oa.producesDep(callee, map[string]bool{}) {
		return false
	}
	summary := oa.fn(callee, calleeSeed)
	if len(summary) == 0 {
		return false
	}
	if callee.Signature.Results().Len() > 1 {
		if tupleOut[callVal] == nil {
			tupleOut[callVal] = map[int]depval{}
		}
		grew := false
		for i, dv := range summary {
			if tupleOut[callVal][i] == nil {
				tupleOut[callVal][i] = depval{}
			}
			before := len(tupleOut[callVal][i])
			tupleOut[callVal][i].merge(dv)
			if len(tupleOut[callVal][i]) != before {
				grew = true
			}
		}
		return grew
	}
	return setV(callVal, summary[0])
}

// producesDep reports whether fn can transitively reach an external service
// call, so its result may carry dependency data. Memoized on the program.
func (oa *outAnalyzer) producesDep(fn *ssa.Function, stack map[string]bool) bool {
	if fn == nil || len(fn.Blocks) == 0 {
		return false
	}
	key := fn.String()
	switch oa.l.depMemo[key] {
	case 1:
		return true
	case 2:
		return false
	}
	if stack[key] {
		return false // cycle: resolve conservatively, caller memoizes
	}
	stack[key] = true
	result := false
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			c, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			cc := &c.Call
			if cc.IsInvoke() {
				if invokeDep(cc) != "" {
					result = true
				}
				continue
			}
			callee := cc.StaticCallee()
			if callee == nil {
				continue
			}
			if len(callee.Blocks) == 0 {
				if classifySink(callee) == "rpc" {
					result = true
				}
				continue
			}
			if analyzableForOutput(callee) && oa.producesDep(callee, stack) {
				result = true
			}
		}
		if result {
			break
		}
	}
	delete(stack, key)
	if result {
		oa.l.depMemo[key] = 1
	} else {
		oa.l.depMemo[key] = 2
	}
	return result
}

// analyzableForOutput limits recursion to devrev code (service + its devrev
// dependencies), where downstream calls live, to keep the pass bounded.
func analyzableForOutput(fn *ssa.Function) bool {
	p := calleePkgPath(fn)
	return strings.Contains(p, "devrev") || strings.Contains(p, "/internal/")
}

// staticDep derives a dependency id "service:Method" for a bodyless callee.
func staticDep(fn *ssa.Function) string {
	return serviceFromPkgPath(calleePkgPath(fn)) + ":" + fn.Name()
}

// invokeDep returns a dependency id for an interface call to an external
// service client, or "" if the call is not an external service call.
func invokeDep(c *ssa.CallCommon) string {
	if looksDB(c.Method.Name()) || isPlumbingMethod(c.Method.Name()) {
		return ""
	}
	named, ok := deref(c.Value.Type()).(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return ""
	}
	p := named.Obj().Pkg().Path()
	if strings.Contains(p, "-client") || strings.HasSuffix(p, "/api") || strings.Contains(p, "/api2") {
		return serviceFromPkgPath(p) + ":" + c.Method.Name()
	}
	return ""
}

// serviceFromPkgPath derives a service name from a package import path, e.g.
// "github.com/devrev/schemaregistry-client/lib" -> "schemaregistry".
func serviceFromPkgPath(p string) string {
	best := ""
	for _, seg := range strings.Split(p, "/") {
		if strings.HasSuffix(seg, "-client") {
			return strings.TrimSuffix(seg, "-client")
		}
		if seg != "" && !strings.Contains(seg, ".") {
			best = seg
		}
	}
	if best == "" {
		best = "service"
	}
	return best
}

// addrPath walks a FieldAddr/IndexAddr chain and returns the root object and
// the field names from root to leaf (index steps are flattened).
func addrPath(v ssa.Value) (ssa.Value, []string) {
	switch x := v.(type) {
	case *ssa.FieldAddr:
		root, fields := addrPath(x.X)
		if name := fieldNameAt(x.X.Type(), x.Field); name != "" {
			fields = append(fields, name)
		}
		return root, fields
	case *ssa.IndexAddr:
		return addrPath(x.X)
	case *ssa.UnOp:
		if x.Op == token.MUL {
			return addrPath(x.X)
		}
	}
	return v, nil
}

// allocRoot resolves a value to its backing alloc, so stores recorded on the
// alloc are visible when the alloc (or a load of it) is later used.
func allocRoot(v ssa.Value) ssa.Value {
	switch x := v.(type) {
	case *ssa.Alloc:
		return x
	case *ssa.UnOp:
		if x.Op == token.MUL {
			return allocRoot(x.X)
		}
	case *ssa.FieldAddr:
		return nil
	}
	return nil
}

func fieldNameAt(t types.Type, idx int) string {
	st := underlyingStruct(t)
	if st == nil || idx < 0 || idx >= st.NumFields() {
		return ""
	}
	return st.Field(idx).Name()
}

func outSignature(fn *ssa.Function, seed map[int]depval) string {
	var sb strings.Builder
	sb.WriteString(fn.String())
	idx := make([]int, 0, len(seed))
	for i := range seed {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		sb.WriteString("|")
		sb.WriteString(itoa(i))
		rels := make([]string, 0, len(seed[i]))
		for rel := range seed[i] {
			rels = append(rels, rel)
		}
		sort.Strings(rels)
		sb.WriteString(strings.Join(rels, ","))
	}
	return sb.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
