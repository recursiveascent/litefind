package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// collectScan runs a table scan to completion, gathering the matches
// scanTable streams to its emit callback so assertions can inspect the
// whole sequence.
func collectScan(db *database, ti tableInfo, re *regexp.Regexp, cols []string, maxCount int, wantRow bool) ([]match, error) {
	var ms []match
	err := scanTable(db, ti, re, cols, maxCount, wantRow, func(m match) { ms = append(ms, m) })
	return ms, err
}

func scanFixture(t *testing.T, table, pattern string, o searchOpts) []match {
	t.Helper()
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.close() })
	cat, err := db.catalog()
	if err != nil {
		t.Fatal(err)
	}
	re, err := buildMatcher(pattern, o)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := collectScan(db, tableNamed(t, cat, table), re, nil, o.maxCount, o.row)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestBuildMatcher(t *testing.T) {
	cases := []struct {
		pattern string
		o       searchOpts
		hit     string
		miss    string
	}{
		{"time.ut", searchOpts{}, "timeout", "timeXout"},
		{"a.b", searchOpts{fixed: true}, "xa.bx", "aXb"},
		{"TIMEOUT", searchOpts{ignoreCase: true}, "timeout", "timeut"},
		{"timeout", searchOpts{smartCase: true}, "TIMEOUT", "timeut"},
		{"Timeout", searchOpts{smartCase: true}, "Timeout after", "timeout"},
		{"out", searchOpts{wordRegexp: true}, "way out", "timeout"},
	}
	for _, c := range cases {
		re, err := buildMatcher(c.pattern, c.o)
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(c.hit) {
			t.Errorf("%q %+v should match %q", c.pattern, c.o, c.hit)
		}
		if re.MatchString(c.miss) {
			t.Errorf("%q %+v should not match %q", c.pattern, c.o, c.miss)
		}
	}
}

func TestScanTableRowidAndSpans(t *testing.T) {
	ms := scanFixture(t, "events", "timeout", searchOpts{})
	if len(ms) != 3 {
		t.Fatalf("matches = %d, want 3 (ids 1, 2, 4)", len(ms))
	}
	m := ms[0]
	if m.table != "events" || m.column != "message" || m.rowid != 1 {
		t.Errorf("first match = %+v", m)
	}
	want := "connection timeout after 30s"
	if m.value != want {
		t.Errorf("value = %q, want %q", m.value, want)
	}
	if len(m.spans) != 1 || m.spans[0] != [2]int{11, 18} {
		t.Errorf("spans = %v", m.spans)
	}
	// Determinism: rowid order.
	if ms[0].rowid >= ms[1].rowid || ms[1].rowid >= ms[2].rowid {
		t.Errorf("rows out of rowid order: %v %v %v", ms[0].rowid, ms[1].rowid, ms[2].rowid)
	}
}

func TestScanTableSQLiteTextRendition(t *testing.T) {
	// REAL 1.0 must match as SQLite renders it ('1.0'), not Go's '1'.
	ms := scanFixture(t, "events", `^1\.0$`, searchOpts{})
	if len(ms) != 1 || ms[0].column != "ratio" || ms[0].value != "1.0" {
		t.Fatalf("REAL rendition matches = %+v", ms)
	}
}

func TestScanTablePKIdentity(t *testing.T) {
	// The key='timeout' row matches on both columns: "key" itself is the
	// literal text "timeout", and "value" ("30 timeout") also contains it.
	ms := scanFixture(t, "config", "timeout", searchOpts{})
	if len(ms) != 2 {
		t.Fatalf("matches = %d, want 2", len(ms))
	}
	if ms[0].pk == nil || ms[0].pk[0].col != "key" || ms[0].pk[0].val != "timeout" {
		t.Errorf("pk = %+v", ms[0].pk)
	}
	ms = scanFixture(t, "shadowed", "alpha", searchOpts{})
	if len(ms) == 0 || ms[0].pk == nil {
		t.Errorf("shadowed (all aliases shadowed) must use pk identity: %+v", ms)
	}
}

func TestScanTableSkipsBlobAndNull(t *testing.T) {
	ms := scanFixture(t, "blobs", "timeout", searchOpts{})
	if len(ms) != 1 || ms[0].column != "note" {
		t.Fatalf("blob/null gating: %+v (blob column must not match)", ms)
	}
}

func TestScanTableGeneratedColumns(t *testing.T) {
	ms := scanFixture(t, "gen", "TIMEOUT", searchOpts{})
	if len(ms) != 1 || ms[0].column != "up" {
		t.Fatalf("generated column search: %+v", ms)
	}
}

func TestScanTableMaxCountAndRow(t *testing.T) {
	ms := scanFixture(t, "events", "timeout", searchOpts{maxCount: 1})
	if len(ms) != 1 {
		t.Fatalf("maxCount: %d matches", len(ms))
	}
	ms = scanFixture(t, "events", "nominal", searchOpts{row: true})
	if ms[0].row == nil || ms[0].row["level"] != "info" {
		t.Errorf("row = %+v", ms[0].row)
	}
}

