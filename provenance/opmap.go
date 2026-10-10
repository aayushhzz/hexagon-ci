package main

import (
	"encoding/json"
	"os"
	"sort"
)

// opMapFile is the reviewed HTTP-operation -> janus-RPC mapping derived from
// gateway call sites (see docs/gateway-mapping.md). It is authoritative over
// the verb+resource heuristic; CLI -op-map still overrides individual entries.
type opMapFile struct {
	Service     string            `json:"service"`
	ByOperation map[string]string `json:"by_operation"`
	// Unresolved is informational only (operations seen but not mapped); its
	// shape varies by generator, so accept any JSON and ignore it.
	Unresolved json.RawMessage `json:"unresolved"`
}

// opMapOut is the emitted operation->RPC map (same shape loadOpMap reads).
type opMapOut struct {
	Service     string            `json:"service"`
	ByOperation map[string]string `json:"by_operation"`
	Unresolved  []string          `json:"unresolved"`
}

// loadOpMap reads by_operation from an op-map JSON file. A missing file yields
// an empty map and no error, so the flag can default to a path.
func loadOpMap(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	var m opMapFile
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	for op, rpc := range m.ByOperation {
		out[op] = rpc
	}
	return out, nil
}

// mergeStringMaps returns base overlaid by hi (hi wins), deterministically.
func mergeStringMaps(base, hi map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(hi))
	for _, k := range sortedStringKeys(base) {
		out[k] = base[k]
	}
	for _, k := range sortedStringKeys(hi) {
		out[k] = hi[k]
	}
	return out
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
