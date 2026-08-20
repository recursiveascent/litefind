package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type database struct{ sql *sql.DB }

// openRO opens path read-only (mode=ro, busy_timeout 5000ms). immutable
// adds immutable=1 — caller-asserted static file, never assumed. It
// verifies the file is a SQLite database by querying sqlite_master and
// returns diagnosable one-line errors: missing file, not a database,
// locked, and the live-WAL + read-only-directory condition with remedy.
func openRO(path string, immutable bool) (*database, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: no such file", path)
		}
		// Anything else — an unreadable parent directory, a path element
		// that isn't a directory, an I/O failure — is a different
		// condition entirely and reporting it as "no such file" sends the
		// user looking for the wrong problem. os.Stat's *fs.PathError
		// already names the path, so only its cause is interpolated here;
		// wrapping keeps errors.Is working on the underlying error.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("%s: %w", path, pe.Err)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dsn := "file:" + url.PathEscape(filepath.ToSlash(path)) + "?mode=ro&_pragma=busy_timeout(5000)"
	if immutable {
		dsn += "&immutable=1"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Force a real read so header validation and WAL setup happen now.
	if _, err := db.Exec(`SELECT count(*) FROM sqlite_master`); err != nil {
		_ = db.Close() // best effort; the query error is what we report
		return nil, diagnoseOpen(path, err)
	}
	return &database{sql: db}, nil
}

func (d *database) close() error { return d.sql.Close() }

// diagnoseOpen maps SQLite open/read errors onto the spec's one-line
// messages with remedies.
func diagnoseOpen(path string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not a database"):
		return fmt.Errorf("%s: not a SQLite database", path)
	case strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY"):
		return fmt.Errorf("%s: database is locked (busy_timeout 5000ms elapsed)", path)
	case walBlocked(path, msg):
		return fmt.Errorf("%s: live WAL database in an unwritable directory — SQLite needs to create the -shm sidecar to read it; make the directory writable, checkpoint the database, or pass --immutable if the file genuinely cannot change", path)
	default:
		return fmt.Errorf("%s: %v", path, err)
	}
}

// walBlocked reports whether the failure pattern matches the read-only
// WAL condition: a -wal sidecar exists but SQLite couldn't open/create
// what it needs.
func walBlocked(path, msg string) bool {
	if _, err := os.Stat(path + "-wal"); err != nil {
		return false
	}
	return strings.Contains(msg, "unable to open database") ||
		strings.Contains(msg, "SQLITE_CANTOPEN") ||
		strings.Contains(msg, "readonly")
}

// identity is how a table's rows are addressed for a targeted rowid/PK
// lookup during scanning.
type identity int

const (
	idAlias     identity = iota // unshadowed rowid/_rowid_/oid
	idIntegerPK                 // alias shadowed; INTEGER PRIMARY KEY column is the rowid
	idPK                        // declared PK fallback, verified non-NULL; render as pk=(...)
	idNone                      // skip table with stderr warning
)

// colInfo is one PRAGMA table_xinfo row for a table.
type colInfo struct {
	name, declType string
	hidden         int // table_xinfo: 0 plain, 1 virtual-table hidden, 2 gen virtual, 3 gen stored
	pk             int // 1-based position in PK, 0 if not
	notNull        bool
	dflt           sql.NullString // default value expression, if any
}

// tableInfo describes one sqlite_master entry (table or view) with its
// searchable columns and resolved row identity.
type tableInfo struct {
	name, kind      string // kind: "table", "view", "fts5"
	shadow          bool   // sqlite_* internal or FTS5 shadow table
	withoutRowid    bool
	cols            []colInfo // searchable enumeration: hidden 0, 2, 3
	xinfoErr        error     // views only: column introspection failed (see catalog)
	ident           identity
	idExpr          string   // SQL identifier for idAlias/idIntegerPK (quoted)
	pkCols          []string // for idPK and WITHOUT ROWID rendering
	ftsContent      string   // fts5 only: content= option ("" none/contentless-flag below)
	contentless     bool     // fts5 with content=''
	ftsContentRowid string   // fts5 only: content_rowid= option, defaulted to "rowid" per SQLite when omitted
}