// TestScanTableRowFullRegardlessOfColsFilter: --row attaches the FULL
// row per the spec, even when a cols filter narrows which columns are
// searched for matches. A column outside the cols scope must still show
// up in match.row.
func TestScanTableRowFullRegardlessOfColsFilter(t *testing.T) {
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.close() })
	cat, err := db.catalog()
	if err != nil {
		t.Fatal(err)
	}
	re, err := buildMatcher("nominal", searchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// Scope matching to the "message" column only; "ratio" and "level"
	// are unscoped but must still appear in the attached row.
	ms, err := collectScan(db, tableNamed(t, cat, "events"), re, []string{"message"}, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("matches = %d, want 1", len(ms))
	}
	if ms[0].row == nil {
		t.Fatal("row is nil, want full row")
	}
	if ms[0].row["level"] != "info" {
		t.Errorf("row[level] = %v, want info (unscoped column must still appear)", ms[0].row["level"])
	}
	if _, ok := ms[0].row["ratio"]; !ok {
		t.Errorf("row missing unscoped column ratio: %+v", ms[0].row)
	}
}

// wideTableCols is the column count of the wide batching fixture, chosen
// to span more than one match batch: a rowid table spends one result
// column on identity, leaving (maxResultColumns-1)/2 = 999 data columns
// per query, so 1200 columns split into batches c1..c999 and c1000..c1200.
const wideTableCols = 1200

// wideFixtureMatchCols name the two columns carrying the fixture's only
// non-NULL values: row 1 (the lower rowid) matches only in the LAST batch,
// row 2 only in the FIRST batch — deliberately the reverse of batch order,
// so a batch-major merge (wrongly finishing one batch across every row
// before moving to the next) would emit row 2's match first, while the
// required row-major merge (finishing one row across every batch before
// moving to the next) emits row 1's match first. Every other column is
// left NULL, which the typeof gate excludes from matching.
const (
	wideRow1Col = "c1100" // second batch
	wideRow2Col = "c50"   // first batch
)

// wideFixturePath builds the standalone wide table described above and
// returns its path.
func wideFixturePath(t *testing.T) string {
	t.Helper()
	defs := make([]string, wideTableCols)
	for i := 1; i <= wideTableCols; i++ {
		defs[i-1] = fmt.Sprintf("c%d TEXT", i)
	}
	path := filepath.Join(t.TempDir(), "wide.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ddl := "CREATE TABLE wide (" + strings.Join(defs, ", ") + ")"
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("wide fixture DDL failed: %v", err)
	}
	if _, err := db.Exec("INSERT INTO wide (" + wideRow1Col + ") VALUES ('needle-one')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO wide (" + wideRow2Col + ") VALUES ('needle-two')"); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanTableWideTableBatching(t *testing.T) {
	db, err := openRO(wideFixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.close() })
	cat, err := db.catalog()
	if err != nil {
		t.Fatal(err)
	}
	re, err := buildMatcher("needle", searchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ti := tableNamed(t, cat, "wide")
	// The fixture only tests cross-batch merging if it actually spans more
	// than one batch at the current capacity.
	size, err := matchBatchSize(1) // rowid identity: one expression
	if err != nil {
		t.Fatal(err)
	}
	if len(batchColumns(ti.cols, size)) < 2 {
		t.Fatalf("wide fixture (%d cols) fits in one batch of %d: no cross-batch coverage", len(ti.cols), size)
	}

	ms, err := collectScan(db, ti, re, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("matches = %d, want 2: %+v", len(ms), ms)
	}
	// Row-major merge: row 1's match (in the last batch) must come before
	// row 2's match (in the first batch) — the opposite of what a
	// batch-major merge would produce.
	if ms[0].rowid >= ms[1].rowid {
		t.Errorf("rows out of identity order: %+v %+v", ms[0], ms[1])
	}
	if ms[0].column != wideRow1Col || ms[0].value != "needle-one" {
		t.Errorf("first match = %+v, want column %s value needle-one", ms[0], wideRow1Col)
	}
	if ms[1].column != wideRow2Col || ms[1].value != "needle-two" {
		t.Errorf("second match = %+v, want column %s value needle-two", ms[1], wideRow2Col)
	}
}

// TestMatchBatchSizeErrorsOnOverwideIdentity pins the exact capacity
// boundary: an identity leaving room for one typeof/CAST pair still
// scans (one column per query), and only an identity that can't fit even
// that pair is reported as an error — never a zero-size batch (which
// would loop forever) or a query SQLite rejects with its own error.
func TestMatchBatchSizeErrorsOnOverwideIdentity(t *testing.T) {
	if _, err := matchBatchSize(maxResultColumns); err == nil {
		t.Fatal("matchBatchSize with identCount == the column limit: want error, got nil")
	}
	if _, err := matchBatchSize(maxResultColumns - 1); err == nil {
		t.Fatal("matchBatchSize with room for half a typeof/CAST pair: want error, got nil")
	}
	n, err := matchBatchSize(maxResultColumns - 2)
	if err != nil || n != 1 {
		t.Errorf("matchBatchSize with room for exactly one pair = (%d, %v), want (1, nil)", n, err)
	}
	// Capacity is maximal, not merely non-zero: a plain rowid identity
	// must leave room for every pair the limit allows.
	if n, err := matchBatchSize(1); err != nil || n != (maxResultColumns-1)/2 {
		t.Errorf("matchBatchSize(1) = (%d, %v), want (%d, nil)", n, err, (maxResultColumns-1)/2)
	}
}

// TestScanTableOverwideIdentityError exercises the same guard through
// scanTable itself, via a synthetic tableInfo sitting exactly at the
// narrowest identity that still trips matchBatchSize — one column short
// of leaving room for a typeof/CAST pair, and already wider than SQLite
// would let such a table exist. No query is ever issued — the guard fires
// before any SQL is built — so the fixture table backing the open
// database doesn't need to match ti at all.
func TestScanTableOverwideIdentityError(t *testing.T) {
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.close() })
	re, err := buildMatcher("x", searchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	pkCols := make([]string, maxResultColumns-1)
	for i := range pkCols {
		pkCols[i] = fmt.Sprintf("pk%d", i)
	}
	ti := tableInfo{
		name:   "hypothetical",
		ident:  idPK,
		pkCols: pkCols,
		cols:   []colInfo{{name: "x"}},
	}
	_, err = collectScan(db, ti, re, nil, 0, false)
	if err == nil || !strings.Contains(err.Error(), "too wide") {
		t.Errorf("err = %v, want a 'too wide' primary key error", err)
	}
}

