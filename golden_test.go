package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func checkGolden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("golden %s: MkdirAll: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("golden %s: WriteFile: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s missing; run: go test -run TestGolden -update", name)
	}
	if got != string(want) {
		t.Errorf("golden %s mismatch:\n--- want ---\n%s--- got ---\n%s", name, want, got)
	}
}

// shadnullWarning is the stderr line every unscoped full-fixture scan emits:
// the fixture deliberately includes a table (shadnull) with every rowid
// alias shadowed and a nullable, non-INTEGER primary key, so it has no
// stable row identity and is skipped with a warning rather than an error —
// expected output on the two cases below that don't scope away from it.
const shadnullWarning = "warning: skipping shadnull: no stable row identity (rowid aliases shadowed, primary key nullable or absent)\n"

func TestGolden(t *testing.T) {
	path := fixturePath(t)
	cases := []struct {
		name       string
		argv       []string
		wantCode   int
		wantStderr string
	}{
		{"search_basic.txt", []string{"timeout", path}, exitMatch, shadnullWarning},
		{"search_json.txt", []string{"timeout", path, "--json", "-t", "events"}, exitMatch, ""},
		{"search_count.txt", []string{"timeout", path, "--count"}, exitMatch, shadnullWarning},
		{"tables.txt", []string{"--tables", path}, exitMatch, ""},
		{"schema_events.txt", []string{"--schema", path, "events"}, exitMatch, ""},
		{"fts_docs.txt", []string{"--fts", "timeout", path, "-t", "docs"}, exitMatch, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errOut, code := runCmd(t, c.argv...)
			if code != c.wantCode {
				t.Errorf("exit = %d, want %d", code, c.wantCode)
			}
			if errOut != c.wantStderr {
				t.Errorf("stderr = %q, want %q", errOut, c.wantStderr)
			}
			checkGolden(t, c.name, out)
		})
	}
}