// quoteIdent double-quotes a SQL identifier, doubling any embedded
// double quotes.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// asciiFold lowercases the bytes 'A'..'Z' and leaves every other byte
// alone. This is exactly the fold SQLite applies when it compares
// identifiers and keywords: its sqlite3UpperToLower table only maps the
// 26 ASCII uppercase letters, so identifiers that differ only outside
// ASCII stay distinct — CREATE TABLE "Ätable" and CREATE TABLE "ätable"
// both succeed in one schema.
//
// strings.ToLower folds the whole of Unicode, so it would map those two
// distinct names onto one key: in a name-keyed lookup that either hides
// a legitimate user table (matched against an unrelated base table) or
// exposes a genuine shadow table (matched against the wrong base's
// content mode). Every identifier comparison in this file uses this
// fold instead.
func asciiFold(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// asciiEqualFold reports whether a and b are equal under SQLite's
// ASCII-only case fold (see asciiFold). It is the identifier- and
// keyword-comparison counterpart to strings.EqualFold, which folds all
// of Unicode and would, for example, call the module name "ftſ5" equal
// to "fts5".
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// ddlTokenKind classifies one token from tokenizeDDL.
type ddlTokenKind int

const (
	ddlWord   ddlTokenKind = iota // bareword: identifier or keyword, matched case-insensitively
	ddlQuoted                     // "quoted", [bracketed], or `backtick` identifier — literal text, never a keyword
	ddlString                     // 'string literal' — literal text
	ddlPunct                      // one of ( ) ,  =
)

type ddlToken struct {
	kind ddlTokenKind
	text string
}

// tokenizeDDL breaks SQL DDL text into a flat token stream: it skips
// whitespace and -- / /* */ comments, and unescapes quoted identifiers
// ("x", [x], `x`) and string literals ('x') into their literal text, so
// later matching can tell "the bareword USING" from "the text USING
// appearing inside a quoted name, string, or comment" — something a
// regex over the raw DDL text can't do. Punctuation outside the small
// set this package's parsing needs — ( ) , = — is silently discarded;
// this is a lexer for a narrow grammar (CREATE VIRTUAL TABLE ... USING
// module(args)), not a general SQL tokenizer.
func tokenizeDDL(s string) []ddlToken {
	var toks []ddlToken
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && i+1 < n && s[i+1] == '-':
			if j := strings.IndexByte(s[i:], '\n'); j < 0 {
				i = n
			} else {
				i += j + 1
			}
		case c == '/' && i+1 < n && s[i+1] == '*':
			if j := strings.Index(s[i+2:], "*/"); j < 0 {
				i = n
			} else {
				i += j + 4
			}
		case c == '"' || c == '`':
			text, next := scanQuoted(s, i, c)
			toks = append(toks, ddlToken{ddlQuoted, text})
			i = next
		case c == '[':
			text, next := scanBracket(s, i)
			toks = append(toks, ddlToken{ddlQuoted, text})
			i = next
		case c == '\'':
			text, next := scanQuoted(s, i, '\'')
			toks = append(toks, ddlToken{ddlString, text})
			i = next
		case c == '(' || c == ')' || c == ',' || c == '=':
			toks = append(toks, ddlToken{ddlPunct, string(c)})
			i++
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentCont(s[j]) {
				j++
			}
			toks = append(toks, ddlToken{ddlWord, s[i:j]})
			i = j
		default:
			i++ // outside this grammar's alphabet; not significant to us
		}
	}
	return toks
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// scanQuoted scans a quote-delimited token starting at s[i] (where
// s[i] == quote), using SQL's standard doubling escape for an embedded
// quote character (a doubled single quote inside a string, or a doubled
// double quote inside an identifier). An unterminated quote consumes to
// the end of the string.
func scanQuoted(s string, i int, quote byte) (text string, next int) {
	n := len(s)
	var b strings.Builder
	i++ // skip opening quote
	for i < n {
		if s[i] == quote {
			if i+1 < n && s[i+1] == quote {
				b.WriteByte(quote)
				i += 2
				continue
			}
			i++
			break
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), i
}

// scanBracket scans a [bracketed identifier] starting at s[i] (where
// s[i] == '['). SQLite defines no escape for an embedded ']'; the token
// ends at the first one.
func scanBracket(s string, i int) (text string, next int) {
	n := len(s)
	j := i + 1
	k := strings.IndexByte(s[j:], ']')
	if k < 0 {
		return s[j:n], n
	}
	return s[j : j+k], j + k + 1
}

// vtabDDL holds the structurally-parsed pieces of a CREATE VIRTUAL
// TABLE statement that catalog() needs.
type vtabDDL struct {
	module          string
	hasContent      bool   // a whole "content = value" argument was present
	content         string // that argument's value (possibly empty: content='')
	hasContentRowid bool   // a whole "content_rowid = value" argument was present
	contentRowid    string // that argument's value
}

// parseVirtualTableDDL tokenizes a CREATE VIRTUAL TABLE statement
// (comments stripped, quoted identifiers and strings respected) and
// extracts the module name from its "USING <module>" clause, plus —
// when the module's argument list contains a whole top-level
// "content = value" and/or "content_rowid = value" argument — those
// arguments' values. It reports ok=false if the text doesn't parse as a
// CREATE VIRTUAL TABLE statement at all.
//
// The table name between TABLE and USING is deliberately not parsed as
// a single token: it's skipped up to the first *bareword* USING token,
// so a quoted table name whose text happens to contain "using fts5"
// can never be mistaken for the real clause — a quoted identifier is a
// different token kind than a keyword and is never compared against
// one.
func parseVirtualTableDDL(sqlText string) (vtabDDL, bool) {
	toks := tokenizeDDL(sqlText)
	i := 0
	word := func(w string) bool {
		if i < len(toks) && toks[i].kind == ddlWord && asciiEqualFold(toks[i].text, w) {
			i++
			return true
		}
		return false
	}

	if !word("CREATE") || !word("VIRTUAL") || !word("TABLE") {
		return vtabDDL{}, false
	}
	if word("IF") {
		word("NOT")
		word("EXISTS")
	}
	for i < len(toks) && (toks[i].kind != ddlWord || !asciiEqualFold(toks[i].text, "USING")) {
		i++
	}
	if i >= len(toks) {
		return vtabDDL{}, false
	}
	i++ // consume USING
	// The module name is normally a bareword, but SQLite also accepts a
	// quoted identifier here (USING "fts5"(...), USING [fts5](...),
	// USING `fts5`(...)); tokenizeDDL already unquotes those into their
	// literal text, so either token kind carries the real module name.
	if i >= len(toks) || (toks[i].kind != ddlWord && toks[i].kind != ddlQuoted) {
		return vtabDDL{}, false
	}
	result := vtabDDL{module: toks[i].text}
	i++

	if i >= len(toks) || toks[i].kind != ddlPunct || toks[i].text != "(" {
		return result, true // module invoked with no argument list
	}
	i++
	depth, start := 1, i
	for i < len(toks) && depth > 0 {
		if toks[i].kind == ddlPunct {
			switch toks[i].text {
			case "(":
				depth++
			case ")":
				depth--
			}
		}
		if depth > 0 {
			i++
		}
	}
	args := toks[start:i]

	// Split the argument-list tokens into top-level comma-separated
	// groups, respecting parens nested inside an argument (e.g. a
	// tokenizer= option's own quoted sub-arguments).
	var groups [][]ddlToken
	depth, last := 0, 0
	for j, t := range args {
		if t.kind != ddlPunct {
			continue
		}
		switch t.text {
		case "(":
			depth++
		case ")":
			depth--
		case ",":
			if depth == 0 {
				groups = append(groups, args[last:j])
				last = j + 1
			}
		}
	}
	groups = append(groups, args[last:])

	for _, g := range groups {
		if len(g) != 3 || g[0].kind != ddlWord || g[1].kind != ddlPunct || g[1].text != "=" || g[2].kind == ddlPunct {
			continue
		}
		switch {
		case asciiEqualFold(g[0].text, "content"):
			result.hasContent = true
			result.content = g[2].text
		case asciiEqualFold(g[0].text, "content_rowid"):
			result.hasContentRowid = true
			result.contentRowid = g[2].text
		}
	}
	return result, true
}

// tableListEntry is one PRAGMA table_list row for a main-schema object:
// the structural source of truth for WITHOUT ROWID and for
// table/view/virtual/shadow classification, so catalog() never has to
// guess those from DDL text.
type tableListEntry struct {
	typ string // "table", "view", "virtual", "shadow"
	wr  bool   // WITHOUT ROWID
}

// tableList runs PRAGMA table_list once and returns every main-schema
// entry keyed by name.
func (d *database) tableList() (map[string]tableListEntry, error) {
	rows, err := d.sql.Query(
		`SELECT name, type, wr FROM pragma_table_list() WHERE schema = 'main'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	m := make(map[string]tableListEntry)
	for rows.Next() {
		var (
			name, typ string
			wr        int
		)
		if err := rows.Scan(&name, &typ, &wr); err != nil {
			return nil, err
		}
		m[name] = tableListEntry{typ: typ, wr: wr != 0}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// catalog returns all tables and views in sqlite_master order with
// identity resolved per the spec's fallback chain (idPK requires the
// non-NULL EXISTS verification to have passed). A view whose columns
// cannot be introspected — it selects from a table that has since been
// dropped — is returned with no columns and its error recorded in
// xinfoErr rather than failing the whole catalog.
func (d *database) catalog() ([]tableInfo, error) {
	rows, err := d.sql.Query(
		`SELECT name, type, coalesce(sql, '') FROM sqlite_master
		 WHERE type IN ('table', 'view') ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type masterRow struct{ name, typ, sqlText string }
	var raw []masterRow
	for rows.Next() {
		var m masterRow
		if err := rows.Scan(&m.name, &m.typ, &m.sqlText); err != nil {
			return nil, err
		}
		raw = append(raw, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	tl, err := d.tableList()
	if err != nil {
		return nil, err
	}

	// First pass: kind, WITHOUT ROWID, shadow, and FTS5 metadata, all
	// from table_list's structural classification. "sqlite_"-prefixed
	// names are reserved by SQLite itself (a CREATE TABLE with that
	// prefix is rejected), so that prefix check is exact, not a
	// heuristic; table_list's own "shadow" type covers a virtual table
	// module's auxiliary tables without name-suffix guessing of our own.
	cat := make([]tableInfo, len(raw))
	for i, m := range raw {
		entry := tl[m.name]
		ti := tableInfo{
			name:         m.name,
			withoutRowid: entry.wr,
			shadow:       strings.HasPrefix(m.name, "sqlite_") || entry.typ == "shadow",
		}
		switch {
		case m.typ == "view":
			ti.kind = "view"
		case entry.typ == "virtual":
			parsed, ok := parseVirtualTableDDL(m.sqlText)
			if ok && asciiEqualFold(parsed.module, "fts5") {
				ti.kind = "fts5"
				ti.ftsContent = parsed.content
				ti.contentless = parsed.hasContent && parsed.content == ""
				if parsed.hasContentRowid {
					ti.ftsContentRowid = parsed.contentRowid
				} else {
					ti.ftsContentRowid = "rowid" // SQLite's own default when content= is given without content_rowid
				}
			} else {
				ti.kind = "table"
			}
		default:
			ti.kind = "table"
		}
		cat[i] = ti
	}

	// table_list's "shadow" classification comes from the virtual-table
	// module's own xShadowName naming-convention check, which — for
	// FTS5 — can't see that content='' or content=<table> (external
	// content) mode means no base_content table is ever created; it
	// only recognizes the *_content name pattern, not whether this
	// particular base table actually has one. Verified against real
	// SQLite (modernc.org/sqlite, bundled 3.53.3): a plain user table
	// named base_content next to an external-content fts5 table named
	// base comes back from table_list as type='shadow' too.
	//
	// Correct only that specific, confirmed blind spot: table_list's
	// classification is trusted by default (including for FTS3/FTS4 and
	// any other module, which do have genuine _content shadow tables of
	// their own that this package has no special knowledge of) and is
	// overridden to false only when the *_content name's base is
	// confirmed — by catalog()'s own parse of the base's DDL — to be an
	// fts5 table in external-content or contentless mode, the two modes
	// where FTS5 provably creates no _content shadow table.
	// SQLite folds identifier case for matching, so both the map keys
	// below and the "_content" suffix check must be case-insensitive
	// too: a real fts5 table named "FtsMixed" and a colliding user table
	// named "ftsmixed_content" are the same match to SQLite's own
	// xShadowName check, even though their spellings differ in case. The
	// fold has to be asciiFold and not strings.ToLower, because SQLite
	// folds ASCII only: "Äfts" and "äfts" are two distinct fts5 tables
	// to SQLite, each with its own shadow tables, and collapsing them
	// onto one map key would resolve "Äfts_content" against the wrong
	// base table's content mode.
	byName := make(map[string]int, len(cat))
	for i, ti := range cat {
		byName[asciiFold(ti.name)] = i
	}
	for i := range cat {
		folded := asciiFold(cat[i].name)
		if !cat[i].shadow || !strings.HasSuffix(folded, "_content") {
			continue
		}
		base, ok := byName[strings.TrimSuffix(folded, "_content")]
		if !ok {
			continue // no matching base at all; trust table_list
		}
		b := cat[base]
		if b.kind != "fts5" {
			continue // some other module; trust table_list
		}
		if b.contentless || b.ftsContent != "" {
			cat[i].shadow = false
		}
	}

	// Second pass: columns and identity resolution.
	//
	// A view's columns come from compiling its SELECT, so a view left
	// dangling by a dropped (or renamed) table fails table_xinfo even
	// though every other object in the database is perfectly readable.
	// That failure is recorded on the view and the catalog is returned
	// intact: search ignores views entirely, `tables` still lists the view
	// with zero columns, and `schema` reports the error against that one
	// view. A table failing xinfo is still fatal — nothing can be scanned
	// without its columns.
	for i := range cat {
		xinfo, err := d.tableXInfo(cat[i].name)
		if err != nil {
			if cat[i].kind == "view" {
				cat[i].xinfoErr = fmt.Errorf("%s: %w", cat[i].name, err)
				continue
			}
			return nil, fmt.Errorf("%s: %w", cat[i].name, err)
		}
		for _, c := range xinfo {
			if c.hidden == 0 || c.hidden == 2 || c.hidden == 3 {
				cat[i].cols = append(cat[i].cols, c)
			}
		}
		if cat[i].kind == "view" {
			continue
		}
		if err := d.resolveIdentity(&cat[i], xinfo); err != nil {
			return nil, fmt.Errorf("%s: %w", cat[i].name, err)
		}
	}

	return cat, nil
}

// tableXInfo runs PRAGMA table_xinfo(name) and returns every column,
// unfiltered (including hidden columns) — callers filter as needed.
func (d *database) tableXInfo(name string) ([]colInfo, error) {
	rows, err := d.sql.Query(`PRAGMA table_xinfo(` + quoteIdent(name) + `)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var cols []colInfo
	for rows.Next() {
		var (
			cid                 int
			colName, declType   string
			notNull, pk, hidden int
			dflt                sql.NullString
		)
		if err := rows.Scan(&cid, &colName, &declType, &notNull, &dflt, &pk, &hidden); err != nil {
			return nil, err
		}
		cols = append(cols, colInfo{
			name:     colName,
			declType: declType,
			hidden:   hidden,
			pk:       pk,
			notNull:  notNull != 0,
			dflt:     dflt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

// resolveIdentity implements the spec's fallback chain, in order:
//
//  1. WITHOUT ROWID: no rowid aliases exist at all, so the alias check
//     below would wrongly succeed against an alias name that isn't
//     actually usable — this must run first. FTS5's own *_idx and
//     *_config shadow tables are WITHOUT ROWID, so this is exercised
//     for real by --all-tables, not just the declared `config` table.
//  2. An unshadowed rowid/_rowid_/oid alias.
//  3. A lone INTEGER PRIMARY KEY column (rowid by another name) — unless
//     it was declared "INTEGER PRIMARY KEY DESC", SQLite's documented
//     exception where the DESC keyword suppresses the rowid alias,
//     leaving an ordinary indexed PK that may hold NULLs; that case
//     falls through to step 4 instead.
//  4. A declared PK verified to never be NULL.
//  5. idNone — no safe way to address rows.
func (d *database) resolveIdentity(ti *tableInfo, xinfo []colInfo) error {
	if ti.withoutRowid {
		ti.ident = idPK
		ti.pkCols = pkColsOf(xinfo)
		return nil
	}

	declared := make(map[string]bool, len(xinfo))
	for _, c := range xinfo {
		declared[asciiFold(c.name)] = true
	}
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		if !declared[alias] {
			ti.ident = idAlias
			ti.idExpr = quoteIdent(alias)
			return nil
		}
	}

	pkCount := 0
	var onlyPK colInfo
	for _, c := range xinfo {
		if c.pk > 0 {
			pkCount++
			if c.pk == 1 {
				onlyPK = c
			}
		}
	}
	if pkCount == 1 && asciiEqualFold(onlyPK.declType, "INTEGER") {
		// "INTEGER PRIMARY KEY DESC" builds a real index for the PK
		// (origin='pk' in PRAGMA index_list); a true rowid alias gets no
		// such index because it *is* the rowid, not a separate indexed
		// value. Only treat this as the rowid alias when no such index
		// exists.
		descPK, err := d.hasPKIndex(ti.name)
		if err != nil {
			return err
		}
		if !descPK {
			ti.ident = idIntegerPK
			ti.idExpr = quoteIdent(onlyPK.name)
			return nil
		}
	}

	if pkCols := pkColsOf(xinfo); len(pkCols) > 0 {
		ok, err := pkAllNonNull(d.sql, ti.name, pkCols)
		if err != nil {
			return err
		}
		if ok {
			ti.ident = idPK
			ti.pkCols = pkCols
			return nil
		}
	}

	ti.ident = idNone
	return nil
}

// pkColsOf returns declared primary-key column names in ordinal order
// (table_xinfo's pk field is the column's 1-based position within a
// composite primary key).
func pkColsOf(xinfo []colInfo) []string {
	highest := 0
	for _, c := range xinfo {
		if c.pk > highest {
			highest = c.pk
		}
	}
	if highest == 0 {
		return nil
	}
	cols := make([]string, highest)
	for _, c := range xinfo {
		if c.pk > 0 {
			cols[c.pk-1] = c.name
		}
	}
	return cols
}

// hasPKIndex reports whether table has a PRAGMA index_list entry with
// origin='pk'. SQLite builds one for a declared PK it treats as an
// ordinary indexed value — including "INTEGER PRIMARY KEY DESC" — but
// never for a column that is genuinely the rowid by another name.
func (d *database) hasPKIndex(table string) (bool, error) {
	rows, err := d.sql.Query(`PRAGMA index_list(` + quoteIdent(table) + `)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			seq, unique, partial int
			name, origin         string
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return false, err
		}
		if origin == "pk" {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// rowQueryer is the slice of *sql.DB and *sql.Tx that pkAllNonNull needs.
// The check has to run in both: once at catalog time to resolve identity,
// and again inside each scan's own transaction, where its verdict is
// binding for the whole scan.
type rowQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

// pkAllNonNull reports whether no row visible to q has a NULL in any of
// the given PK columns — the test for whether a declared PK is a stable
// row identity (WITHOUT ROWID tables skip it; their PKs cannot be NULL by
// construction).
//
// Run against the database, at catalog time, it is only advisory: a
// concurrent writer can insert a NULL-keyed row between the check and the
// scan that reads it, so it merely saves the cost of starting a scan that
// would be abandoned. Run against a scan's transaction, before that scan
// emits anything, it is authoritative — the snapshot it inspects is the
// same one every batch cursor reads.
func pkAllNonNull(q rowQueryer, table string, pkCols []string) (bool, error) {
	conds := make([]string, len(pkCols))
	for i, c := range pkCols {
		conds[i] = quoteIdent(c) + " IS NULL"
	}
	query := `SELECT EXISTS(SELECT 1 FROM ` + quoteIdent(table) + ` WHERE ` +
		strings.Join(conds, " OR ") + `)`
	var hasNull bool
	if err := q.QueryRow(query).Scan(&hasNull); err != nil {
		return false, err
	}
	return !hasNull, nil
}