// wideCompositePKPath builds a real WITHOUT ROWID table with a genuinely
// wide (700-column) composite primary key plus one ordinary searchable
// column, and returns its path. Unlike TestScanTableOverwideIdentityError
// (a synthetic tableInfo that never reaches SQL), this exercises
// matchBatchSize's capacity math against SQLite itself: the batch queries
// actually issued must repeat all 700 PK expressions in every batch,
// still leave room within maxResultColumns for the data column, and still
// search it correctly. 700 PK columns + 1 data column stays well under
// SQLite's own default SQLITE_MAX_COLUMN (2000).
func wideCompositePKPath(t *testing.T) string {
	t.Helper()
	const numPK = 700
	pkNames := make([]string, numPK)
	defs := make([]string, numPK, numPK+1)
	for i := 1; i <= numPK; i++ {
		pkNames[i-1] = fmt.Sprintf("pk%d", i)
		defs[i-1] = pkNames[i-1] + " TEXT"
	}
	defs = append(defs, "note TEXT")
	ddl := "CREATE TABLE widepk (" + strings.Join(defs, ", ") +
		", PRIMARY KEY (" + strings.Join(pkNames, ", ") + ")) WITHOUT ROWID"

	path := filepath.Join(t.TempDir(), "widepk.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("wide composite PK fixture DDL failed: %v", err)
	}

	// Every PK column must carry a value (WITHOUT ROWID keys can't be
	// NULL), so each row gets a distinct value per PK column derived from
	// a row label — trivially unique across rows without needing 700
	// hand-written literals.
	insertRow := func(label, note string) {
		cols := append(append([]string{}, pkNames...), "note")
		vals := make([]string, 0, numPK+1)
		for i := 1; i <= numPK; i++ {
			vals = append(vals, fmt.Sprintf("'%s-c%d'", label, i))
		}
		vals = append(vals, "'"+note+"'")
		stmt := "INSERT INTO widepk (" + strings.Join(cols, ", ") +
			") VALUES (" + strings.Join(vals, ", ") + ")"
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("wide composite PK insert failed: %v", err)
		}
	}
	insertRow("row1", "contains needle here")
	insertRow("row2", "no match here")
	return path
}

func TestScanTableWideCompositePK(t *testing.T) {
	db, err := openRO(wideCompositePKPath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.close() })
	cat, err := db.catalog()
	if err != nil {
		t.Fatal(err)
	}
	ti := tableNamed(t, cat, "widepk")
	if ti.ident != idPK {
		t.Fatalf("widepk identity = %v, want idPK", ti.ident)
	}
	if len(ti.pkCols) != 700 {
		t.Fatalf("widepk pkCols = %d, want 700", len(ti.pkCols))
	}

	re, err := buildMatcher("needle", searchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := collectScan(db, ti, re, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("matches = %d, want 1: %+v", len(ms), ms)
	}
	if ms[0].column != "note" || ms[0].value != "contains needle here" {
		t.Errorf("match = %+v, want column note value 'contains needle here'", ms[0])
	}
	if len(ms[0].pk) != 700 {
		t.Fatalf("pk length = %d, want 700", len(ms[0].pk))
	}
	if ms[0].pk[0].col != "pk1" || ms[0].pk[0].val != "row1-c1" {
		t.Errorf("pk[0] = %+v, want col pk1 val row1-c1", ms[0].pk[0])
	}
}

