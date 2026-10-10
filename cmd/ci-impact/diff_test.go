package main

import (
	"strings"
	"testing"

	"github.com/aayushhzz/hexagon-ci/hexagon"
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
	got, err := parseUnifiedDiff(strings.NewReader(patch))
	if err != nil {
		t.Fatalf("parseUnifiedDiff() error = %v", err)
	}
	want := hexagon.ParsedDiff{Files: []hexagon.DiffFile{
		{OldPath: "pkg/changed.go", NewPath: "pkg/changed.go", Hunks: []hexagon.DiffHunk{{OldStart: 10, OldLines: 2, NewStart: 10, NewLines: 3}}},
		{OldPath: "", NewPath: "pkg/new.go", Hunks: []hexagon.DiffHunk{{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 2}}},
		{OldPath: "pkg/old.go", NewPath: "", Hunks: []hexagon.DiffHunk{{OldStart: 7, OldLines: 2, NewStart: 0, NewLines: 0}}},
	}}
	if len(got.Files) != len(want.Files) {
		t.Fatalf("parsed file count = %d, want %d", len(got.Files), len(want.Files))
	}
	for index := range want.Files {
		if got.Files[index].OldPath != want.Files[index].OldPath || got.Files[index].NewPath != want.Files[index].NewPath ||
			len(got.Files[index].Hunks) != 1 || got.Files[index].Hunks[0] != want.Files[index].Hunks[0] {
			t.Errorf("file %d = %+v, want %+v", index, got.Files[index], want.Files[index])
		}
	}
}

func TestParseUnifiedDiffHandlesQuotedPathsAndMultipleHunks(t *testing.T) {
	patch := strings.Join([]string{
		`diff --git "a/pkg/file with space.go" "b/pkg/file with space.go"`,
		`--- "a/pkg/file with space.go"`,
		`+++ "b/pkg/file with space.go"`,
		`@@ -1 +1 @@`,
		`-one`,
		`+two`,
		`@@ -20,0 +21,2 @@`,
		`+three`,
		`+four`,
	}, "\n")
	got, err := parseUnifiedDiff(strings.NewReader(patch))
	if err != nil {
		t.Fatalf("parseUnifiedDiff() error = %v", err)
	}
	if len(got.Files) != 1 || got.Files[0].OldPath != "pkg/file with space.go" || got.Files[0].NewPath != "pkg/file with space.go" {
		t.Fatalf("quoted paths parsed as %+v", got.Files)
	}
	want := []hexagon.DiffHunk{
		{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1},
		{OldStart: 20, OldLines: 0, NewStart: 21, NewLines: 2},
	}
	if len(got.Files[0].Hunks) != len(want) {
		t.Fatalf("hunks = %+v, want %+v", got.Files[0].Hunks, want)
	}
	for index := range want {
		if got.Files[0].Hunks[index] != want[index] {
			t.Errorf("hunk %d = %+v, want %+v", index, got.Files[0].Hunks[index], want[index])
		}
	}
}

func TestParseUnifiedDiffRejectsMalformedInput(t *testing.T) {
	for name, input := range map[string]string{
		"empty":    "",
		"bad hunk": "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ nope @@\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseUnifiedDiff(strings.NewReader(input)); err == nil {
				t.Fatal("parseUnifiedDiff() accepted malformed input")
			}
		})
	}
}
