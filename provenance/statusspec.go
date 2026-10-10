package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// statusSpec maps an API operation to the HTTP statuses documented for it. The
// source of truth is the generated oapi-codegen client: each `type <Op>Response
// struct` lists `JSON<code>` fields for every documented response code. This is
// authoritative, unlike inferring statuses from test assertions.

var (
	respStructRe = regexp.MustCompile(`^type ([A-Za-z0-9]+)Response struct \{`)
	jsonFieldRe  = regexp.MustCompile(`^\s*JSON(\d{3})\b`)
)

// parseStatusSpec reads op -> sorted documented statuses from a generated client.go.
func parseStatusSpec(path string) (map[string][]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	spec := map[string]map[int]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	var cur string
	for sc.Scan() {
		line := sc.Text()
		if m := respStructRe.FindStringSubmatch(line); m != nil {
			cur = m[1]
			if spec[cur] == nil {
				spec[cur] = map[int]bool{}
			}
			continue
		}
		if cur == "" {
			continue
		}
		if strings.HasPrefix(line, "}") {
			cur = ""
			continue
		}
		if m := jsonFieldRe.FindStringSubmatch(line); m != nil {
			code, _ := strconv.Atoi(m[1])
			spec[cur][code] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make(map[string][]int, len(spec))
	for op, codes := range spec {
		out[op] = sortedKeysInt(codes)
	}
	return out, nil
}

// locateStatusSpec ascends from the cover dir to find gen/v2_internal/client.go.
func locateStatusSpec(coverDir string) string {
	dir := coverDir
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "gen", "v2_internal", "client.go")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// successCodes returns the 2xx entries of a documented status list.
func successCodes(codes []int) []int {
	var out []int
	for _, c := range codes {
		if c >= 200 && c < 300 {
			out = append(out, c)
		}
	}
	sort.Ints(out)
	return out
}
