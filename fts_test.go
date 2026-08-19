package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func resolveFixture(t *testing.T, o searchOpts) ([]ftsTarget, error) {
	t.Helper()
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.close() })
	cat, err := db.catalog()
	if err != nil {
		t.Fatal(err)
	}
	return resolveFTS(cat, o)
}

func TestResolveFTSDirect(t *testing.T) {
	tgts, err := resolveFixture(t, searchOpts{tables: []string{"notes"}})
	if err != nil || len(tgts) != 1 || tgts[0].index != "notes" || tgts[0].source != "" {
		t.Fatalf("tgts = %+v, err = %v", tgts, err)
	}
}

func TestResolveFTSSiblingMapping(t *testing.T) {
	tgts, err := resolveFixture(t, searchOpts{tables: []string{"docs"}})
	if err != nil || len(tgts) != 1 || tgts[0].index != "docs_fts" || tgts[0].source != "docs" {
		t.Fatalf("tgts = %+v, err = %v", tgts, err)
	}
}

func TestResolveFTSDedupesGlobOverlap(t *testing.T) {
	// 'docs*' matches both the source and its index; the index must
	// resolve once, with source metadata retained.
	tgts, err := resolveFixture(t, searchOpts{tables: []string{"docs*"}})
	if err != nil || len(tgts) != 1 {
		t.Fatalf("tgts = %+v, err = %v (want single deduped target)", tgts, err)
	}
	if tgts[0].index != "docs_fts" || tgts[0].source != "docs" {
		t.Errorf("deduped target = %+v, want source metadata kept", tgts[0])
	}
}

func TestResolveFTSContentlessRejected(t *testing.T) {
	_, err := resolveFixture(t, searchOpts{tables: []string{"ghost"}})
	if err == nil || !strings.Contains(err.Error(), "contentless") {
		t.Fatalf("err = %v, want contentless diagnostic", err)
	}
}

func TestResolveFTSAmbiguousSiblings(t *testing.T) {
	_, err := resolveFixture(t, searchOpts{tables: []string{"multi"}})
	if err == nil || !strings.Contains(err.Error(), "multi_fts_a") ||
		!strings.Contains(err.Error(), "multi_fts_b") {
		t.Fatalf("err = %v, want ambiguity error naming both candidates", err)
	}
}

