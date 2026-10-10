package main

import (
	"encoding/json"
	"os"
)

// FieldMap is a reviewable HTTP-body field -> RPC request field mapping. Keys
// and values are field leaf names (matched case/style-insensitively). Global
// applies to every RPC; ByRPC entries override/add per RPC.
type FieldMap struct {
	Global map[string]string            `json:"global"`
	ByRPC  map[string]map[string]string `json:"by_rpc"`
}

// loadFieldMap reads a field map JSON file. A missing file is not an error.
func loadFieldMap(path string) (FieldMap, bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return FieldMap{}, false, nil
		}
		return FieldMap{}, true, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FieldMap{}, true, err
	}
	var fm FieldMap
	if err := json.Unmarshal(data, &fm); err != nil {
		return fm, true, err
	}
	return fm, true, nil
}

// effectiveAliases merges global, RPC-specific, and command-line mappings.
func effectiveAliases(fm FieldMap, rpc string, cliAliases map[string]string) map[string]string {
	aliases := make(map[string]string, len(fm.Global)+len(fm.ByRPC[rpc])+len(cliAliases))
	for key, value := range fm.Global {
		aliases[key] = value
	}
	for key, value := range fm.ByRPC[rpc] {
		aliases[key] = value
	}
	for key, value := range cliAliases {
		aliases[key] = value
	}
	return aliases
}

// writeScaffold emits a field-map scaffold for unmatched test fields.
func writeScaffold(path string, reports []CoverageReport) error {
	byRPC := map[string]map[string]string{}
	review := map[string]map[string][]string{}
	for _, report := range reports {
		for _, unmatched := range report.Anomalies.UnmatchedTestFields {
			if byRPC[report.RPC] == nil {
				byRPC[report.RPC] = map[string]string{}
				review[report.RPC] = map[string][]string{}
			}
			best := ""
			if len(unmatched.Suggestion) > 0 {
				best = lastComponent(unmatched.Suggestion[0])
			}
			byRPC[report.RPC][unmatched.Field] = best
			suggestions := make([]string, 0, len(unmatched.Suggestion))
			for _, suggestion := range unmatched.Suggestion {
				suggestions = append(suggestions, lastComponent(suggestion))
			}
			review[report.RPC][unmatched.Field] = suggestions
		}
	}
	payload := struct {
		ByRPC  map[string]map[string]string   `json:"by_rpc"`
		Review map[string]map[string][]string `json:"_review"`
	}{ByRPC: byRPC, Review: review}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
