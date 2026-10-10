package hexagon

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

var hunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(,([0-9]+))? \+([0-9]+)(,([0-9]+))? @@`)

// ParseUnifiedDiff parses a Git unified diff into repository-relative file
// paths and one-based old/new hunk ranges. It accepts the zero-context diffs
// emitted by git diff --unified=0 as well as ordinary unified diffs.
func ParseUnifiedDiff(reader io.Reader) (ParsedDiff, error) {
	if reader == nil {
		return ParsedDiff{}, fmt.Errorf("unified diff reader is required")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	parsed := ParsedDiff{Files: make([]DiffFile, 0)}
	current := -1
	inHunk := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "diff --git ") {
			oldPath, newPath, err := parseDiffGitHeader(strings.TrimPrefix(line, "diff --git "))
			if err != nil {
				return ParsedDiff{}, err
			}
			parsed.Files = append(parsed.Files, DiffFile{OldPath: oldPath, NewPath: newPath, Hunks: make([]DiffHunk, 0)})
			current = len(parsed.Files) - 1
			inHunk = false
			continue
		}
		if current < 0 {
			continue
		}
		if !inHunk {
			switch {
			case strings.HasPrefix(line, "--- "):
				path, err := parsePatchPath(strings.TrimPrefix(line, "--- "), "a/")
				if err != nil {
					return ParsedDiff{}, err
				}
				parsed.Files[current].OldPath = path
				continue
			case strings.HasPrefix(line, "+++ "):
				path, err := parsePatchPath(strings.TrimPrefix(line, "+++ "), "b/")
				if err != nil {
					return ParsedDiff{}, err
				}
				parsed.Files[current].NewPath = path
				continue
			}
		}
		if strings.HasPrefix(line, "@@ ") {
			hunk, err := parseHunkHeader(line)
			if err != nil {
				return ParsedDiff{}, err
			}
			parsed.Files[current].Hunks = append(parsed.Files[current].Hunks, hunk)
			inHunk = true
		}
	}
	if err := scanner.Err(); err != nil {
		return ParsedDiff{}, fmt.Errorf("read unified diff: %w", err)
	}
	if len(parsed.Files) == 0 {
		return ParsedDiff{}, fmt.Errorf("unified diff contains no changed files")
	}
	for index, file := range parsed.Files {
		if file.OldPath == "" && file.NewPath == "" {
			return ParsedDiff{}, fmt.Errorf("diff file %d has no repository-relative path", index)
		}
	}
	return parsed, nil
}

func parseDiffGitHeader(value string) (string, string, error) {
	oldPath, rest, err := readGitPathToken(value)
	if err != nil {
		return "", "", fmt.Errorf("parse diff --git old path: %w", err)
	}
	newPath, rest, err := readGitPathToken(rest)
	if err != nil {
		return "", "", fmt.Errorf("parse diff --git new path: %w", err)
	}
	if strings.TrimSpace(rest) != "" {
		return "", "", fmt.Errorf("unexpected data in diff --git header %q", value)
	}
	return stripDiffPrefix(oldPath, "a/"), stripDiffPrefix(newPath, "b/"), nil
}

func parsePatchPath(value, prefix string) (string, error) {
	path, _, err := readGitPathToken(value)
	if err != nil {
		return "", fmt.Errorf("parse patch path: %w", err)
	}
	return stripDiffPrefix(path, prefix), nil
}

func readGitPathToken(value string) (string, string, error) {
	value = strings.TrimLeft(value, " \t")
	if value == "" {
		return "", "", fmt.Errorf("path token is empty")
	}
	if value[0] != '"' {
		end := strings.IndexAny(value, " \t")
		if end < 0 {
			return value, "", nil
		}
		return value[:end], value[end:], nil
	}
	for index := 1; index < len(value); index++ {
		if value[index] == '\\' {
			index++
			continue
		}
		if value[index] == '"' {
			decoded, err := strconv.Unquote(value[:index+1])
			if err != nil {
				return "", "", fmt.Errorf("decode quoted Git path: %w", err)
			}
			return decoded, value[index+1:], nil
		}
	}
	return "", "", fmt.Errorf("unterminated quoted Git path")
}

func stripDiffPrefix(value, prefix string) string {
	if value == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(value, prefix)
}

func parseHunkHeader(line string) (DiffHunk, error) {
	match := hunkHeader.FindStringSubmatch(line)
	if match == nil {
		return DiffHunk{}, fmt.Errorf("malformed unified diff hunk header %q", line)
	}
	oldStart, err := strconv.Atoi(match[1])
	if err != nil {
		return DiffHunk{}, fmt.Errorf("invalid old hunk start in %q: %w", line, err)
	}
	oldLines, err := hunkCount(match[3])
	if err != nil {
		return DiffHunk{}, fmt.Errorf("invalid old hunk count in %q: %w", line, err)
	}
	newStart, err := strconv.Atoi(match[4])
	if err != nil {
		return DiffHunk{}, fmt.Errorf("invalid new hunk start in %q: %w", line, err)
	}
	newLines, err := hunkCount(match[6])
	if err != nil {
		return DiffHunk{}, fmt.Errorf("invalid new hunk count in %q: %w", line, err)
	}
	return DiffHunk{OldStart: oldStart, OldLines: oldLines, NewStart: newStart, NewLines: newLines}, nil
}

func hunkCount(raw string) (int, error) {
	if raw == "" {
		return 1, nil
	}
	return strconv.Atoi(raw)
}
