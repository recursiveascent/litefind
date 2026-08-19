package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ftsTarget is one resolved FTS5 search target.
type ftsTarget struct {
	index        string // FTS5 table to query
	source       string // external-content source ("" when the index is queried standalone)
	contentRowid string // source's content_rowid column name; meaningful only when source != ""
}

// missError reports that a table explicitly scoped by -t has no FTS5
// index covering it. Its Error() text is the full setup guidance built by
// ftsMissError: complete runnable SQL (CREATE VIRTUAL TABLE + rebuild + 3
// triggers) for a rowid-accessible table, or the standalone-FTS diagnostic
// for a WITHOUT ROWID or rowid-inaccessible one. Kept as a distinct type
// rather than a bare fmt.Errorf so callers can still recover the table it
// names, even though the message itself already carries everything a user
// needs.
type missError struct {
	table tableInfo
	msg   string
}

func (e missError) Error() string { return e.msg }

// ftsMissError builds the guidance for a scoped table with no index.
// Rowid-accessible tables (idAlias, idIntegerPK) get complete runnable SQL:
// CREATE VIRTUAL TABLE with external content, the ('rebuild') insert, and
// three AI/AD/AU triggers keyed on the rowid. WITHOUT ROWID and
// rowid-inaccessible tables (idPK) get the standalone-FTS diagnostic
// instead: PK columns UNINDEXED alongside the text columns, in a
// self-contained fts5 table with PK-keyed triggers — the table has no
// rowid to hang external content off of, so it must store its own text.
// A table with no stable identity at all (idNone) or no TEXT-affinity
// columns to index gets a plain diagnostic explaining why no SQL can be
// generated, rather than malformed SQL built from an empty column list.
func ftsMissError(ti tableInfo) error {
	name := quoteIdent(ti.name)

	if ti.ident == idNone {
		return missError{table: ti, msg: fmt.Sprintf(
			"%s has no FTS5 index and no stable row identity (rowid aliases shadowed, primary key nullable or absent), so litefind cannot generate a synchronized index for it; add a usable primary key or unshadow rowid, then retry",
			name)}
	}

	if ti.ident == idAlias || ti.ident == idIntegerPK {
		textCols := textAffinityCols(ti)
		if len(textCols) == 0 {
			return missError{table: ti, msg: fmt.Sprintf(
				"%s has no FTS5 index and no TEXT-affinity columns (TEXT/CHAR/CLOB) to index; no full-text index can be generated for it",
				name)}
		}
		intro := fmt.Sprintf("no FTS5 index covers %s; create one (litefind will use it on the next run):\n\n", name)
		return missError{table: ti, msg: intro + externalContentFTSSetupSQL(ti, textCols)}
	}

	// idPK: WITHOUT ROWID, or an ordinary table whose rowid is genuinely
	// inaccessible (every alias name shadowed, and no rowid-aliasing
	// INTEGER PRIMARY KEY — one declared DESC is an ordinary indexed
	// column, not the rowid).
	textCols := standaloneTextCols(ti)
	if len(textCols) == 0 {
		return missError{table: ti, msg: fmt.Sprintf(
			"%s has no FTS5 index and no TEXT-affinity columns beyond its primary key to index; no full-text index can be generated for it",
			name)}
	}
	ftsName := quoteIdent(ti.name + "_fts")
	intro := fmt.Sprintf(
		"no FTS5 index covers %s; create a standalone one — it has no content= mapping back to %s, so litefind won't find it via -t %s on its own; rerun naming the index directly, -t %s:\n\n",
		name, name, name, ftsName)
	return missError{table: ti, msg: intro + standaloneFTSSetupSQL(ti, textCols)}
}