func TestSearchEndToEnd(t *testing.T) {
	out, _, code := runCmd(t, "timeout", fixturePath(t))
	if code != exitMatch {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "events.message:1: connection timeout after 30s") {
		t.Errorf("missing events match:\n%s", out)
	}
	if !strings.Contains(out, "config.value:pk=(timeout)") ||
		!strings.Contains(out, "pk=(timeout): 30") {
		t.Errorf("missing WITHOUT ROWID match:\n%s", out)
	}
	// Deterministic: events (sqlite_master order) precedes config.
	if strings.Index(out, "events.") > strings.Index(out, "config.") {
		t.Errorf("table order not schema order:\n%s", out)
	}
	if strings.Contains(out, "v_errors") {
		t.Errorf("views must not be searched:\n%s", out)
	}
}

func TestSearchScoping(t *testing.T) {
	out, _, _ := runCmd(t, "timeout", fixturePath(t), "-t", "events")
	if strings.Contains(out, "config.") {
		t.Errorf("-t events must exclude config:\n%s", out)
	}
	out, _, _ = runCmd(t, "timeout", fixturePath(t), "-T", "events", "-T", "blobs", "-T", "ipkshadow", "-T", "gen", "-T", "docs")
	if strings.Contains(out, "events.") {
		t.Errorf("-T events must exclude events:\n%s", out)
	}
	out, _, _ = runCmd(t, "timeout", fixturePath(t), "-c", "events.message")
	if strings.Contains(out, "config.") || strings.Contains(out, "events.ratio") {
		t.Errorf("-c scoping leaked:\n%s", out)
	}
}

func TestSearchScansFTS5AndShadowTables(t *testing.T) {
	// Regex search treats fts5 tables as ordinary text tables.
	out, _, code := runCmd(t, "ripgrep", fixturePath(t), "-t", "notes")
	if code != exitMatch || !strings.Contains(out, "notes.body:2:") {
		t.Errorf("fts5 table must be regex-searchable: exit %d\n%s", code, out)
	}
	// Shadow tables scan under --all-tables with no errors AND no
	// silent skips — every shadow table must resolve an identity
	// (the WITHOUT ROWID *_idx/*_config included).
	_, stderr, code := runCmd(t, "zzznever", fixturePath(t), "--all-tables", "-T", "shadnull")
	if code == exitError {
		t.Errorf("--all-tables scan errored: %s", stderr)
	}
	if strings.Contains(stderr, "skipping") {
		t.Errorf("--all-tables scan skipped a table: %s", stderr)
	}
}