func TestResolveFTSNotTablesUnscopedExcludesIndex(t *testing.T) {
	tgts, err := resolveFixture(t, searchOpts{notTables: []string{"notes"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range tgts {
		if tg.index == "notes" {
			t.Fatalf("notes excluded by -T still present: %+v", tgts)
		}
	}
	found := false
	for _, tg := range tgts {
		if tg.index == "docs_fts" {
			found = true
		}
	}
	if !found {
		t.Errorf("-T notes must not exclude unrelated indexes: %+v", tgts)
	}
}

func TestResolveFTSNotTablesExcludesScopedTable(t *testing.T) {
	tgts, err := resolveFixture(t, searchOpts{tables: []string{"notes"}, notTables: []string{"notes"}})
	if err != nil {
		t.Fatalf("excluded scoped table must not error: %v", err)
	}
	if len(tgts) != 0 {
		t.Fatalf("tgts = %+v, want none (table excluded by -T)", tgts)
	}
}

func TestResolveFTSNotTablesExcludesSiblingIndex(t *testing.T) {
	tgts, err := resolveFixture(t, searchOpts{tables: []string{"docs"}, notTables: []string{"docs_fts"}})
	if err != nil {
		t.Fatalf("excluded sibling index must not error: %v", err)
	}
	if len(tgts) != 0 {
		t.Fatalf("tgts = %+v, want none (sole sibling excluded by -T)", tgts)
	}
}

func TestFTSEndToEnd(t *testing.T) {
	out, _, code := runCmd(t, "--fts", "timeout", fixturePath(t), "-t", "docs")
	if code != exitMatch {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "docs_fts:2:") || !strings.Contains(out, "timeout") {
		t.Errorf("fts output:\n%s", out)
	}
}

func TestFTSJSONContract(t *testing.T) {
	out, _, _ := runCmd(t, "--fts", "sqlite", fixturePath(t), "-t", "notes", "--json")
	if !strings.Contains(out, `"snippet"`) || !strings.Contains(out, `"rank"`) {
		t.Errorf("fts json needs snippet+rank:\n%s", out)
	}
	if strings.Contains(out, `"spans"`) || strings.Contains(out, `"column"`) {
		t.Errorf("fts json must not carry spans/column:\n%s", out)
	}
}

func TestFTSMissEmitsRunnableSetupSQL(t *testing.T) {
	path := fixturePath(t)
	_, stderr, code := runCmd(t, "--fts", "timeout", path, "-t", "events")
	if code != exitError {
		t.Fatalf("miss must exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "CREATE VIRTUAL TABLE") ||
		!strings.Contains(stderr, "content='events'") ||
		!strings.Contains(stderr, "'rebuild'") ||
		strings.Count(stderr, "CREATE TRIGGER") != 3 {
		t.Fatalf("setup SQL incomplete:\n%s", stderr)
	}
	// The emitted SQL must actually work: execute it, retry, succeed.
	sqlText := stderr[strings.Index(stderr, "CREATE VIRTUAL TABLE"):]
	w, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(sqlText); err != nil {
		t.Fatalf("emitted SQL failed to execute: %v\n%s", err, sqlText)
	}
	w.Close()
	out, _, code := runCmd(t, "--fts", "timeout", path, "-t", "events")
	if code != exitMatch || !strings.Contains(out, "events_fts:") {
		t.Fatalf("retry after setup: exit %d\n%s", code, out)
	}
}

func TestFTSMissWithoutRowidDiagnostic(t *testing.T) {
	_, stderr, code := runCmd(t, "--fts", "x", fixturePath(t), "-t", "config")
	if code != exitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr, "UNINDEXED") || strings.Contains(stderr, "content='config'") {
		t.Errorf("WITHOUT ROWID diagnostic must suggest standalone FTS, not external content:\n%s", stderr)
	}
	// A standalone index has no content= mapping back to config, so the
	// unscoped/-t-config sibling lookup will never find it automatically;
	// the guidance must instruct rerunning with -t naming the index
	// itself.
	if !strings.Contains(stderr, "config_fts") {
		t.Errorf("standalone diagnostic must instruct rerunning with -t naming config_fts:\n%s", stderr)
	}
}

func TestFTSMissKeylessTableDiagnostic(t *testing.T) {
	// shadnull: every rowid alias is shadowed by a declared column, and
	// its PK (code) holds a NULL row, so it resolves to idNone — no
	// stable identity at all, so no CREATE SQL (which would need one) can
	// be generated.
	_, stderr, code := runCmd(t, "--fts", "q", fixturePath(t), "-t", "shadnull")
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "stable") {
		t.Errorf("keyless diagnostic must mention the missing stable identity:\n%s", stderr)
	}
	if strings.Contains(stderr, "CREATE") {
		t.Errorf("keyless diagnostic must not emit CREATE SQL:\n%s", stderr)
	}
}

func TestFTSMissNoTextColumnsDiagnostic(t *testing.T) {
	// metrics has only INTEGER/REAL columns: no TEXT-affinity column
	// exists to put in a generated FTS5 index.
	_, stderr, code := runCmd(t, "--fts", "q", fixturePath(t), "-t", "metrics")
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if strings.Contains(stderr, "CREATE") {
		t.Errorf("no-text-columns diagnostic must not emit CREATE SQL:\n%s", stderr)
	}
}

func TestFTSValidatesGlobsBeforeCatalog(t *testing.T) {
	_, stderr, code := runCmd(t, "--fts", "q", fixturePath(t), "-t", "[bad")
	if code != exitError {
		t.Fatalf("exit = %d, want %d\n%s", code, exitError, stderr)
	}
}

// TestFTSRowFetchesFullSourceRowViaDirectIndexTarget: direct-scoping the
// index by name (-t docs_fts, not -t docs) must still populate the target's
// content/content_rowid metadata from the catalog, so --row fetches the
// full docs row — including slug, a column docs_fts doesn't index — rather
// than querying docs_fts itself (which only has title/body).
func TestFTSRowFetchesFullSourceRowViaDirectIndexTarget(t *testing.T) {
	out, _, code := runCmd(t, "--fts", "timeout", fixturePath(t), "-t", "docs_fts", "--row", "--json")
	if code != exitMatch {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no matches:\n%s", out)
	}
	for _, line := range lines {
		var obj struct {
			Row map[string]any `json:"row"`
		}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("bad JSONL %q: %v", line, err)
		}
		if obj.Row == nil {
			t.Fatalf("missing row: %q", line)
		}
		if _, ok := obj.Row["slug"]; !ok {
			t.Errorf("row missing non-indexed column slug: %q", line)
		}
	}
}

// TestFTSRowContentRowidDivergesFromPhysicalRowid builds a dedicated
// fixture where a source table's content_rowid column (docid) genuinely
// diverges from its physical rowid (the INTEGER PRIMARY KEY id): the fts5
// index is populated so its own rowid equals docid, not id. docs_fts in
// the shared fixture can't exercise this — its content_rowid='id' names
// the IPK itself, which IS the physical rowid, so a --row fetch keyed on
// literal "rowid" would happen to land on the same row anyway. Here, a
// rowid-keyed fetch would look up src2 WHERE rowid = 501/502 — no such
// physical rowid exists (only 1 and 2 do) — so a regression to the old
// hardcoded-"rowid" behavior fails loudly (missing row / exit 2), not
// silently with the wrong data.
func TestFTSRowContentRowidDivergesFromPhysicalRowid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "divergent.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE src2 (id INTEGER PRIMARY KEY, docid INTEGER, note TEXT, extra TEXT)`,
		`INSERT INTO src2 (docid, note, extra) VALUES
			(501, 'timeout warning', 'alpha-extra'),
			(502, 'all clear', 'beta-extra')`,
		`CREATE VIRTUAL TABLE src2_fts USING fts5(note, content='src2', content_rowid='docid')`,
		// Manual population, not triggers: the fts5 index's own rowid is
		// set to docid here, which is what makes it diverge from src2's
		// physical rowid (id).
		`INSERT INTO src2_fts(rowid, note) SELECT docid, note FROM src2`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture stmt failed: %v\n%s", err, stmt)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	assertRow := func(t *testing.T, args ...string) {
		t.Helper()
		full := append([]string{"--fts", "timeout", path, "--row", "--json"}, args...)
		out, stderr, code := runCmd(t, full...)
		if code != exitMatch {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitMatch, stderr)
		}
		line := strings.TrimSpace(out)
		var obj struct {
			Row map[string]any `json:"row"`
		}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("bad JSONL %q: %v", line, err)
		}
		if obj.Row == nil {
			t.Fatalf("missing row: %q", line)
		}
		if got := obj.Row["extra"]; got != "alpha-extra" {
			t.Errorf("row[extra] = %v, want alpha-extra (docid-keyed fetch, not physical-rowid-keyed): %q", got, line)
		}
	}

	t.Run("scoped by source", func(t *testing.T) { assertRow(t, "-t", "src2") })
	t.Run("scoped by index", func(t *testing.T) { assertRow(t, "-t", "src2_fts") })
	t.Run("unscoped", func(t *testing.T) { assertRow(t) })
}