// resolveFTS maps the scoped tables onto FTS5 indexes:
//   - scoped fts5 table -> direct target (error for contentless)
//   - scoped ordinary table with exactly one content= sibling -> mapped
//   - multiple siblings -> ambiguity error naming candidates
//   - no index -> miss error carrying setup SQL / diagnostic (Task 12)
//
// Scoping semantics: with no -t flags, the eligible set is every
// non-shadow fts5 table (direct targets only — unscoped searches never
// pull in source-table mapping); with -t globs, each matched table
// resolves per the rules above. Targets are deduplicated by index name
// (a glob like 'docs*' matches both docs and docs_fts, resolving the
// same index twice): catalog order is kept, and the entry carrying
// source metadata wins so --row fetches from the source table.
//
// -T (opts.notTables) prunes before any of the above resolves or
// errors: a table matching a -T glob neither resolves nor raises
// contentless/ambiguity/miss errors, in both the unscoped sweep and the
// -t-scoped path. For sibling mapping, -T also excludes a candidate
// index by its own name — "-t docs -T docs_fts" leaves docs with zero
// eligible siblings, which yields no target (not a miss error: the
// index exists, the user just excluded it).
func resolveFTS(cat []tableInfo, opts searchOpts) ([]ftsTarget, error) {
	excluded := func(name string) bool {
		return len(opts.notTables) > 0 && matchesAnyGlob(opts.notTables, name)
	}

	if len(opts.tables) == 0 {
		return unscopedFTSTargets(cat, excluded), nil
	}

	var raw []ftsTarget
	for _, ti := range cat {
		if ti.shadow || !matchesAnyGlob(opts.tables, ti.name) || excluded(ti.name) {
			continue
		}
		switch ti.kind {
		case "fts5":
			if ti.contentless {
				return nil, fmt.Errorf("%s is a contentless FTS5 table (content=''): it stores no text, so snippets and --row cannot be produced; search an external-content or regular FTS5 table instead", ti.name)
			}
			raw = append(raw, ftsTarget{index: ti.name, source: ti.ftsContent, contentRowid: ti.ftsContentRowid})
		case "table":
			siblings := ftsSiblingsOf(cat, ti.name)
			if len(siblings) == 0 {
				return nil, ftsMissError(ti)
			}
			var kept []tableInfo
			for _, s := range siblings {
				if !excluded(s.name) {
					kept = append(kept, s)
				}
			}
			switch len(kept) {
			case 0:
				// Every candidate index was excluded by -T: the index
				// genuinely exists, so this isn't a miss — just nothing
				// to resolve.
			case 1:
				raw = append(raw, ftsTarget{index: kept[0].name, source: ti.name, contentRowid: kept[0].ftsContentRowid})
			default:
				names := make([]string, len(kept))
				for i, k := range kept {
					names[i] = k.name
				}
				return nil, fmt.Errorf("%s has multiple FTS5 indexes (%s): scope one directly with -t", ti.name, strings.Join(names, ", "))
			}
		}
	}
	return dedupFTSTargets(raw), nil
}

// ftsSiblingsOf returns, in catalog order, the non-shadow, non-contentless
// FTS5 tables whose content= option names source (matched
// case-insensitively per SQLite's ASCII identifier fold).
func ftsSiblingsOf(cat []tableInfo, source string) []tableInfo {
	var siblings []tableInfo
	for _, ti := range cat {
		if ti.kind != "fts5" || ti.shadow || ti.contentless {
			continue
		}
		if asciiEqualFold(ti.ftsContent, source) {
			siblings = append(siblings, ti)
		}
	}
	return siblings
}

// unscopedFTSTargets returns every non-shadow, non-contentless FTS5
// table as a direct target, except those whose name matches a -T glob
// (excluded). Each target still carries its own content/content_rowid
// metadata straight from the catalog (so --row works for an
// external-content index reached this way too) — what never applies to
// the unscoped sweep is *mapping an ordinary table onto* a sibling
// index; every target here is an fts5 table matched directly.
func unscopedFTSTargets(cat []tableInfo, excluded func(name string) bool) []ftsTarget {
	var targets []ftsTarget
	for _, ti := range cat {
		if ti.kind != "fts5" || ti.shadow || ti.contentless || excluded(ti.name) {
			continue
		}
		targets = append(targets, ftsTarget{index: ti.name, source: ti.ftsContent, contentRowid: ti.ftsContentRowid})
	}
	return targets
}

