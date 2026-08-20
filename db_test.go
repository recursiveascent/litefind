package main

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenROOpensFixture(t *testing.T) {
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.close() }()
}

func TestOpenROMissingFile(t *testing.T) {
	_, err := openRO(filepath.Join(t.TempDir(), "nope.db"), false)
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %v, want missing-file message", err)
	}
}

// TestOpenROUnreadableFileReportsRealError: only a genuine
// does-not-exist failure may be reported as "no such file". A stat that
// fails for any other reason — here an unreadable parent directory —
// must report what actually went wrong, or the user goes looking for a
// missing file that is sitting right there.
func TestOpenROUnreadableFileReportsRealError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hidden.db")
	if err := os.WriteFile(p, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.Stat(p); err == nil {
		t.Skip("stat succeeded through a mode-0 directory (running as root?)")
	}

	_, err := openRO(p, false)
	if err == nil {
		t.Fatal("openRO succeeded on an unreadable path")
	}
	if strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %v, want the real condition, not a missing-file claim", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want the underlying error preserved (fs.ErrPermission)", err)
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("err = %v, want the path named", err)
	}
}

func TestOpenRONotADatabase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(p, []byte("just text, definitely long enough to not be sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := openRO(p, false)
	if err == nil || !strings.Contains(err.Error(), "not a SQLite database") {
		t.Errorf("err = %v, want not-a-database message", err)
	}
}

func TestOpenRORefusesWrites(t *testing.T) {
	db, err := openRO(fixturePath(t), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.close() }()
	if _, err := db.sql.Exec(`INSERT INTO events (message) VALUES ('nope')`); err == nil {
		t.Fatal("write on mode=ro connection succeeded; must fail")
	}
}

func TestOpenROLiveWALReadOnlyDir(t *testing.T) {
	// Build a WAL-mode db, leave a live -wal, then remove the -shm and
	// make the directory unwritable so SQLite cannot recreate it.
	dir := t.TempDir()
	p := filepath.Join(dir, "wal.db")
	w, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE x(a); INSERT INTO x VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil { // modernc checkpoints on close; re-create a live -wal artificially
		t.Fatal(err)
	}
	if err := os.WriteFile(p+"-wal", []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(p + "-shm") // may not exist; absence is fine
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, err = openRO(p, false)
	if err == nil {
		t.Skip("platform allowed read-only WAL open; condition not reproducible here")
	}
	if !strings.Contains(err.Error(), "WAL") {
		t.Errorf("err = %v, want WAL condition explanation", err)
	}
}

func catalogOf(t *testing.T) []tableInfo {
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
	return cat
}

func tableNamed(t *testing.T, cat []tableInfo, name string) tableInfo {
	t.Helper()
	for _, ti := range cat {
		if ti.name == name {
			return ti
		}
	}
	t.Fatalf("table %q not in catalog", name)
	return tableInfo{}
}

func TestCatalogKindsAndShadows(t *testing.T) {
	cat := catalogOf(t)
	if ti := tableNamed(t, cat, "events"); ti.kind != "table" || ti.shadow {
		t.Errorf("events: %+v", ti)
	}
	if ti := tableNamed(t, cat, "v_errors"); ti.kind != "view" {
		t.Errorf("v_errors: %+v", ti)
	}
	if ti := tableNamed(t, cat, "notes"); ti.kind != "fts5" {
		t.Errorf("notes: %+v", ti)
	}
	if ti := tableNamed(t, cat, "notes_data"); !ti.shadow {
		t.Errorf("notes_data should be shadow: %+v", ti)
	}
	if ti := tableNamed(t, cat, "docs_fts"); ti.ftsContent != "docs" {
		t.Errorf("docs_fts content = %q, want docs", ti.ftsContent)
	}
	if ti := tableNamed(t, cat, "ghost"); !ti.contentless {
		t.Errorf("ghost should be contentless: %+v", ti)
	}
	// multi_fts_a uses content='multi' (external content), so FTS5 never
	// creates a multi_fts_a_content shadow table — a user table that
	// happens to have that name must not be misidentified as one.
	if ti := tableNamed(t, cat, "multi_fts_a_content"); ti.kind != "table" || ti.shadow {
		t.Errorf("multi_fts_a_content: %+v", ti)
	}
	// notes2 declares a plain column literally named "content" (no "="),
	// which must not be mistaken for the content= option: it stays in
	// default (internal) content mode, so its real notes2_content shadow
	// table must still be recognized as shadow.
	if ti := tableNamed(t, cat, "notes2"); ti.kind != "fts5" || ti.ftsContent != "" || ti.contentless {
		t.Errorf("notes2: %+v, want internal content mode", ti)
	}
	if ti := tableNamed(t, cat, "notes2_content"); !ti.shadow {
		t.Errorf("notes2_content should be shadow (genuine internal-content table): %+v", ti)
	}
	// notes3's module name is quoted (USING "fts5"(...)) rather than a
	// bareword — must still classify as fts5, not an ordinary table.
	if ti := tableNamed(t, cat, "notes3"); ti.kind != "fts5" {
		t.Errorf("notes3: %+v, want kind fts5 (quoted module name)", ti)
	}
	// FtsMixed is external-content (mixed-case table name); its lowercase
	// lookalike ftsmixed_content must still be recognized as a legit user
	// table, not hidden as a shadow table, despite the case difference
	// between the fts5 table's own name and the lookalike's prefix.
	if ti := tableNamed(t, cat, "ftsmixed_content"); ti.kind != "table" || ti.shadow {
		t.Errorf("ftsmixed_content: %+v, want ordinary non-shadow table", ti)
	}
	// SQLite's identifier fold is ASCII-only, so names differing only by
	// a non-ASCII case pair are distinct tables that must be classified
	// independently. A Unicode fold (strings.ToLower) collapses "Äfts"
	// and "äfts" onto one name-map key; whichever loses the collision
	// takes the other's content mode, and Äfts_content — a genuine
	// shadow table, since Äfts is internal-content — gets exposed as a
	// user table by äfts's external-content mode.
	if ti := tableNamed(t, cat, "Äfts"); ti.kind != "fts5" || ti.ftsContent != "" || ti.contentless {
		t.Errorf("Äfts: %+v, want fts5 in internal content mode", ti)
	}
	if ti := tableNamed(t, cat, "äfts"); ti.kind != "fts5" || ti.ftsContent != "unisrc" {
		t.Errorf("äfts: %+v, want fts5 with content=unisrc", ti)
	}
	if ti := tableNamed(t, cat, "Äfts_content"); !ti.shadow {
		t.Errorf("Äfts_content should stay shadow: %+v", ti)
	}
	if ti := tableNamed(t, cat, "Ätable"); ti.kind != "table" || ti.shadow || ti.withoutRowid {
		t.Errorf("Ätable: %+v, want ordinary rowid table", ti)
	}
	if ti := tableNamed(t, cat, "ätable"); ti.kind != "table" || ti.shadow || !ti.withoutRowid {
		t.Errorf("ätable: %+v, want ordinary WITHOUT ROWID table", ti)
	}
}

func TestASCIIFold(t *testing.T) {
	// SQLite folds A-Z only; every other byte, including the upper half
	// of a UTF-8 sequence, must survive untouched.
	cases := []struct{ in, want string }{
		{"Ätable", "Ätable"},
		{"ätable", "ätable"},
		{"FtsMixed", "ftsmixed"},
		{"_Content", "_content"},
		{"", ""},
		{"already", "already"},
	}
	for _, c := range cases {
		if got := asciiFold(c.in); got != c.want {
			t.Errorf("asciiFold(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if asciiFold("Ätable") == asciiFold("ätable") {
		t.Error("asciiFold collapsed two identifiers SQLite keeps distinct")
	}
	if !asciiEqualFold("FTS5", "fts5") {
		t.Error("asciiEqualFold(FTS5, fts5) = false, want true")
	}
	if asciiEqualFold("Äfts", "äfts") {
		t.Error("asciiEqualFold folded a non-ASCII case pair")
	}
	// strings.EqualFold folds the long s (U+017F) onto s; SQLite does
	// not, so a module spelled that way is not the fts5 module.
	if asciiEqualFold("ftſ5", "fts5") {
		t.Error("asciiEqualFold folded U+017F onto s")
	}
}

func TestCatalogIdentityResolution(t *testing.T) {
	cat := catalogOf(t)
	cases := []struct {
		table string
		want  identity
	}{
		{"events", idAlias},
		{"config", idPK}, // WITHOUT ROWID renders pk
		{"ipkshadow", idIntegerPK},
		{"shadowed", idPK},
		{"shadnull", idNone},    // NULL in declared PK: skip
		{"notes", idAlias},      // fts5 tables get identity too (Task 10 scans them)
		{"notes_data", idAlias}, // rowid-backed shadow table
		{"notes_idx", idPK},     // fts5 shadow tables that are WITHOUT ROWID
		{"notes_config", idPK},  // must resolve via idPK, not the alias path
		{"ipkdesc", idPK},       // INTEGER PRIMARY KEY DESC is not a rowid alias
		{"Ätable", idAlias},     // distinct from ätable: SQLite folds ASCII only
		{"ätable", idPK},        // WITHOUT ROWID, unlike its uppercase-umlaut twin
	}
	for _, c := range cases {
		if ti := tableNamed(t, cat, c.table); ti.ident != c.want {
			t.Errorf("%s identity = %v, want %v", c.table, ti.ident, c.want)
		}
	}
	if ti := tableNamed(t, cat, "ipkshadow"); ti.idExpr != `"id"` {
		t.Errorf("ipkshadow idExpr = %q, want quoted id", ti.idExpr)
	}
	// Composite PK columns must keep declared ordinal order.
	if ti := tableNamed(t, cat, "notes_idx"); len(ti.pkCols) != 2 ||
		ti.pkCols[0] != "segid" || ti.pkCols[1] != "term" {
		t.Errorf("notes_idx pkCols = %v, want [segid term]", ti.pkCols)
	}
	// INTEGER PRIMARY KEY DESC must not be treated as the rowid alias:
	// it renders via the declared-PK path instead.
	if ti := tableNamed(t, cat, "ipkdesc"); ti.ident != idPK ||
		len(ti.pkCols) != 1 || ti.pkCols[0] != "a" {
		t.Errorf("ipkdesc: %+v, want idPK with pkCols [a]", ti)
	}
}

func TestCatalogGeneratedColumnsEnumerated(t *testing.T) {
	ti := tableNamed(t, catalogOf(t), "gen")
	var names []string
	for _, c := range ti.cols {
		names = append(names, c.name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "up") || !strings.Contains(joined, "oid") {
		t.Errorf("gen cols = %v, want generated columns included", names)
	}
	// The generated column named oid shadows the alias, but rowid and
	// _rowid_ remain usable.
	if ti.ident != idAlias {
		t.Errorf("gen identity = %v, want idAlias via unshadowed alias", ti.ident)
	}
}

// TestParseVirtualTableDDL exercises the tokenizer-based parser directly
// against DDL strings a regex over raw text would misread: text that
// merely looks like "USING fts5" or "content=" inside a quoted name,
// comment, or unrelated column, plus module/module-argument forms an
// unanchored regex previously could not distinguish. modernc.org/sqlite
// only ships the fts5 module, so the module-name cases below (a
// different real module, e.g. fts4) are exercised as pure parser unit
// tests against crafted DDL text rather than against a real virtual
// table.
func TestParseVirtualTableDDL(t *testing.T) {
	cases := []struct {
		name       string
		sql        string
		wantOK     bool
		wantModule string
		wantHasC   bool
		wantC      string
	}{
		{
			name:       "quoted table name containing USING fts5 text, real module is fts4",
			sql:        `CREATE VIRTUAL TABLE "sneaky USING fts5 trick" USING fts4(a, b)`,
			wantOK:     true,
			wantModule: "fts4",
		},
		{
			name:       "bareword column named content is not the content= option",
			sql:        `CREATE VIRTUAL TABLE notes USING fts5(content, body)`,
			wantOK:     true,
			wantModule: "fts5",
		},
		{
			name:       "content= inside a block comment is ignored",
			sql:        `CREATE VIRTUAL TABLE notes USING fts5(body /* content='x' */)`,
			wantOK:     true,
			wantModule: "fts5",
		},
		{
			name:       "content= inside a line comment before the statement is ignored",
			sql:        "-- content='x'\nCREATE VIRTUAL TABLE notes USING fts5(body)",
			wantOK:     true,
			wantModule: "fts5",
		},
		{
			name:       "fts4-style DDL with double-quoted content value",
			sql:        `CREATE VIRTUAL TABLE old USING fts4(a, b, content="mytable")`,
			wantOK:     true,
			wantModule: "fts4",
			wantHasC:   true,
			wantC:      "mytable",
		},
		{
			name:       "content='' is contentless (empty, not absent)",
			sql:        `CREATE VIRTUAL TABLE ghost USING fts5(body, content='')`,
			wantOK:     true,
			wantModule: "fts5",
			wantHasC:   true,
			wantC:      "",
		},
		{
			name:       "content=bareword table reference",
			sql:        `CREATE VIRTUAL TABLE docs_fts USING fts5(title, body, content=docs, content_rowid='id')`,
			wantOK:     true,
			wantModule: "fts5",
			wantHasC:   true,
			wantC:      "docs",
		},
		{
			name:       "content_rowid is not mistaken for a content= argument",
			sql:        `CREATE VIRTUAL TABLE docs_fts USING fts5(title, body, content_rowid='id')`,
			wantOK:     true,
			wantModule: "fts5",
			wantHasC:   false,
		},
		{
			name:       "IF NOT EXISTS is skipped",
			sql:        `CREATE VIRTUAL TABLE IF NOT EXISTS notes USING fts5(body)`,
			wantOK:     true,
			wantModule: "fts5",
		},
		{
			name:       "double-quoted module name",
			sql:        `CREATE VIRTUAL TABLE t USING "fts5"(body)`,
			wantOK:     true,
			wantModule: "fts5",
		},
		{
			name:       "bracketed module name",
			sql:        `CREATE VIRTUAL TABLE t USING [fts5](body)`,
			wantOK:     true,
			wantModule: "fts5",
		},
		{
			name:       "backtick-quoted module name",
			sql:        "CREATE VIRTUAL TABLE t USING `fts5`(body)",
			wantOK:     true,
			wantModule: "fts5",
		},
		{
			name:   "an ordinary CREATE TABLE does not parse as a virtual table",
			sql:    `CREATE TABLE notes (body TEXT)`,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseVirtualTableDDL(c.sql)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if !strings.EqualFold(got.module, c.wantModule) {
				t.Errorf("module = %q, want %q", got.module, c.wantModule)
			}
			if got.hasContent != c.wantHasC {
				t.Errorf("hasContent = %v, want %v", got.hasContent, c.wantHasC)
			}
			if got.hasContent && got.content != c.wantC {
				t.Errorf("content = %q, want %q", got.content, c.wantC)
			}
		})
	}
}
