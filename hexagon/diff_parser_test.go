package hexagon

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseUnifiedDiffMapsModifiedAddedAndDeletedFiles(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/pkg/changed.go b/pkg/changed.go",
		"index 1111111..2222222 100644",
		"--- a/pkg/changed.go",
		"+++ b/pkg/changed.go",
		"@@ -10,2 +10,3 @@ func Changed() {",
		"- old",
		"+ new",
		"+ more",
		"diff --git a/pkg/new.go b/pkg/new.go",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/pkg/new.go",
		"@@ -0,0 +1,2 @@",
		"+package pkg",
		"+func Added() {}",
		"diff --git a/pkg/old.go b/pkg/old.go",
		"deleted file mode 100644",
		"--- a/pkg/old.go",
		"+++ /dev/null",
		"@@ -7,2 +0,0 @@",
		"-func Removed() {}",
	}, "\n")
	got, err := ParseUnifiedDiff(strings.NewReader(patch))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	want := ParsedDiff{Files: []DiffFile{
		{OldPath: "pkg/changed.go", NewPath: "pkg/changed.go", Hunks: []DiffHunk{{OldStart: 10, OldLines: 2, NewStart: 10, NewLines: 3}}},
		{OldPath: "", NewPath: "pkg/new.go", Hunks: []DiffHunk{{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 2}}},
		{OldPath: "pkg/old.go", NewPath: "", Hunks: []DiffHunk{{OldStart: 7, OldLines: 2, NewStart: 0, NewLines: 0}}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseUnifiedDiff() = %#v, want %#v", got, want)
	}
}

func TestParseUnifiedDiffHandlesQuotedPathsAndMultipleHunks(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git \"a/pkg/file with space.go\" \"b/pkg/file with space.go\"",
		"--- \"a/pkg/file with space.go\"",
		"+++ \"b/pkg/file with space.go\"",
		"@@ -1 +1 @@",
		"-one",
		"+two",
		"@@ -20,0 +21,2 @@",
		"+three",
		"+four",
	}, "\n")
	got, err := ParseUnifiedDiff(strings.NewReader(patch))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	want := ParsedDiff{Files: []DiffFile{{
		OldPath: "pkg/file with space.go", NewPath: "pkg/file with space.go",
		Hunks: []DiffHunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1}, {OldStart: 20, OldLines: 0, NewStart: 21, NewLines: 2}},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseUnifiedDiff() = %#v, want %#v", got, want)
	}
}

func TestParseUnifiedDiffRejectsMalformedInput(t *testing.T) {
	for name, input := range map[string]string{
		"empty":    "",
		"bad hunk": "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ nope @@\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseUnifiedDiff(strings.NewReader(input)); err == nil {
				t.Fatal("ParseUnifiedDiff() accepted malformed input")
			}
		})
	}
}