// dedupFTSTargets collapses targets that resolved to the same index name
// — a glob like "docs*" can match both a source table and its index —
// keeping catalog order of first appearance and preferring the
// source-carrying entry so --row still knows where to fetch rows from.
func dedupFTSTargets(raw []ftsTarget) []ftsTarget {
	order := make([]string, 0, len(raw))
	byIndex := make(map[string]ftsTarget, len(raw))
	for _, tg := range raw {
		if existing, ok := byIndex[tg.index]; ok {
			if existing.source == "" && tg.source != "" {
				byIndex[tg.index] = tg
			}
			continue
		}
		order = append(order, tg.index)
		byIndex[tg.index] = tg
	}
	targets := make([]ftsTarget, len(order))
	for i, idx := range order {
		targets[i] = byIndex[idx]
	}
	return targets
}

// hasTextAffinity reports whether declType carries SQLite's TEXT column
// affinity: its declared type name contains CHAR, CLOB, or TEXT, matched
// case-insensitively per SQLite's own affinity determination
// (https://sqlite.org/datatype3.html rule 2). Used to pick which columns
// of a source table belong in a generated FTS5 index.
func hasTextAffinity(declType string) bool {
	f := asciiFold(declType)
	return strings.Contains(f, "char") || strings.Contains(f, "clob") || strings.Contains(f, "text")
}

// textAffinityCols returns ti's columns with TEXT affinity, in declared
// order.
func textAffinityCols(ti tableInfo) []colInfo {
	var out []colInfo
	for _, c := range ti.cols {
		if hasTextAffinity(c.declType) {
			out = append(out, c)
		}
	}
	return out
}

// colNamesOf returns the bare names of cols, in order.
func colNamesOf(cols []colInfo) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.name
	}
	return out
}

// unquoteIdent reverses quoteIdent: strips the surrounding double quotes
// and un-doubles any embedded "". Used to recover the bare identifier
// text (e.g. for a content_rowid='...' string argument) from a
// tableInfo.idExpr, which is always pre-quoted for use directly in a SQL
// expression.
func unquoteIdent(q string) string {
	s := strings.TrimPrefix(q, `"`)
	s = strings.TrimSuffix(s, `"`)
	return strings.ReplaceAll(s, `""`, `"`)
}

