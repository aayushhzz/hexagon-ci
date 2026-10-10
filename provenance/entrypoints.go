package main

import (
	"fmt"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// entrypoint is one RPC: an interface method implemented by the concrete server.
type entrypoint struct {
	rpcName      string // interface method name, e.g. GetDevOrg
	fn           *ssa.Function
	requestType  types.Type // *XxxRequest
	responseType types.Type // *XxxResponse
}

// serverSpec identifies a gRPC server registration to enumerate.
type serverSpec struct {
	ifacePkg  string // e.g. github.com/devrev/janus-client/api
	ifaceName string // e.g. JanusServer
	implPkg   string // e.g. github.com/devrev/janus/internal/service
	implName  string // e.g. Janus (pointer receiver assumed)
}

// enumerate resolves every interface method to the concrete *ssa.Function on
// the implementing type. filter, if non-empty, restricts to those RPC names.
func (l *loaded) enumerate(spec serverSpec, filter map[string]bool) ([]entrypoint, error) {
	ifaceNamed, err := l.lookupType(spec.ifacePkg, spec.ifaceName)
	if err != nil {
		return nil, err
	}
	iface, ok := ifaceNamed.Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("%s.%s is not an interface", spec.ifacePkg, spec.ifaceName)
	}
	implNamed, err := l.lookupType(spec.implPkg, spec.implName)
	if err != nil {
		return nil, err
	}
	recv := types.NewPointer(implNamed)
	mset := l.prog.MethodSets.MethodSet(recv)

	var out []entrypoint
	for i := 0; i < iface.NumMethods(); i++ {
		m := iface.Method(i)
		name := m.Name()
		if len(filter) > 0 && !filter[name] {
			continue
		}
		sel := mset.Lookup(m.Pkg(), name)
		if sel == nil {
			continue // concrete type does not implement this method directly
		}
		fn := l.prog.MethodValue(sel)
		if fn == nil {
			continue
		}
		req, resp := requestResponseTypes(fn)
		out = append(out, entrypoint{rpcName: name, fn: fn, requestType: req, responseType: resp})
	}
	return out, nil
}

// requestResponseTypes extracts the (request, response) types from an RPC
// signature func(ctx, *Req) (*Resp, error). Receiver is Params[0] on methods.
func requestResponseTypes(fn *ssa.Function) (req, resp types.Type) {
	sig := fn.Signature
	params := sig.Params()
	// Signature params exclude the receiver; request is the last param.
	if params.Len() >= 2 {
		req = params.At(params.Len() - 1).Type()
	}
	if sig.Results().Len() >= 1 {
		resp = sig.Results().At(0).Type()
	}
	return req, resp
}

// requestParam returns the ssa.Value for the request message parameter.
func requestParam(fn *ssa.Function) *ssa.Parameter {
	// fn.Params includes the receiver at index 0 for methods.
	// Layout: [recv, ctx, in]. The request is the last parameter.
	if len(fn.Params) == 0 {
		return nil
	}
	return fn.Params[len(fn.Params)-1]
}