func TestSearchNoMatchExit(t *testing.T) {
	_, _, code := runCmd(t, "zzznothing", fixturePath(t))
	if code != exitNoMatch {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestSearchSkipWarning(t *testing.T) {
	_, stderr, _ := runCmd(t, "x", fixturePath(t), "-t", "shadnull")
	if !strings.Contains(stderr, "skipping shadnull") {
		t.Errorf("stderr = %q, want skip warning", stderr)
	}
}

func TestSearchListAndCount(t *testing.T) {
	out, _, _ := runCmd(t, "timeout", fixturePath(t), "-l")
	if !strings.Contains(out, "events\n") || strings.Contains(out, "events.message") {
		t.Errorf("-l output:\n%s", out)
	}
	out, _, _ = runCmd(t, "timeout", fixturePath(t), "--count", "-t", "events")
	if !strings.Contains(out, "events:3") {
		t.Errorf("--count output:\n%s", out)
	}
}

func TestSearchJSONParity(t *testing.T) {
	// Both modes must report identical matches INCLUDING values: each
	// JSONL object is reconstructed into the exact text line it should
	// correspond to, and the two line sets must be equal. PK identity
	// order comes from the catalog's declared PK order, never from Go
	// map iteration.
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.close() }()
	cat, err := db.catalog()
	if err != nil {
		t.Fatal(err)
	}
	pkOrder := map[string][]string{}
	for _, ti := range cat {
		pkOrder[ti.name] = ti.pkCols
	}

	textOut, _, _ := runCmd(t, "timeout", fixturePath(t), "--max-columns", "0")
	jsonOut, _, _ := runCmd(t, "timeout", fixturePath(t), "--json")

	escape := strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`)
	textSet := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(textOut), "\n") {
		textSet[line] = true
	}
	jsonSet := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(jsonOut), "\n") {
		var obj struct {
			Table  string         `json:"table"`
			Column string         `json:"column"`
			Rowid  *int64         `json:"rowid"`
			PK     map[string]any `json:"pk"`
			Value  string         `json:"value"`
		}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("bad JSONL %q: %v", line, err)
		}
		id := ""
		if obj.Rowid != nil {
			id = fmt.Sprintf("%d", *obj.Rowid)
		} else {
			var vals []string
			for _, col := range pkOrder[obj.Table] {
				vals = append(vals, fmt.Sprint(obj.PK[col]))
			}
			id = "pk=(" + strings.Join(vals, ",") + ")"
		}
		jsonSet[fmt.Sprintf("%s.%s:%s: %s", obj.Table, obj.Column, id, escape.Replace(obj.Value))] = true
	}
	if !reflect.DeepEqual(textSet, jsonSet) {
		t.Errorf("parity mismatch:\ntext: %v\njson: %v", textSet, jsonSet)
	}
}

func TestSearchInvalidRegex(t *testing.T) {
	_, stderr, code := runCmd(t, "([", fixturePath(t))
	if code != exitError || !strings.Contains(stderr, "([") {
		t.Errorf("invalid regex: exit %d, stderr %q (must echo pattern)", code, stderr)
	}
}

// TestSearchJSONListAndCount: --json must switch -l and --count to JSONL
// records too — {"table":...} and {"table":...,"count":...} — not the
// text-mode "name\n" / "table:count\n" lines, so a --json pipeline never
// has to parse a mix of JSON and plain text.
func TestSearchJSONListAndCount(t *testing.T) {
	out, _, code := runCmd(t, "timeout", fixturePath(t), "-l", "--json")
	if code != exitMatch {
		t.Fatalf("exit = %d, want 0", code)
	}
	sawEvents := false
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var obj struct {
			Table string `json:"table"`
		}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("bad JSONL %q: %v", line, err)
		}
		if obj.Table == "" {
			t.Errorf("missing table field: %q", line)
		}
		if obj.Table == "events" {
			sawEvents = true
		}
	}
	if !sawEvents {
		t.Errorf("-l --json missing events:\n%s", out)
	}

	out, _, code = runCmd(t, "timeout", fixturePath(t), "--count", "--json", "-t", "events")
	if code != exitMatch {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("--count --json lines = %d, want 1:\n%s", len(lines), out)
	}
	var obj struct {
		Table string `json:"table"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &obj); err != nil {
		t.Fatalf("bad JSONL %q: %v", lines[0], err)
	}
	if obj.Table != "events" || obj.Count != 3 {
		t.Errorf("obj = %+v, want {events 3}", obj)
	}
}

// failWriter is an io.Writer whose every Write fails, simulating a broken
// pipe: any output path that ignores a write error would still exit 0/1
// while silently truncating output.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("simulated write failure")
}

func TestSearchStdoutWriteErrorExitsError(t *testing.T) {
	var stderr strings.Builder
	if code := run([]string{"timeout", fixturePath(t)}, failWriter{}, &stderr); code != exitError {
		t.Errorf("default mode: exit = %d, want %d", code, exitError)
	}

	stderr.Reset()
	if code := run([]string{"timeout", fixturePath(t), "-l"}, failWriter{}, &stderr); code != exitError {
		t.Errorf("-l mode: exit = %d, want %d", code, exitError)
	}

	stderr.Reset()
	if code := run([]string{"timeout", fixturePath(t), "--count"}, failWriter{}, &stderr); code != exitError {
		t.Errorf("--count mode: exit = %d, want %d", code, exitError)
	}

	stderr.Reset()
	// No matches, so the stats summary is the only stdout write attempted
	// — isolating the stats-write-error path from the match/list paths
	// exercised above.
	if code := run([]string{"zzznothing", fixturePath(t), "--stats"}, failWriter{}, &stderr); code != exitError {
		t.Errorf("--stats mode: exit = %d, want %d", code, exitError)
	}
}

// TestScanTableNullIdentityAbortsScan covers the scan-time NULL-identity
// guard, the half of the check that survives a concurrent writer.
// catalog()'s EXISTS verification runs before the scan transaction
// opens, so a NULL-keyed row inserted in between reaches the scan with
// ti.ident still idPK — exactly the tableInfo built here by hand against
// the fixture's shadnull table (every rowid alias shadowed, a nullable
// TEXT primary key holding NULL). The scan must abandon the table with
// errNoStableIdentity rather than emit a row addressed by an empty key.
func TestScanTableNullIdentityAbortsScan(t *testing.T) {
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.close() })
	cat, err := db.catalog()
	if err != nil {
		t.Fatal(err)
	}
	ti := tableNamed(t, cat, "shadnull")
	if ti.ident != idNone {
		t.Fatalf("shadnull identity = %v, want idNone from the catalog-time check", ti.ident)
	}
	// Bypass that check: this is the state a concurrent insert produces.
	ti.ident = idPK
	ti.pkCols = []string{"code"}

	re, err := buildMatcher("x", searchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := collectScan(db, ti, re, nil, 0, false)
	// Emission first: a table without a stable identity must produce no
	// output at all, not output that stops once the damage is noticed.
	if len(ms) != 0 {
		t.Errorf("matches = %+v, want none emitted from a table with a NULL key", ms)
	}
	if !errors.Is(err, errNoStableIdentity) {
		t.Fatalf("err = %v, want errNoStableIdentity", err)
	}
}