// sqlStringLit renders s as a single-quoted SQL string literal, doubling
// any embedded single quotes.
func sqlStringLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// externalContentFTSSetupSQL builds the complete, runnable setup SQL for a
// rowid-accessible table with no FTS5 index: an external-content fts5
// table over textCols, the ('rebuild') insert that populates it from the
// existing rows, and AI/AD/AU triggers that keep it in sync going
// forward. idcol (content_rowid, and the rowid reference in every
// trigger) is ti's own identity expression — the unshadowed rowid alias
// or INTEGER PRIMARY KEY column name. Callers must ensure textCols is
// non-empty.
func externalContentFTSSetupSQL(ti tableInfo, textCols []colInfo) string {
	idcolRaw := unquoteIdent(ti.idExpr)
	idQ := quoteIdent(idcolRaw)

	fts5ColArgs := make([]string, len(textCols))
	colList := make([]string, len(textCols))
	newVals := make([]string, len(textCols))
	oldVals := make([]string, len(textCols))
	for i, c := range textCols {
		q := quoteIdent(c.name)
		fts5ColArgs[i] = q
		colList[i] = q
		newVals[i] = "new." + q
		oldVals[i] = "old." + q
	}

	ftsQ := quoteIdent(ti.name + "_fts")
	tblQ := quoteIdent(ti.name)
	colListStr := strings.Join(colList, ", ")

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE VIRTUAL TABLE %s USING fts5(%s, content=%s, content_rowid=%s);\n\n",
		ftsQ, strings.Join(fts5ColArgs, ", "), sqlStringLit(ti.name), sqlStringLit(idcolRaw))
	fmt.Fprintf(&b, "INSERT INTO %s(%s) VALUES('rebuild');\n\n", ftsQ, ftsQ)
	fmt.Fprintf(&b, "CREATE TRIGGER %s AFTER INSERT ON %s BEGIN\n"+
		"  INSERT INTO %s(rowid, %s) VALUES (new.%s, %s); END;\n\n",
		quoteIdent(ti.name+"_fts_ai"), tblQ, ftsQ, colListStr, idQ, strings.Join(newVals, ", "))
	fmt.Fprintf(&b, "CREATE TRIGGER %s AFTER DELETE ON %s BEGIN\n"+
		"  INSERT INTO %s(%s, rowid, %s) VALUES('delete', old.%s, %s); END;\n\n",
		quoteIdent(ti.name+"_fts_ad"), tblQ, ftsQ, ftsQ, colListStr, idQ, strings.Join(oldVals, ", "))
	fmt.Fprintf(&b, "CREATE TRIGGER %s AFTER UPDATE ON %s BEGIN\n"+
		"  INSERT INTO %s(%s, rowid, %s) VALUES('delete', old.%s, %s);\n"+
		"  INSERT INTO %s(rowid, %s) VALUES (new.%s, %s); END;\n\n",
		quoteIdent(ti.name+"_fts_au"), tblQ,
		ftsQ, ftsQ, colListStr, idQ, strings.Join(oldVals, ", "),
		ftsQ, colListStr, idQ, strings.Join(newVals, ", "))

	return b.String()
}

// standaloneTextCols returns ti's TEXT-affinity columns that aren't
// already part of its primary key — the columns standaloneFTSSetupSQL
// puts in the full-text index itself, as opposed to carrying UNINDEXED
// for identification only.
func standaloneTextCols(ti tableInfo) []colInfo {
	pkFold := make(map[string]bool, len(ti.pkCols))
	for _, c := range ti.pkCols {
		pkFold[asciiFold(c)] = true
	}
	var out []colInfo
	for _, c := range textAffinityCols(ti) {
		if !pkFold[asciiFold(c.name)] {
			out = append(out, c)
		}
	}
	return out
}

