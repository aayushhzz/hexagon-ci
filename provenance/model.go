package main

// Provenance records emitted per RPC entrypoint. This is the deterministic
// "useful information" extracted from a backend service for coverage analysis.

type Provenance struct {
	RPC          string       `json:"rpc"`
	Func         string       `json:"func"`
	RequestType  string       `json:"request_type"`
	ResponseType string       `json:"response_type"`
	InputPaths   []InputPath  `json:"input_paths"`
	OutputPaths  []OutputPath `json:"output_paths"`
	Branches     []Branch     `json:"branches"`
	Sinks        []Sink       `json:"sinks"`
	Errors       []ErrorExit  `json:"errors"`
	Unresolved   []Unresolved `json:"unresolved"`
}

// OutputPath is a response-rooted field path that carries data derived from a
// downstream dependency call, e.g. {Path: "out.DevUser.CustomSchemaFragments",
// Dep: "schemaregistry:GetAggregatedSchema"}. Path "out" (no sub-fields) means
// the dependency result reaches the response but a precise field could not be
// resolved (an opaque response builder), so the signal is coarse there.
type OutputPath struct {
	Path string `json:"path"`
	Dep  string `json:"dep"`
}

type InputPath struct {
	Path  string   `json:"path"`
	Kinds []string `json:"kinds"`
}

type Branch struct {
	ID             string   `json:"id"`
	At             string   `json:"at"`
	Cond           string   `json:"cond"`
	PredicatePaths []string `json:"predicate_paths"`
}

type Sink struct {
	Kind     string   `json:"kind"` // db | rpc | opaque | dispatch
	Target   string   `json:"target"`
	ViaPaths []string `json:"via_paths"`
	At       string   `json:"at"`
}

type ErrorExit struct {
	Symbol  string `json:"symbol"`
	At      string `json:"at"`
	Tainted bool   `json:"tainted"`
}

type Unresolved struct {
	Reason string `json:"reason"`
	Expr   string `json:"expr"`
	At     string `json:"at"`
}
