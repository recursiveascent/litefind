package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTablesText(t *testing.T) {
	out, _, code := runCmd(t, "tables", fixturePath(t))
	if code != exitMatch {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "events\ttable\t4\t4") {
		t.Errorf("missing events line:\n%s", out)
	}
	if !strings.Contains(out, "v_errors\tview\t-\t1") {
		t.Errorf("views must show '-' count (never executed):\n%s", out)
	}
	if strings.Contains(out, "notes_data") {
		t.Errorf("shadow tables hidden by default:\n%s", out)
	}
}

func TestTablesAllAndNoCounts(t *testing.T) {
	out, _, _ := runCmd(t, "tables", fixturePath(t), "--all-tables")
	if !strings.Contains(out, "notes_data") {
		t.Errorf("--all-tables must include shadow tables:\n%s", out)
	}
	out, _, _ = runCmd(t, "tables", fixturePath(t), "--no-counts")
	if strings.Contains(out, "\t4\t") {
		t.Errorf("--no-counts must omit row counts:\n%s", out)
	}
}

func TestTablesJSON(t *testing.T) {
	out, _, _ := runCmd(t, "tables", fixturePath(t), "--json")
	// Decode and verify every key is present and lowercase.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("not JSONL: %v: %q", err, line)
		}
		for _, key := range []string{"name", "kind", "rows", "cols"} {
			if _, ok := obj[key]; !ok {
				t.Fatalf("missing key %q in %q", key, line)
			}
		}
		if obj["name"] == "events" && obj["rows"] != float64(4) {
			t.Errorf("events rows = %v, want 4", obj["rows"])
		}
	}
}

func TestTablesErrorRouting(t *testing.T) {
	// Test that openRO errors route to stderr and don't corrupt stdout.
	stdout, stderr, code := runCmd(t, "tables", "/nonexistent/db/path.db")
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if stdout != "" {
		t.Errorf("stdout must be empty on error, got: %q", stdout)
	}
	if !strings.Contains(stderr, "no such file") {
		t.Errorf("stderr must contain error message, got: %q", stderr)
	}
}

func TestTablesCmdTablesErrorRouting(t *testing.T) {
	// Test cmdTables directly with a closed database to verify catalog
	// errors route to stderr, keeping stdout pure.
	var stdout, stderr strings.Builder
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	// Close the database to cause catalog() to fail
	db.close()

	code := cmdTables(db, searchOpts{}, &stdout, &stderr)
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if stdout.String() != "" {
		t.Errorf("stdout must be empty on catalog error, got: %q", stdout.String())
	}
	if stderr.String() == "" {
		t.Errorf("stderr must contain error message")
	}
}

func TestSchemaText(t *testing.T) {
	out, _, code := runCmd(t, "schema", fixturePath(t), "events")
	if code != exitMatch {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "CREATE TABLE events") {
		t.Errorf("missing DDL:\n%s", out)
	}
	if !strings.Contains(out, "id") || !strings.Contains(out, "INTEGER") {
		t.Errorf("missing column detail:\n%s", out)
	}
}

func TestSchemaGlobNoMatch(t *testing.T) {
	_, _, code := runCmd(t, "schema", fixturePath(t), "zzz*")
	if code != exitNoMatch {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestSchemaJSON(t *testing.T) {
	out, _, _ := runCmd(t, "schema", fixturePath(t), "config", "--json")
	if !strings.Contains(out, `"name":"config"`) || !strings.Contains(out, `"pk":1`) {
		t.Errorf("json schema output:\n%s", out)
	}
}

func TestSchemaTextDefaults(t *testing.T) {
	out, _, code := runCmd(t, "schema", fixturePath(t), "defaults_t")
	if code != exitMatch {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "DEFAULT 'info'") || !strings.Contains(out, "DEFAULT 0") {
		t.Errorf("missing DEFAULT values:\n%s", out)
	}
}

func TestSchemaTextExpressionIndex(t *testing.T) {
	out, _, code := runCmd(t, "schema", fixturePath(t), "events")
	if code != exitMatch {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "expr_idx") || !strings.Contains(out, "<expr>") {
		t.Errorf("missing expression index:\n%s", out)
	}
	// Verify the actual expression appears in the output
	if !strings.Contains(out, "lower(message)") {
		t.Errorf("missing expression DDL with lower(message):\n%s", out)
	}
}

func TestSchemaForeignKeyImplicitPK(t *testing.T) {
	out, _, code := runCmd(t, "schema", fixturePath(t), "fk_source", "--json")
	if code != exitMatch {
		t.Fatalf("exit = %d", code)
	}
	// JSON should show null for implicit PK reference
	if !strings.Contains(out, `"to":null`) {
		t.Errorf("missing null FK column:\n%s", out)
	}
}

func TestSchemaMalformedGlob(t *testing.T) {
	_, stderr, code := runCmd(t, "schema", fixturePath(t), "[unclosed")
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "invalid glob pattern") {
		t.Errorf("stderr should mention invalid pattern, got: %s", stderr)
	}
}

// TestSchemaPartialIndexDDL: PRAGMA index_info reports indexed column
// names and nothing else, so a partial index's predicate, a DESC key,
// and a COLLATE clause exist only in the index's own statement. Schema
// output must carry that statement for every user-created index, not
// just the expression indexes that would otherwise print "<expr>".
func TestSchemaPartialIndexDDL(t *testing.T) {
	out, _, code := runCmd(t, "schema", fixturePath(t), "metrics")
	if code != exitMatch {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "metrics_hot") {
		t.Fatalf("missing index name:\n%s", out)
	}
	for _, want := range []string{"WHERE count > 3", "value DESC", "COLLATE NOCASE"} {
		if !strings.Contains(out, want) {
			t.Errorf("index DDL missing %q (index_info cannot report it):\n%s", want, out)
		}
	}

	out, _, code = runCmd(t, "schema", fixturePath(t), "metrics", "--json")
	if code != exitMatch {
		t.Fatalf("json exit = %d", code)
	}
	var obj schemaObject
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		t.Fatalf("not JSON: %v: %s", err, out)
	}
	if len(obj.Indexes) != 1 {
		t.Fatalf("indexes = %+v, want 1", obj.Indexes)
	}
	if !strings.Contains(obj.Indexes[0].DDL, "WHERE count > 3") {
		t.Errorf("json ddl = %q, want the partial-index predicate", obj.Indexes[0].DDL)
	}
}