// standaloneFTSSetupSQL builds the diagnostic setup SQL for a WITHOUT
// ROWID or rowid-inaccessible table with no FTS5 index. Such a table has
// no rowid to key external content on, so the generated fts5 table stores
// its own text: ti's declared PK columns are carried as UNINDEXED columns
// (present so a hit can be matched back to its row, excluded from the
// full-text index itself) alongside textCols, and the AI/AD/AU triggers
// key inserts/deletes/updates by PK equality instead of rowid. Callers
// must ensure textCols (standaloneTextCols(ti)) is non-empty.
func standaloneFTSSetupSQL(ti tableInfo, textCols []colInfo) string {
	colDefs := make([]string, 0, len(ti.pkCols)+len(textCols))
	for _, c := range ti.pkCols {
		colDefs = append(colDefs, quoteIdent(c)+" UNINDEXED")
	}
	for _, c := range textCols {
		colDefs = append(colDefs, quoteIdent(c.name))
	}

	allCols := append(append([]string{}, ti.pkCols...), colNamesOf(textCols)...)
	quotedAllCols := make([]string, len(allCols))
	newVals := make([]string, len(allCols))
	oldVals := make([]string, len(allCols))
	for i, c := range allCols {
		q := quoteIdent(c)
		quotedAllCols[i] = q
		newVals[i] = "new." + q
		oldVals[i] = "old." + q
	}
	colList := strings.Join(quotedAllCols, ", ")

	pkCond := make([]string, len(ti.pkCols))
	for i, c := range ti.pkCols {
		q := quoteIdent(c)
		pkCond[i] = q + " = old." + q
	}
	pkCondStr := strings.Join(pkCond, " AND ")

	ftsQ := quoteIdent(ti.name + "_fts")
	tblQ := quoteIdent(ti.name)

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE VIRTUAL TABLE %s USING fts5(%s);\n\n", ftsQ, strings.Join(colDefs, ", "))
	fmt.Fprintf(&b, "INSERT INTO %s(%s) SELECT %s FROM %s;\n\n", ftsQ, colList, colList, tblQ)
	fmt.Fprintf(&b, "CREATE TRIGGER %s AFTER INSERT ON %s BEGIN\n"+
		"  INSERT INTO %s(%s) VALUES (%s); END;\n\n",
		quoteIdent(ti.name+"_fts_ai"), tblQ, ftsQ, colList, strings.Join(newVals, ", "))
	fmt.Fprintf(&b, "CREATE TRIGGER %s AFTER DELETE ON %s BEGIN\n"+
		"  DELETE FROM %s WHERE %s; END;\n\n",
		quoteIdent(ti.name+"_fts_ad"), tblQ, ftsQ, pkCondStr)
	fmt.Fprintf(&b, "CREATE TRIGGER %s AFTER UPDATE ON %s BEGIN\n"+
		"  DELETE FROM %s WHERE %s;\n"+
		"  INSERT INTO %s(%s) VALUES (%s); END;\n\n",
		quoteIdent(ti.name+"_fts_au"), tblQ,
		ftsQ, pkCondStr,
		ftsQ, colList, strings.Join(newVals, ", "))

	return b.String()
}

// searchFTSTarget runs one FTS5 query against tgt.index:
//
//	SELECT rowid, snippet("<idx>", -1, char(1), char(2), '…', 16), rank
//	FROM "<idx>" WHERE "<idx>" MATCH ? ORDER BY rank, rowid
//
// rank is SQLite's bm25-derived rank (more negative is a better match); the
// rowid tie-break after it makes result order deterministic across runs.
// maxCount, when positive, caps the result count with LIMIT. When wantRow
// is set, each match's full row is fetched from tgt.source keyed by
// tgt.contentRowid (or from the index itself, keyed by its own rowid,
// when source == "" — a standalone or internal-content fts5 table with no
// external content). The fts5 query's own "rowid" column is, by FTS5's
// design, kept in sync with the content_rowid value in the source table,
// so m.rowid is exactly the key fetchRowByRowid needs.
func searchFTSTarget(db *database, tgt ftsTarget, query string, maxCount int, wantRow bool) ([]ftsMatch, error) {
	idxQ := quoteIdent(tgt.index)
	q := "SELECT rowid, snippet(" + idxQ + ", -1, char(1), char(2), '…', 16), rank FROM " + idxQ +
		" WHERE " + idxQ + " MATCH ? ORDER BY rank, rowid"
	args := []any{query}
	if maxCount > 0 {
		q += " LIMIT ?"
		args = append(args, maxCount)
	}

	rows, err := db.sql.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tgt.index, err)
	}
	defer func() { _ = rows.Close() }()

	rowSource := tgt.source
	rowKey := tgt.contentRowid
	if rowSource == "" {
		rowSource = tgt.index
	}
	if rowKey == "" {
		rowKey = "rowid"
	}

	var matches []ftsMatch
	for rows.Next() {
		m := ftsMatch{table: tgt.index}
		if err := rows.Scan(&m.rowid, &m.snippet, &m.rank); err != nil {
			return nil, fmt.Errorf("%s: %w", tgt.index, err)
		}
		if wantRow {
			row, err := fetchRowByRowid(db, rowSource, rowKey, m.rowid)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", tgt.index, err)
			}
			m.row = row
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", tgt.index, err)
	}
	return matches, nil
}

