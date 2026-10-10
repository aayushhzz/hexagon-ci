package main

import (
	"fmt"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

var dbg = os.Getenv("PROV_DEBUG") != ""

func dprintf(format string, args ...any) {
	if dbg {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

// pathSet is a set of request-rooted access paths, e.g. {"in.DevOid"}.
type pathSet map[string]bool

func (p pathSet) clone() pathSet {
	q := make(pathSet, len(p))
	for k := range p {
		q[k] = true
	}
	return q
}
func (p pathSet) union(q pathSet) bool {
	changed := false
	for k := range q {
		if !p[k] {
			p[k] = true
			changed = true
		}
	}
	return changed
}
func (p pathSet) appendField(field string) pathSet {
	q := make(pathSet, len(p))
	for k := range p {
		if isContainer(k) {
			q[k] = true // container reachability does not field-expand
			continue
		}
		q[k+"."+field] = true
	}
	return q
}

// Container-reachability marker. A path prefixed with cmark means "this object
// was built using the given request field" and is used only to flag sinks. It
// is excluded from input_paths and branches and never field-expands.
const cmark = "~"

func isContainer(p string) bool { return strings.HasPrefix(p, cmark) }
func stripMark(p string) string { return strings.TrimPrefix(p, cmark) }

func containerize(p pathSet) pathSet {
	q := make(pathSet, len(p))
	for k := range p {
		if isContainer(k) {
			q[k] = true
		} else {
			q[cmark+k] = true
		}
	}
	return q
}

func nonContainer(p pathSet) pathSet {
	q := pathSet{}
	for k := range p {
		if !isContainer(k) {
			q[k] = true
		}
	}
	return q
}

func (p pathSet) displayStripped() []string {
	seen := map[string]bool{}
	for k := range p {
		seen[stripMark(k)] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func (p pathSet) sorted() []string {
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// analyzer runs field-sensitive taint for one entrypoint.
type analyzer struct {
	l       *loaded
	dir     string
	cur     *Provenance
	inputs  map[string]map[string]bool // path -> set of kinds
	seenBr  map[string]bool
	seenSk  map[string]bool
	seenErr   map[string]bool
	visited   map[string]bool            // fn+seed signature guard (cycle breaker)
	summaries map[string]map[int]pathSet // fn+seed signature -> result taint
	depth     int
}

func (l *loaded) analyze(ep entrypoint, dir string) *Provenance {
	a := &analyzer{
		l:       l,
		dir:     dir,
		cur:     &Provenance{RPC: ep.rpcName, Func: ep.fn.String(), RequestType: typeStr(ep.requestType), ResponseType: typeStr(ep.responseType)},
		inputs:  map[string]map[string]bool{},
		seenBr:  map[string]bool{},
		seenSk:  map[string]bool{},
		seenErr:   map[string]bool{},
		visited:   map[string]bool{},
		summaries: map[string]map[int]pathSet{},
	}
	rp := requestParam(ep.fn)
	if rp == nil {
		return a.cur
	}
	seed := map[*ssa.Parameter]pathSet{rp: {"in": true}}
	a.analyzeFunc(ep.fn, seedToIndex(ep.fn, seed))
	// Materialize input paths.
	for p, kinds := range a.inputs {
		ks := make([]string, 0, len(kinds))
		for k := range kinds {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		a.cur.InputPaths = append(a.cur.InputPaths, InputPath{Path: p, Kinds: ks})
	}
	sort.Slice(a.cur.InputPaths, func(i, j int) bool { return a.cur.InputPaths[i].Path < a.cur.InputPaths[j].Path })
	return a.cur
}

func seedToIndex(fn *ssa.Function, seed map[*ssa.Parameter]pathSet) map[int]pathSet {
	out := map[int]pathSet{}
	for i, p := range fn.Params {
		if ps, ok := seed[p]; ok {
			out[i] = ps
		}
	}
	return out
}

func (a *analyzer) markInput(ps pathSet, kind string) {
	for p := range ps {
		if isContainer(p) {
			continue
		}
		if a.inputs[p] == nil {
			a.inputs[p] = map[string]bool{}
		}
		a.inputs[p][kind] = true
	}
}

// analyzeFunc computes taint within fn given seeded parameter paths and records
// branches, sinks, and errors. It returns a summary: result index -> taint, so
// callers can propagate taint through this function's return values precisely.
func (a *analyzer) analyzeFunc(fn *ssa.Function, seedIdx map[int]pathSet) map[int]pathSet {
	if fn == nil || len(fn.Blocks) == 0 {
		return nil
	}
	sig := signature(fn, seedIdx)
	if s, ok := a.summaries[sig]; ok {
		return s
	}
	if a.visited[sig] || a.depth > 60 {
		return nil // break recursion conservatively
	}
	a.visited[sig] = true
	a.depth++
	defer func() { a.depth-- }()

	taint := map[ssa.Value]pathSet{}
	tuple := map[ssa.Value]map[int]pathSet{}
	for i, ps := range seedIdx {
		if i < len(fn.Params) {
			taint[fn.Params[i]] = ps.clone()
		}
	}

	// Fixpoint over instructions (bounded iterations).
	maxIters := len(fn.Blocks)*2 + 4
	for iter := 0; iter < maxIters; iter++ {
		changed := false
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if a.step(fn, instr, taint, tuple) {
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	// Record control flow and exits after taint stabilizes.
	taintedIfBlocks := map[int]bool{}
	for _, b := range fn.Blocks {
		if len(b.Instrs) == 0 {
			continue
		}
		if ifi, ok := b.Instrs[len(b.Instrs)-1].(*ssa.If); ok {
			if ps, ok := taint[ifi.Cond]; ok {
				precise := nonContainer(ps)
				if len(precise) > 0 {
					taintedIfBlocks[b.Index] = true
					a.markInput(precise, "predicate")
					a.recordBranch(ifi, precise, taint)
				}
			}
		}
	}
	summary := map[int]pathSet{}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if ret, ok := instr.(*ssa.Return); ok {
				a.recordReturn(b, ret, taintedIfBlocks)
				for i, r := range ret.Results {
					if ps, ok := taint[r]; ok && len(ps) > 0 {
						if summary[i] == nil {
							summary[i] = pathSet{}
						}
						summary[i].union(ps)
					}
				}
			}
		}
	}
	a.summaries[sig] = summary
	return summary
}

// step propagates taint for a single instruction. Returns true if taint grew.
func (a *analyzer) step(fn *ssa.Function, instr ssa.Instruction, taint map[ssa.Value]pathSet, tuple map[ssa.Value]map[int]pathSet) bool {
	set := func(v ssa.Value, ps pathSet) bool {
		if len(ps) == 0 {
			return false
		}
		if taint[v] == nil {
			taint[v] = pathSet{}
		}
		return taint[v].union(ps)
	}
	switch v := instr.(type) {
	case *ssa.FieldAddr:
		if src, ok := taint[v.X]; ok {
			st := underlyingStruct(v.X.Type())
			if st != nil && v.Field < st.NumFields() {
				if isProtoType(v.X.Type()) {
					res := src.appendField(st.Field(v.Field).Name())
					a.markInput(res, "read")
					return set(v, res)
				}
				// Non-proto struct (stdlib/DON internals): keep reachability for
				// sinks but stop expanding request access paths.
				return set(v, containerize(src))
			}
		}
	case *ssa.Field:
		if src, ok := taint[v.X]; ok {
			st := underlyingStruct(v.X.Type())
			if st != nil && v.Field < st.NumFields() {
				if isProtoType(v.X.Type()) {
					res := src.appendField(st.Field(v.Field).Name())
					a.markInput(res, "read")
					return set(v, res)
				}
				return set(v, containerize(src))
			}
		}
	case *ssa.UnOp:
		// Any unary op (load *p, logical !, negation, ^) yields a value derived
		// from its operand, so taint flows through. Restricting this to loads
		// dropped taint on predicates like `!in.ResolvePrimary`, hiding real
		// service-logic branches that gate on them.
		if src, ok := taint[v.X]; ok {
			return set(v, src)
		}
	case *ssa.Phi:
		grew := false
		for _, e := range v.Edges {
			if src, ok := taint[e]; ok {
				grew = set(v, src) || grew
			}
		}
		return grew
	case *ssa.BinOp:
		grew := false
		if src, ok := taint[v.X]; ok {
			grew = set(v, src) || grew
		}
		if src, ok := taint[v.Y]; ok {
			grew = set(v, src) || grew
		}
		return grew
	case *ssa.ChangeType:
		if src, ok := taint[v.X]; ok {
			return set(v, src)
		}
	case *ssa.Convert:
		if src, ok := taint[v.X]; ok {
			return set(v, src)
		}
	case *ssa.MakeInterface:
		if src, ok := taint[v.X]; ok {
			return set(v, src)
		}
	case *ssa.TypeAssert:
		if src, ok := taint[v.X]; ok {
			return set(v, src)
		}
	case *ssa.Extract:
		if tt, ok := tuple[v.Tuple]; ok {
			if ps, ok := tt[v.Index]; ok {
				return set(v, ps)
			}
			return false // tuple tracked per-index: absent index is untainted
		}
		if src, ok := taint[v.Tuple]; ok {
			return set(v, src)
		}
	case *ssa.Store:
		// A store into a struct/array field taints the containing object
		// (container reachability) so query documents built from request fields
		// reach DB/RPC sinks. A store to a plain local/alloc is a precise
		// variable assignment (including SSA parameter spills), so it keeps the
		// exact request-rooted taint.
		if src, ok := taint[v.Val]; ok && len(src) > 0 {
			switch v.Addr.(type) {
			case *ssa.FieldAddr, *ssa.IndexAddr:
				return set(rootAddr(v.Addr), containerize(src))
			default:
				return set(v.Addr, src)
			}
		}
	case *ssa.Call:
		return a.stepCall(v, &v.Call, taint, tuple)
	}
	return false
}

// stepCall handles getters (field access), interprocedural following, and sinks.
// callVal is the SSA value produced by the call (a single value or a tuple).
func (a *analyzer) stepCall(callVal ssa.Value, c *ssa.CallCommon, taint map[ssa.Value]pathSet, tuple map[ssa.Value]map[int]pathSet) bool {
	setSingle := func(ps pathSet) bool {
		if len(ps) == 0 {
			return false
		}
		if taint[callVal] == nil {
			taint[callVal] = pathSet{}
		}
		return taint[callVal].union(ps)
	}
	// Interface dispatch (VTA resolution is future work). Record a sink when
	// the receiver OR any argument is tainted, classified by method/receiver.
	if c.IsInvoke() {
		via := pathSet{}
		if ps, ok := taint[c.Value]; ok {
			via.union(ps)
		}
		for _, arg := range c.Args {
			if ps, ok := taint[arg]; ok {
				via.union(ps)
			}
		}
		if len(via) > 0 && !isPlumbingMethod(c.Method.Name()) {
			a.recordSink(classifyInvoke(c), c.Method.Name(), via, callVal.(ssa.Instruction))
		}
		return false
	}
	callee := c.StaticCallee()
	if callee == nil {
		return false
	}
	// Proto getter: canonicalize as a field read, do not follow.
	if field, isGetter := protoGetter(callee); isGetter && len(c.Args) >= 1 {
		if src, ok := taint[c.Args[0]]; ok {
			if field == "" { // wrapper .GetValue collapse
				return setSingle(src)
			}
			res := src.appendField(field)
			a.markInput(res, "read")
			return setSingle(res)
		}
		return false
	}
	// Collect tainted args and their callee parameter indices.
	calleeSeed := map[int]pathSet{}
	anyTainted := false
	for i, arg := range c.Args {
		if src, ok := taint[arg]; ok && len(src) > 0 {
			anyTainted = true
			calleeSeed[i] = src.clone()
		}
	}
	if !anyTainted {
		return false
	}
	// Plumbing (fmt/errors/log/grpc status): neither a sink nor taint-propagating.
	if isPlumbing(callee) {
		return false
	}
	via := pathSet{}
	for _, ps := range calleeSeed {
		via.union(ps)
	}
	if len(callee.Blocks) > 0 {
		dprintf("follow %s seed=%v\n", callee.Name(), calleeSeed)
		// Follow repo-local function and propagate its return taint precisely.
		paramSeed := map[int]pathSet{}
		for i, ps := range calleeSeed {
			if i < len(callee.Params) {
				paramSeed[i] = ps
			}
		}
		summary := a.analyzeFunc(callee, paramSeed)
		ctor := isConstructorLike(callee)
		if callee.Signature.Results().Len() > 1 {
			if tuple[callVal] == nil {
				tuple[callVal] = map[int]pathSet{}
			}
			grew := false
			for i, ps := range summary {
				if tuple[callVal][i] == nil {
					tuple[callVal][i] = pathSet{}
				}
				if tuple[callVal][i].union(ps) {
					grew = true
				}
			}
			// Constructor/converter: ensure provenance reaches the primary
			// result when the body summary could not track it precisely.
			if ctor {
				if tuple[callVal][0] == nil {
					tuple[callVal][0] = pathSet{}
				}
				if tuple[callVal][0].union(via) {
					grew = true
				}
			}
			return grew
		}
		res := summary[0]
		if len(res) == 0 && ctor {
			res = via
		}
		return setSingle(res)
	}
	// Bodyless (external) callee reached with tainted input: a sink.
	a.recordSink(classifySink(callee), callee.String(), via, callVal.(ssa.Instruction))
	// Propagate through constructors/converters only, so provenance reaches
	// later DB/RPC sinks (e.g. NewDevOrgDON) without flooding error plumbing.
	if isConstructorLike(callee) {
		return setSingle(via)
	}
	return false
}

func (a *analyzer) recordBranch(ifi *ssa.If, ps pathSet, taint map[ssa.Value]pathSet) {
	cond := a.renderCond(ifi.Cond, taint)
	at := a.pos(ifi.Cond.Pos())
	key := at + "|" + cond
	if a.seenBr[key] {
		return
	}
	a.seenBr[key] = true
	a.cur.Branches = append(a.cur.Branches, Branch{
		ID:             fmt.Sprintf("b%d", len(a.cur.Branches)+1),
		At:             at,
		Cond:           cond,
		PredicatePaths: ps.sorted(),
	})
}

func (a *analyzer) recordSink(kind, target string, via pathSet, instr ssa.Instruction) {
	// Suppress unknown (dispatch) sinks that are only container-reachable: they
	// are object-method noise, not real data sinks. Keep db/rpc regardless.
	if kind == "dispatch" && len(nonContainer(via)) == 0 {
		return
	}
	dprintf("sink? %s %s via=%v\n", kind, target, via.sorted())
	at := a.pos(instr.Pos())
	key := kind + "|" + target + "|" + at
	if a.seenSk[key] {
		return
	}
	a.seenSk[key] = true
	a.cur.Sinks = append(a.cur.Sinks, Sink{Kind: kind, Target: shortName(target), ViaPaths: via.displayStripped(), At: at})
}

func (a *analyzer) recordReturn(b *ssa.BasicBlock, ret *ssa.Return, taintedIf map[int]bool) {
	for _, r := range ret.Results {
		name := sentinelName(r)
		if name == "" {
			continue
		}
		at := a.pos(ret.Pos())
		key := name + "|" + at
		if a.seenErr[key] {
			continue
		}
		a.seenErr[key] = true
		a.cur.Errors = append(a.cur.Errors, ErrorExit{Symbol: name, At: at, Tainted: dominatedByTainted(b, taintedIf)})
	}
}

// ---- helpers ----

func (a *analyzer) renderCond(v ssa.Value, taint map[ssa.Value]pathSet) string {
	// Render structural operators first so comparisons read naturally.
	if bo, ok := v.(*ssa.BinOp); ok {
		return a.renderCond(bo.X, taint) + " " + bo.Op.String() + " " + a.renderCond(bo.Y, taint)
	}
	if ps, ok := taint[v]; ok && len(ps) > 0 {
		return strings.Join(ps.displayStripped(), "|")
	}
	switch x := v.(type) {
	case *ssa.UnOp:
		return x.Op.String() + a.renderCond(x.X, taint)
	case *ssa.Const:
		return constStr(x)
	case *ssa.Global:
		return x.Name()
	case *ssa.Call:
		if sc := x.Call.StaticCallee(); sc != nil {
			return shortName(sc.String()) + "()"
		}
		return "call()"
	default:
		if v == nil {
			return "?"
		}
		return v.Name()
	}
}

func (a *analyzer) pos(p token.Pos) string {
	if p == token.NoPos {
		return ""
	}
	pos := a.l.prog.Fset.Position(p)
	f := pos.Filename
	if a.dir != "" && strings.HasPrefix(f, a.dir) {
		f = strings.TrimPrefix(strings.TrimPrefix(f, a.dir), "/")
	}
	return fmt.Sprintf("%s:%d", f, pos.Line)
}

func signature(fn *ssa.Function, seed map[int]pathSet) string {
	var sb strings.Builder
	sb.WriteString(fn.String())
	idx := make([]int, 0, len(seed))
	for i := range seed {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		sb.WriteString(fmt.Sprintf("|%d:%s", i, strings.Join(seed[i].sorted(), ",")))
	}
	return sb.String()
}

func underlyingStruct(t types.Type) *types.Struct {
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Named:
			t = x.Underlying()
		case *types.Struct:
			return x
		default:
			return nil
		}
	}
}

// protoGetter reports whether fn is a generated proto getter GetXxx() and the
// field name it reads (empty string means a wrapper .GetValue collapse).
func protoGetter(fn *ssa.Function) (string, bool) {
	if fn.Signature.Recv() == nil {
		return "", false
	}
	name := fn.Name()
	if !strings.HasPrefix(name, "Get") || len(name) == 3 {
		return "", false
	}
	if fn.Signature.Params().Len() != 0 || fn.Signature.Results().Len() != 1 {
		return "", false
	}
	recv := fn.Signature.Recv().Type()
	if isWrapper(recv) && name == "GetValue" {
		return "", true
	}
	return strings.TrimPrefix(name, "Get"), true
}

func isWrapper(t types.Type) bool {
	named, ok := deref(t).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return strings.Contains(named.Obj().Pkg().Path(), "protobuf/types/known/wrapperspb")
}

// calleePkgPath returns the import path of a callee's declaring or receiver pkg.
func calleePkgPath(fn *ssa.Function) string {
	if fn.Pkg != nil && fn.Pkg.Pkg != nil {
		return fn.Pkg.Pkg.Path()
	}
	if fn.Signature.Recv() != nil {
		if named, ok := deref(fn.Signature.Recv().Type()).(*types.Named); ok && named.Obj().Pkg() != nil {
			return named.Obj().Pkg().Path()
		}
	}
	return ""
}

func classifySink(fn *ssa.Function) string {
	path := calleePkgPath(fn)
	switch {
	case strings.Contains(path, "mongo") || strings.Contains(path, "collection") || strings.Contains(path, "/db") || strings.Contains(path, "dbclient"):
		return "db"
	case strings.Contains(path, "-client") || strings.HasSuffix(path, "/api") || strings.Contains(path, "/api2"):
		return "rpc"
	default:
		return "opaque"
	}
}

// isPlumbing reports whether a callee is error/format/log/grpc machinery that
// should neither be recorded as a sink nor propagate taint.
func isPlumbing(fn *ssa.Function) bool {
	p := calleePkgPath(fn)
	switch {
	case p == "fmt" || p == "errors" || p == "strings" || p == "strconv" || p == "sort" || p == "reflect" || p == "unsafe" || p == "runtime" || p == "sync" || p == "sync/atomic" || p == "time":
		return true
	case strings.HasPrefix(p, "google.golang.org/grpc") || strings.HasPrefix(p, "google.golang.org/protobuf") || strings.HasPrefix(p, "internal/"):
		return true
	case strings.Contains(p, "/log") || strings.HasSuffix(p, "/errors/types") || strings.Contains(p, "shared/log"):
		return true
	}
	return false
}

// isProtoType reports whether t is a generated protobuf message. Field reads on
// proto messages are request access paths; reads on other structs are not.
func isProtoType(t types.Type) bool {
	named, ok := deref(t).(*types.Named)
	if !ok {
		return false
	}
	if named.Obj().Pkg() != nil {
		p := named.Obj().Pkg().Path()
		if strings.Contains(p, "-client") {
			return true
		}
	}
	ms := types.NewMethodSet(types.NewPointer(named))
	return ms.Lookup(nil, "ProtoReflect") != nil
}

// isPlumbingMethod skips error/stringer interface methods for dispatch sinks.
func isPlumbingMethod(name string) bool {
	switch name {
	case "Error", "Unwrap", "Is", "As", "String", "GRPCStatus":
		return true
	}
	return false
}

// isConstructorLike reports whether taint should flow through a bodyless
// callee's result (constructors/converters), so provenance reaches later sinks.
func isConstructorLike(fn *ssa.Function) bool {
	n := fn.Name()
	for _, pre := range []string{"New", "Parse", "To", "From", "Make", "Must", "Build"} {
		if strings.HasPrefix(n, pre) {
			return true
		}
	}
	// Single non-error result is likely a pure converter.
	if fn.Signature.Results().Len() == 1 {
		r := fn.Signature.Results().At(0).Type()
		if named, ok := r.(*types.Named); !ok || named.Obj().Name() != "error" {
			return true
		}
	}
	return false
}

func sentinelName(v ssa.Value) string {
	switch x := v.(type) {
	case *ssa.MakeInterface:
		return sentinelName(x.X)
	case *ssa.UnOp:
		if x.Op == token.MUL {
			if g, ok := x.X.(*ssa.Global); ok {
				return g.Name()
			}
		}
	case *ssa.Global:
		return x.Name()
	}
	return ""
}

func dominatedByTainted(b *ssa.BasicBlock, taintedIf map[int]bool) bool {
	for cur := b; cur != nil; cur = cur.Idom() {
		if taintedIf[cur.Index] {
			return true
		}
	}
	return false
}

// classifyInvoke labels an interface-dispatched sink using the method name and
// receiver interface type.
func classifyInvoke(c *ssa.CallCommon) string {
	m := c.Method.Name()
	if looksDB(m) {
		return "db"
	}
	name := ""
	if named, ok := deref(c.Value.Type()).(*types.Named); ok {
		name = named.Obj().Name()
		if named.Obj().Pkg() != nil {
			p := named.Obj().Pkg().Path()
			if strings.Contains(p, "-client") || strings.HasSuffix(p, "/api") || strings.Contains(p, "/api2") {
				return "rpc"
			}
		}
	}
	switch {
	case strings.Contains(name, "DB") || strings.Contains(name, "Cache") || strings.Contains(name, "Collection") || strings.Contains(name, "Store"):
		return "db"
	case strings.Contains(name, "Client") || strings.Contains(name, "Service"):
		return "rpc"
	}
	return "dispatch"
}

func looksDB(method string) bool {
	switch method {
	case "Read", "ReadWithFilter", "ReadMany", "Find", "FindOne", "Insert", "InsertOne", "InsertMany",
		"Update", "UpdateOne", "UpdateMany", "ReplaceOne", "Delete", "DeleteOne", "DeleteMany",
		"Count", "Aggregate", "BulkWrite", "Scan", "Get", "Set", "LockForRead", "Upsert":
		return true
	}
	return false
}

// rootAddr walks FieldAddr/IndexAddr chains to the underlying allocated object.
func rootAddr(v ssa.Value) ssa.Value {
	for {
		switch x := v.(type) {
		case *ssa.FieldAddr:
			v = x.X
		case *ssa.IndexAddr:
			v = x.X
		default:
			return v
		}
	}
}

func deref(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

func typeStr(t types.Type) string {
	if t == nil {
		return ""
	}
	return shortName(t.String())
}

func shortName(s string) string {
	// Trim long module path prefixes for readability, keep last two segments.
	s = strings.TrimPrefix(s, "*")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func constStr(c *ssa.Const) string {
	if c.Value == nil {
		return "nil"
	}
	return c.Value.String()
}