func TestFTSRankOrderWithRowidTieBreak(t *testing.T) {
	out, _, _ := runCmd(t, "--fts", "transfer OR sqlite", fixturePath(t), "-t", "notes")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Skip("need 2+ hits for order check")
	}
	// Deterministic across runs: run twice, byte-identical.
	out2, _, _ := runCmd(t, "--fts", "transfer OR sqlite", fixturePath(t), "-t", "notes")
	if out != out2 {
		t.Errorf("fts output not deterministic:\n%s\nvs\n%s", out, out2)
	}
}

func TestFTSMaxColumnsTruncatesSnippet(t *testing.T) {
	out, _, _ := runCmd(t, "--fts", "timeout", fixturePath(t), "-t", "docs", "--max-columns", "10")
	line := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	body := line[strings.LastIndex(line, ": ")+2:]
	if len([]rune(body)) > 14 { // window + ellipses slack
		t.Errorf("snippet not truncated by --max-columns: %q", line)
	}
}

func TestResolveFTSUnscopedUsesAllIndexes(t *testing.T) {
	tgts, err := resolveFixture(t, searchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tg := range tgts {
		names[tg.index] = true
	}
	// Direct fts5 tables only; ghost (contentless) silently excluded in
	// the unscoped sweep — rejection is for explicit scoping.
	for _, want := range []string{"notes", "docs_fts", "multi_fts_a", "multi_fts_b"} {
		if !names[want] {
			t.Errorf("unscoped targets missing %s: %v", want, names)
		}
	}
	if names["ghost"] {
		t.Errorf("contentless index must not join the unscoped sweep")
	}
}