// fetchRowByRowid fetches every column of table's row whose rowKey column
// equals rowid. Used for --row on an FTS match: table is either the
// external-content source table (rowKey its content_rowid column, which
// may name an ordinary INTEGER PRIMARY KEY rather than literal "rowid")
// or, for a standalone/internal-content fts5 index, the index itself
// (rowKey "rowid").
func fetchRowByRowid(db *database, table, rowKey string, rowid int64) (map[string]any, error) {
	rows, err := db.sql.Query("SELECT * FROM "+quoteIdent(table)+" WHERE "+quoteIdent(rowKey)+" = ?", rowid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s: row %d not found", table, rowid)
	}
	raws := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range raws {
		dest[i] = &raws[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}
	row := make(map[string]any, len(cols))
	for i, c := range cols {
		row[c] = raws[i]
	}
	return row, nil
}

// cmdSearchFTS implements --fts: resolve targets (resolveFTS), query each
// in catalog order (searchFTSTarget), and print per the FTS output
// contract — mirroring cmdSearch's -l/--count/-m/--stats/--row/--json
// behavior, but sequential rather than concurrent, since an invocation
// scopes at most a handful of FTS5 indexes. Every stdout write is checked;
// the first failure is treated like a query error (stderr, exit 2).
func cmdSearchFTS(db *database, inv *invocation, stdout, stderr io.Writer) int {
	o := inv.opts

	// Glob syntax (-t, -T) is checked before catalog() runs, same as
	// cmdSearch: catalog() issues identity-verification queries per
	// table, work a malformed glob — a pure usage error — should never
	// pay for.
	if err := validateGlobs(o); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	cat, err := db.catalog()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	targets, err := resolveFTS(cat, o)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	start := time.Now()
	p := &printer{
		w:          stdout,
		jsonOut:    o.jsonOut,
		color:      searchColorEnabled(stdout, o.jsonOut),
		maxColumns: o.maxColumns,
	}

	totalMatches, matchedTables := 0, 0
	for _, tgt := range targets {
		matches, err := searchFTSTarget(db, tgt, o.fts, o.maxCount, o.row)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		if len(matches) == 0 {
			continue
		}
		matchedTables++
		totalMatches += len(matches)

		switch {
		case o.listTables:
			if o.jsonOut {
				if err := json.NewEncoder(stdout).Encode(map[string]any{"table": tgt.index}); err != nil {
					fmt.Fprintln(stderr, err)
					return exitError
				}
			} else if _, err := fmt.Fprintln(stdout, tgt.index); err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
		case o.count:
			if o.jsonOut {
				if err := json.NewEncoder(stdout).Encode(map[string]any{"table": tgt.index, "count": len(matches)}); err != nil {
					fmt.Fprintln(stderr, err)
					return exitError
				}
			} else if _, err := fmt.Fprintf(stdout, "%s:%d\n", tgt.index, len(matches)); err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
		default:
			for _, m := range matches {
				if err := p.printFTSMatch(m); err != nil {
					fmt.Fprintln(stderr, err)
					return exitError
				}
			}
		}
	}

	if o.stats {
		elapsed := time.Since(start)
		if o.jsonOut {
			if err := json.NewEncoder(stdout).Encode(map[string]any{
				"stats": map[string]any{
					"matches":       totalMatches,
					"tables":        matchedTables,
					"tablesScanned": len(targets),
					"elapsed":       elapsed.String(),
				},
			}); err != nil {
				fmt.Fprintln(stderr, err)
				return exitError
			}
		} else if _, err := fmt.Fprintf(stdout, "%d matches in %d tables (%d tables scanned) in %s\n",
			totalMatches, matchedTables, len(targets), elapsed); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
	}

	if totalMatches == 0 {
		return exitNoMatch
	}
	return exitMatch
}
