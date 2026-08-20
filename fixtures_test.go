package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

var fixtureStmts = []string{
	`CREATE TABLE events (id INTEGER PRIMARY KEY, message TEXT, level TEXT, ratio REAL)`,
	`INSERT INTO events (message, level, ratio) VALUES
		('connection timeout after 30s', 'error', 1.0),
		('retry timeout exceeded', 'warn', 2.5),
		('all systems nominal', 'info', 0.5),
		('line one' || char(10) || 'timeout on line two', 'error', 3.0)`,
	`CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT) WITHOUT ROWID`,
	`INSERT INTO config VALUES ('timeout', '30 timeout'), ('retries', '5')`,
	`CREATE VIEW v_errors AS SELECT message FROM events WHERE level = 'error'`,
	`CREATE VIRTUAL TABLE notes USING fts5(body)`,
	`INSERT INTO notes (body) VALUES ('grep for sqlite'), ('ripgrep habits transfer')`,
	`CREATE TABLE docs (id INTEGER PRIMARY KEY, title TEXT, body TEXT, slug TEXT)`,
	`CREATE VIRTUAL TABLE docs_fts USING fts5(title, body, content='docs', content_rowid='id')`,
	`CREATE TRIGGER docs_ai AFTER INSERT ON docs BEGIN
		INSERT INTO docs_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
	END`,
	`CREATE TRIGGER docs_ad AFTER DELETE ON docs BEGIN
		INSERT INTO docs_fts(docs_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
	END`,
	`CREATE TRIGGER docs_au AFTER UPDATE ON docs BEGIN
		INSERT INTO docs_fts(docs_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
		INSERT INTO docs_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
	END`,
	`INSERT INTO docs (title, body, slug) VALUES
		('intro', 'sqlite databases reward full text search', 'intro-slug'),
		('guide', 'the timeout setting matters for agents', 'guide-slug')`,
	`CREATE VIRTUAL TABLE ghost USING fts5(body, content='')`,
	`INSERT INTO ghost (rowid, body) VALUES (1, 'phantom text')`,
	`CREATE TABLE multi (id INTEGER PRIMARY KEY, txt TEXT)`,
	`INSERT INTO multi (txt) VALUES ('shared source row')`,
	`CREATE VIRTUAL TABLE multi_fts_a USING fts5(txt, content='multi', content_rowid='id')`,
	`CREATE VIRTUAL TABLE multi_fts_b USING fts5(txt, content='multi', content_rowid='id')`,
	`CREATE TABLE shadowed ("rowid" TEXT, "_rowid_" TEXT, "oid" TEXT, code TEXT PRIMARY KEY)`,
	`INSERT INTO shadowed VALUES ('r1', 'r2', 'r3', 'alpha'), ('r4', 'r5', 'r6', 'beta')`,
	`CREATE TABLE shadnull ("rowid" TEXT, "_rowid_" TEXT, "oid" TEXT, code TEXT PRIMARY KEY)`,
	`INSERT INTO shadnull VALUES ('x', 'y', 'z', NULL)`,
	`CREATE TABLE ipkshadow (id INTEGER PRIMARY KEY, "rowid" TEXT, "_rowid_" TEXT, "oid" TEXT)`,
	`INSERT INTO ipkshadow ("rowid", "_rowid_", "oid") VALUES ('a', 'b', 'timeout in shadow')`,
	`CREATE TABLE gen (a TEXT, up TEXT GENERATED ALWAYS AS (upper(a)) VIRTUAL,
		"oid" TEXT GENERATED ALWAYS AS (a || '!') STORED)`,
	`INSERT INTO gen (a) VALUES ('timeout lore')`,
	`CREATE TABLE blobs (id INTEGER PRIMARY KEY, data BLOB, note TEXT)`,
	`INSERT INTO blobs (data, note) VALUES (x'74696d656f7574', 'timeout in a text column'), (NULL, NULL)`,
	`CREATE TABLE ipkdesc ("rowid" TEXT, "_rowid_" TEXT, "oid" TEXT, a INTEGER PRIMARY KEY DESC)`,
	`INSERT INTO ipkdesc (a) VALUES (1)`,
	`CREATE TABLE multi_fts_a_content (id INTEGER PRIMARY KEY, note TEXT)`,
	`INSERT INTO multi_fts_a_content (note) VALUES ('legit user table, not a shadow')`,
	`CREATE VIRTUAL TABLE notes2 USING fts5(content, body)`,
	`INSERT INTO notes2 (content, body) VALUES ('lookalike column', 'not a content= option')`,
	`CREATE VIRTUAL TABLE notes3 USING "fts5"(body)`,
	`INSERT INTO notes3 (body) VALUES ('quoted module name still means fts5')`,
	`CREATE TABLE SrcMixed (id INTEGER PRIMARY KEY, txt TEXT)`,
	`INSERT INTO SrcMixed (txt) VALUES ('mixed-case source row')`,
	`CREATE VIRTUAL TABLE FtsMixed USING fts5(txt, content='SrcMixed', content_rowid='id')`,
	`CREATE TABLE ftsmixed_content (id INTEGER PRIMARY KEY, note TEXT)`,
	`INSERT INTO ftsmixed_content (note) VALUES ('legit user table, case-folded lookalike')`,
	// SQLite folds identifier case for ASCII only, so "Äfts" and "äfts"
	// are two distinct fts5 tables in one schema, each with its own
	// shadow tables. "Äfts" is in default (internal) content mode and
	// therefore really does own an Äfts_content shadow table; "äfts" is
	// external-content and owns none. A Unicode case fold would collapse
	// the two names together and resolve Äfts_content against the wrong
	// base table's content mode.
	`CREATE TABLE unisrc (id INTEGER PRIMARY KEY, txt TEXT)`,
	`INSERT INTO unisrc (txt) VALUES ('unicode fold source row')`,
	`CREATE VIRTUAL TABLE "Äfts" USING fts5(body)`,
	`INSERT INTO "Äfts" (body) VALUES ('timeout in an internal-content table')`,
	`CREATE VIRTUAL TABLE "äfts" USING fts5(txt, content='unisrc', content_rowid='id')`,
	`CREATE TABLE "Ätable" (id INTEGER PRIMARY KEY, note TEXT)`,
	`INSERT INTO "Ätable" (note) VALUES ('uppercase-umlaut table')`,
	`CREATE TABLE "ätable" (code TEXT PRIMARY KEY, note TEXT) WITHOUT ROWID`,
	`INSERT INTO "ätable" VALUES ('a', 'lowercase-umlaut table')`,
	`CREATE TABLE defaults_t (id INTEGER PRIMARY KEY, level TEXT DEFAULT 'info', count INTEGER DEFAULT 0)`,
	`INSERT INTO defaults_t (level, count) VALUES ('warn', 5)`,
	`CREATE INDEX expr_idx ON events (lower(message))`,
	`CREATE TABLE fk_target (id INTEGER PRIMARY KEY, name TEXT)`,
	`INSERT INTO fk_target (name) VALUES ('test')`,
	`CREATE TABLE fk_source (id INTEGER PRIMARY KEY, target_id INTEGER REFERENCES fk_target)`,
	`INSERT INTO fk_source (target_id) VALUES (1)`,
	`CREATE TABLE metrics (id INTEGER PRIMARY KEY, value REAL, count INTEGER)`,
	`INSERT INTO metrics (value, count) VALUES (1.5, 3), (2.5, 4)`,
	// A partial index with a descending, collated key: everything about
	// this index that PRAGMA index_info cannot report, and that only its
	// DDL preserves. Indexes are absent from litefind's catalog (it reads
	// sqlite_master for tables and views only), so this adds nothing to
	// the `tables` or search fixtures — only to `schema metrics`.
	`CREATE INDEX metrics_hot ON metrics (value DESC, count COLLATE NOCASE) WHERE count > 3`,
}

// fixturePath builds the shared fixture database and returns its path.
func fixturePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range fixtureStmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture stmt failed: %v\n%s", err, stmt)
		}
	}
	return path
}

// brokenViewStmts build a database holding one ordinary table and one
// view left dangling by a dropped table. Compiling that view's SELECT is
// what PRAGMA table_xinfo does, so the view cannot report its columns —
// a condition that must not make the rest of the database unreadable.
var brokenViewStmts = []string{
	`CREATE TABLE keep (id INTEGER PRIMARY KEY, note TEXT)`,
	`INSERT INTO keep (note) VALUES ('kept intact')`,
	`CREATE TABLE gone (id INTEGER PRIMARY KEY, note TEXT)`,
	`CREATE VIEW v_gone AS SELECT note FROM gone`,
	`DROP TABLE gone`,
}

// brokenViewPath builds the dangling-view database and returns its path.
func brokenViewPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brokenview.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range brokenViewStmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("broken-view fixture stmt failed: %v\n%s", err, stmt)
		}
	}
	return path
}

func TestFixtureBuilds(t *testing.T) {
	path := fixturePath(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("events rows = %d, want 4", n)
	}
}