// TestBrokenViewDoesNotBlockOtherTables: a view whose base table was
// dropped fails column introspection. That must cost the view its column
// list and nothing else — the catalog still builds, unrelated tables are
// still searchable, `tables` still lists the view, and `schema` reports
// the failure against that view alone.
func TestBrokenViewDoesNotBlockOtherTables(t *testing.T) {
	path := brokenViewPath(t)

	db, err := openRO(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.close() })
	cat, err := db.catalog()
	if err != nil {
		t.Fatalf("catalog failed on a dangling view: %v", err)
	}
	view := tableNamed(t, cat, "v_gone")
	if view.xinfoErr == nil {
		t.Error("v_gone.xinfoErr = nil, want the column-introspection failure recorded")
	}
	if len(view.cols) != 0 {
		t.Errorf("v_gone cols = %+v, want none", view.cols)
	}
	if keep := tableNamed(t, cat, "keep"); len(keep.cols) != 2 {
		t.Errorf("keep cols = %+v, want 2 (unaffected by the broken view)", keep.cols)
	}

	out, stderr, code := runCmd(t, "tables", path)
	if code != exitMatch {
		t.Fatalf("tables exit = %d, want %d (stderr: %s)", code, exitMatch, stderr)
	}
	if !strings.Contains(out, "keep\ttable\t1\t2") || !strings.Contains(out, "v_gone\tview\t-\t0") {
		t.Errorf("tables output:\n%s", out)
	}

	out, stderr, code = runCmd(t, "schema", path)
	if code != exitMatch {
		t.Fatalf("schema exit = %d, want %d (stderr: %s)", code, exitMatch, stderr)
	}
	if !strings.Contains(out, "CREATE TABLE keep") {
		t.Errorf("schema dropped the healthy table:\n%s", out)
	}
	if !strings.Contains(out, "CREATE VIEW v_gone") {
		t.Errorf("schema dropped the broken view's own DDL:\n%s", out)
	}
	if !strings.Contains(stderr, "v_gone") || !strings.Contains(stderr, "no such table: main.gone") {
		t.Errorf("stderr must name the view and its cause, got: %q", stderr)
	}

	// The healthy table is still searchable.
	out, _, code = runCmd(t, "kept", path)
	if code != exitMatch {
		t.Fatalf("search exit = %d, want %d", code, exitMatch)
	}
	if !strings.Contains(out, "keep.note:1: kept intact") {
		t.Errorf("search output:\n%s", out)
	}
}

// TestIntrospectStdoutWriteErrorExitsError: truncated output must not
// look like success. Every `tables`/`schema` write is checked, the same
// contract cmdSearch holds.
func TestIntrospectStdoutWriteErrorExitsError(t *testing.T) {
	path := fixturePath(t)
	for _, argv := range [][]string{
		{"tables", path},
		{"tables", path, "--json"},
		{"schema", path},
		{"schema", path, "--json"},
		{"schema", path, "events"},
	} {
		var stderr strings.Builder
		if code := run(argv, failWriter{}, &stderr); code != exitError {
			t.Errorf("run(%v) = %d, want %d", argv, code, exitError)
		}
		if !strings.Contains(stderr.String(), "simulated write failure") {
			t.Errorf("run(%v) stderr = %q, want the write failure reported", argv, stderr.String())
		}
	}
}