// nullKeyLatePath builds a database whose composite-PK table holds a
// NULL key that sorts *after* matching rows: ORDER BY a, b puts NULL b
// first only within its own a-group, so ('a1','x') precedes ('a2',NULL).
// Scanning it row by row would emit the earlier matches before ever
// seeing the NULL — and with -m need never see it at all.
func nullKeyLatePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "latenull.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	stmts := []string{
		`CREATE TABLE comp (a TEXT, b TEXT, txt TEXT, PRIMARY KEY (a, b))`,
		`INSERT INTO comp VALUES ('a1', 'x', 'needle one')`,
		`INSERT INTO comp VALUES ('a1', 'y', 'needle two')`,
		`INSERT INTO comp VALUES ('a2', NULL, 'needle three')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestScanTableNullIdentityEmitsNothingWhenNullSortsLate covers the
// in-transaction identity verification: the per-row guard fires only when
// the scan reaches the offending row, which under a composite key can be
// after matches were already emitted and under -m need never happen at
// all. The scan must therefore verify the whole table's key inside its
// own transaction, before emitting anything, and produce nothing here in
// either case.
func TestScanTableNullIdentityEmitsNothingWhenNullSortsLate(t *testing.T) {
	db, err := openRO(nullKeyLatePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.close() })
	cat, err := db.catalog()
	if err != nil {
		t.Fatal(err)
	}
	ti := tableNamed(t, cat, "comp")
	// comp shadows no rowid alias, so the catalog resolves it to the rowid
	// and never runs the PK check. Force the identity a concurrent
	// NULL-keyed insert would leave behind: verified idPK, now broken.
	ti.ident = idPK
	ti.pkCols = []string{"a", "b"}

	re, err := buildMatcher("needle", searchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, maxCount := range []int{0, 1} {
		ms, err := collectScan(db, ti, re, nil, maxCount, false)
		if len(ms) != 0 {
			t.Errorf("maxCount %d: matches = %+v, want none: two rows match before the NULL-keyed one is reached", maxCount, ms)
		}
		if !errors.Is(err, errNoStableIdentity) {
			t.Errorf("maxCount %d: err = %v, want errNoStableIdentity", maxCount, err)
		}
	}
}

// TestSearchNullKeyLateSkipsWholeTable is the same table end to end: the
// catalog's own check rejects it (identity resolution reaches the
// declared PK only for a table whose rowid aliases are shadowed, so this
// one searches by rowid — but the fixture below shadows them), and the
// warning names it with nothing printed to stdout.
func TestSearchNullKeyLateSkipsWholeTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latenull2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE comp ("rowid" TEXT, "_rowid_" TEXT, "oid" TEXT, a TEXT, b TEXT, txt TEXT, PRIMARY KEY (a, b))`,
		`INSERT INTO comp VALUES ('r', 'r', 'r', 'a1', 'x', 'needle one')`,
		`INSERT INTO comp VALUES ('r', 'r', 'r', 'a2', NULL, 'needle two')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := runCmd(t, "needle", path)
	if code != exitNoMatch {
		t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, exitNoMatch, out, stderr)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing from a table with no stable identity", out)
	}
	if !strings.Contains(stderr, "skipping comp: no stable row identity") {
		t.Errorf("stderr = %q, want the no-stable-row-identity warning", stderr)
	}
}

// TestSearchNullIdentityWarnsAndSkips checks the scan-time guard lands on
// the same skip-with-warning path as the catalog-time one: exit code and
// stderr are indistinguishable between the two.
func TestSearchNullIdentityWarnsAndSkips(t *testing.T) {
	_, stderr, code := runCmd(t, "timeout", fixturePath(t), "-t", "shadnull")
	if code != exitNoMatch {
		t.Fatalf("exit = %d, want %d", code, exitNoMatch)
	}
	if !strings.Contains(stderr, "skipping shadnull: no stable row identity") {
		t.Errorf("stderr = %q, want the no-stable-row-identity warning", stderr)
	}
}

// streamFixturePath builds a database of `tables` tables each matching
// `rows` rows — far more than one table stream buffers
// (tableMatchBuffer) when rows exceeds it, so scans must block on
// emission and resume as the emitter drains them.
func streamFixturePath(t *testing.T, tables, rows int) string {
	t.Helper()
	counts := make([]int, tables)
	for i := range counts {
		counts[i] = rows
	}
	return streamFixtureRows(t, counts)
}

// streamFixtureRows builds one table per entry of counts — t01, t02, ...
// in catalog order — with that many matching rows, so a fixture can mix
// tables big enough to block their scan with tables that finish at once.
func streamFixtureRows(t *testing.T, counts []int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i, rows := range counts {
		name := fmt.Sprintf("t%02d", i+1)
		if _, err := db.Exec("CREATE TABLE " + name + " (id INTEGER PRIMARY KEY, txt TEXT)"); err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for r := range rows {
			if _, err := tx.Exec("INSERT INTO "+name+" (txt) VALUES (?)", fmt.Sprintf("needle row %d", r)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestSearchStreamingOrderAndCompleteness pins determinism across the
// bounded streaming path: every table matches several buffers' worth of
// rows, so workers genuinely block mid-scan, yet output must still be
// catalog-ordered, rowid-ordered within a table, and complete. Run under
// -race this also covers the worker/emitter handoff.
func TestSearchStreamingOrderAndCompleteness(t *testing.T) {
	const (
		tables = 4
		rows   = 3 * tableMatchBuffer
	)
	// Fewer workers than tables is the configuration that would deadlock
	// if the pool handed tables out in any order but catalog order: a
	// worker blocked on a full stream the emitter cannot reach yet would
	// hold a slot the emitter's own table needs. Force it.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	path := streamFixturePath(t, tables, rows)

	out, stderr, code := runCmd(t, "needle", path)
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitMatch, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != tables*rows {
		t.Fatalf("lines = %d, want %d", len(lines), tables*rows)
	}
	for i, line := range lines {
		wantTable := fmt.Sprintf("t%02d", i/rows+1)
		wantRowid := i%rows + 1
		wantPrefix := fmt.Sprintf("%s.txt:%d: needle row %d", wantTable, wantRowid, wantRowid-1)
		if line != wantPrefix {
			t.Fatalf("line %d = %q, want %q", i, line, wantPrefix)
		}
	}

	// The aggregate modes read the same stream: counts must agree with it.
	out, _, code = runCmd(t, "needle", path, "--count")
	if code != exitMatch {
		t.Fatalf("--count exit = %d", code)
	}
	var wantCounts strings.Builder
	for i := 1; i <= tables; i++ {
		fmt.Fprintf(&wantCounts, "t%02d:%d\n", i, rows)
	}
	if out != wantCounts.String() {
		t.Errorf("--count output =\n%s\nwant\n%s", out, wantCounts.String())
	}

	out, _, code = runCmd(t, "needle", path, "-l")
	if code != exitMatch {
		t.Fatalf("-l exit = %d", code)
	}
	if want := "t01\nt02\nt03\nt04\n"; out != want {
		t.Errorf("-l output = %q, want %q", out, want)
	}

	out, _, code = runCmd(t, "needle", path, "--stats")
	if code != exitMatch {
		t.Fatalf("--stats exit = %d", code)
	}
	if want := fmt.Sprintf("%d matches in %d tables (%d tables scanned)", tables*rows, tables, tables); !strings.Contains(out, want) {
		t.Errorf("--stats summary missing %q:\n%s", want, out)
	}
}

// TestSearchStreamingManyTablesFewSlots is the shape the stream-slot
// bound exists for: far more tables than slots, with the tables the
// emitter reaches first big enough to block on their buffers and every
// later table finishing instantly. Without emitter-released slots the
// small tables all run to completion behind the emitter's back, piling up
// finished-but-undrained streams; with them, a table can only start once
// the emitter has consumed an earlier one — which is also the arrangement
// most likely to deadlock if slots were acquired out of catalog order.
// Output must still be complete and in catalog order.
func TestSearchStreamingManyTablesFewSlots(t *testing.T) {
	const (
		bigTables   = 2
		bigRows     = 3 * tableMatchBuffer
		smallTables = 12
	)
	counts := make([]int, 0, bigTables+smallTables)
	for range bigTables {
		counts = append(counts, bigRows)
	}
	for range smallTables {
		counts = append(counts, 1)
	}
	// Two slots against fourteen tables.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	path := streamFixtureRows(t, counts)

	out, stderr, code := runCmdBefore(t, time.Minute, "needle", path, "--count")
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitMatch, stderr)
	}
	var wantCounts strings.Builder
	for i, n := range counts {
		fmt.Fprintf(&wantCounts, "t%02d:%d\n", i+1, n)
	}
	if out != wantCounts.String() {
		t.Errorf("--count output =\n%s\nwant\n%s", out, wantCounts.String())
	}

	// -l names every table in catalog order: the emitter is handed each
	// table's stream by one goroutine that creates them in that order, and
	// twelve instant tables behind two blocking ones is the arrangement
	// that would expose completion order leaking through.
	out, stderr, code = runCmdBefore(t, time.Minute, "needle", path, "-l")
	if code != exitMatch {
		t.Fatalf("-l exit = %d, want %d (stderr: %s)", code, exitMatch, stderr)
	}
	var wantTables strings.Builder
	for i := range counts {
		fmt.Fprintf(&wantTables, "t%02d\n", i+1)
	}
	if out != wantTables.String() {
		t.Errorf("-l output =\n%s\nwant\n%s", out, wantTables.String())
	}
}

// runCmdBefore runs one command, failing the test if it has not finished
// within limit rather than hanging the whole suite — the failure mode a
// mistake in the scan-slot handoff produces.
func runCmdBefore(t *testing.T, limit time.Duration, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	type result struct {
		out, errs string
		code      int
	}
	done := make(chan result, 1)
	go func() {
		out, errs, code := runCmd(t, argv...)
		done <- result{out, errs, code}
	}()
	select {
	case r := <-done:
		return r.out, r.errs, r.code
	case <-time.After(limit):
		t.Fatalf("%v did not finish in %s: scan slots deadlocked", argv, limit)
		return "", "", 0
	}
}

// recvBefore returns the next value from ch, failing the test if none
// arrives within limit. Used where a hang, not a wrong value, is the
// failure being guarded against.
func recvBefore[T any](t *testing.T, limit time.Duration, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(limit):
		var zero T
		t.Fatalf("%s did not arrive within %s", what, limit)
		return zero
	}
}

// TestStartScansOverlapsScansUpToPoolSize pins the concurrency the pool
// exists to provide, which output-only tests cannot see: a scan must
// start as soon as a slot is free, not when the reader gets around to
// receiving its stream. The reader here deliberately receives nothing
// until two scans are running at once — so an implementation that hands a
// stream over before starting its scan (and thus makes every scan wait on
// the reader) hangs on the second start rather than merely running slow.
// The pool's ceiling is checked in the same breath: with two slots the
// third table must not start until a stream has been fully consumed.
func TestStartScansOverlapsScansUpToPoolSize(t *testing.T) {
	const poolSize = 2
	scoped := []tableInfo{{name: "t1"}, {name: "t2"}, {name: "t3"}, {name: "t4"}}

	var active, maxActive atomic.Int64
	started := make(chan string, len(scoped))
	// Both pool-filling scans park here until the test has seen them both
	// start, so their overlap is a fact rather than a scheduling accident:
	// neither can return before the other has begun.
	overlap := make(chan struct{})
	scan := func(ti tableInfo, emit func(match)) tableResult {
		n := active.Add(1)
		for {
			was := maxActive.Load()
			if n <= was || maxActive.CompareAndSwap(was, n) {
				break
			}
		}
		started <- ti.name
		if ti.name == "t1" || ti.name == "t2" {
			<-overlap
		}
		emit(match{table: ti.name})
		active.Add(-1)
		return tableResult{scanned: true}
	}

	slots := make(chan struct{}, poolSize)
	streams := startScans(scoped, slots, scan)

	// Which of the two starts first is up to the scheduler; that both are
	// running before anything is drained is the point.
	first := recvBefore(t, 10*time.Second, "first scan start", started)
	second := recvBefore(t, 10*time.Second, "second scan start", started)
	if got := []string{first, second}; !reflect.DeepEqual(got, []string{"t1", "t2"}) &&
		!reflect.DeepEqual(got, []string{"t2", "t1"}) {
		t.Errorf("scans started = %v, want t1 and t2", got)
	}
	// Slots are exhausted: no further scan may begin until the reader
	// consumes one. Nothing can make this arrive, so the wait is a
	// ceiling check, not a race.
	select {
	case name := <-started:
		t.Errorf("%s started with both slots held", name)
	case <-time.After(100 * time.Millisecond):
	}
	close(overlap)

	var order []string
	for st := range streams {
		order = append(order, st.name)
		for range st.matches {
		}
		if res := recvBefore(t, 10*time.Second, st.name+" result", st.done); !res.scanned {
			t.Errorf("%s: scanned = false", st.name)
		}
		<-slots
	}
	if want := []string{"t1", "t2", "t3", "t4"}; !reflect.DeepEqual(order, want) {
		t.Errorf("stream order = %v, want %v", order, want)
	}
	if got := maxActive.Load(); got < 2 {
		t.Errorf("peak concurrent scans = %d, want at least %d", got, poolSize)
	}
}

// TestSearchMaxCountUnderStreaming: -m stops a table's scan at N matches
// even when N is smaller than the stream buffer, and the per-table cap
// applies independently to every table.
func TestSearchMaxCountUnderStreaming(t *testing.T) {
	path := streamFixturePath(t, 2, tableMatchBuffer+10)
	out, _, code := runCmd(t, "needle", path, "-m", "3", "--count")
	if code != exitMatch {
		t.Fatalf("exit = %d", code)
	}
	if want := "t01:3\nt02:3\n"; out != want {
		t.Errorf("--count with -m 3 = %q, want %q", out, want)
	}
}
