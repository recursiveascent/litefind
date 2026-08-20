package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// buildMatcher compiles the pattern per flags: -F quotes it literally,
// -w wraps in \b(?:...)\b, -i (or -S with an all-lowercase pattern)
// prepends (?i).
func buildMatcher(pattern string, o searchOpts) (*regexp.Regexp, error) {
	p := pattern
	if o.fixed {
		p = regexp.QuoteMeta(p)
	}
	if o.wordRegexp {
		p = `\b(?:` + p + `)\b`
	}
	ci := o.ignoreCase || (o.smartCase && strings.ToLower(pattern) == pattern)
	if ci {
		p = `(?i)` + p
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("invalid regex %q: %v", pattern, err)
	}
	return re, nil
}

// match is one (row, column) hit: a searchable column whose SQLite text
// rendition contained at least one match for the pattern.
type match struct {
	table, column string
	rowid         int64
	pk            []pkVal // non-nil => pk identity (replaces rowid)
	value         string  // SQLite text rendition that was matched
	spans         [][2]int
	row           map[string]any // populated when --row
}

// pkVal is one column of a composite (or singleton) primary-key identity,
// in declared PK order.
type pkVal struct {
	col string
	val any
}

// maxResultColumns is SQLite's default SQLITE_MAX_COLUMN, which also caps
// how many expressions one result set may carry. Scan queries are batched
// to sit exactly at this ceiling rather than under an invented margin, so
// a table's own column or composite-PK width is never itself the reason it
// can't be searched: any table SQLite will accept, this scanner can query.
// See matchBatchSize for the sole remaining unaddressable case (an
// identity so wide that not even one typeof/CAST pair fits beside it).
const maxResultColumns = 2000

// colBatch is one contiguous slice of a table's columns, scanned by a
// single query.
type colBatch []colInfo

// batchColumns splits cols into contiguous batches of at most size
// columns each, preserving column order.
func batchColumns(cols []colInfo, size int) []colBatch {
	var batches []colBatch
	for i := 0; i < len(cols); i += size {
		end := i + size
		if end > len(cols) {
			end = len(cols)
		}
		batches = append(batches, colBatch(cols[i:end]))
	}
	return batches
}

// matchBatchSize returns how many data columns one match-scan batch may
// cover, given identCount identity expressions repeated in every batch
// query alongside two result expressions (typeof, CAST) per data column.
// The capacity is exact — every result column SQLite allows is used — so
// a wide composite primary key eats directly into the room left for data
// columns, down to a single column per query. Only an identity so wide
// that it can't even be joined by one typeof/CAST pair is unaddressable:
// that's reported rather than silently scanning zero columns per batch
// (which would loop forever).
func matchBatchSize(identCount int) (int, error) {
	room := maxResultColumns - identCount
	if room < 2 {
		return 0, fmt.Errorf("primary key is too wide to scan (%d identity columns)", identCount)
	}
	return room / 2, nil
}

// matchBatchQuery builds one batch's typeof/CAST scan query: identity
// columns first, then typeof(c), CAST(c AS TEXT) per column in the batch.
func matchBatchQuery(table string, identExprs []string, batch colBatch) string {
	parts := append([]string{}, identExprs...)
	for _, c := range batch {
		q := quoteIdent(c.name)
		parts = append(parts, "typeof("+q+")", "CAST("+q+" AS TEXT)")
	}
	return "SELECT " + strings.Join(parts, ", ") +
		" FROM " + quoteIdent(table) +
		" ORDER BY " + strings.Join(identExprs, ", ")
}

// rowBatchQuery builds one batch's raw-value query for --row output: the
// columns themselves, unconverted. Identity isn't selected here — the
// shared ORDER BY keeps it aligned, row for row, with the match batch
// cursors advanced in lockstep alongside it.
func rowBatchQuery(table string, identExprs []string, batch colBatch) string {
	parts := make([]string, len(batch))
	for i, c := range batch {
		parts[i] = quoteIdent(c.name)
	}
	return "SELECT " + strings.Join(parts, ", ") +
		" FROM " + quoteIdent(table) +
		" ORDER BY " + strings.Join(identExprs, ", ")
}

// advanceAll calls Next on every cursor in lockstep — they share one FROM
// table and ORDER BY, evaluated inside one transaction, so they must all
// report the same row count in the same order; a mismatch means the table
// changed under the scan.
func advanceAll(table string, cursorGroups ...[]*sql.Rows) (bool, error) {
	first := true
	hasNext := false
	for _, group := range cursorGroups {
		for _, c := range group {
			ok := c.Next()
			if first {
				hasNext, first = ok, false
				continue
			}
			if ok != hasNext {
				return false, fmt.Errorf("%s: scan batches diverged mid-table (concurrent write?)", table)
			}
		}
	}
	return hasNext, nil
}

// errNoStableIdentity marks a table abandoned because an identity value
// is NULL: the same condition catalog()'s verification rejects up front,
// caught again inside the scan transaction where a concurrent writer can
// no longer sneak past it. It is not a failure of the search — the caller
// reports it through the identical "no stable row identity" warning that
// skips the table.
var errNoStableIdentity = errors.New("no stable row identity")

// scanTable streams one table's matches, in identity order, to emit.
// Query shape (spec "Search semantics"): for idAlias/idIntegerPK,
//
//	SELECT <id>, typeof(c1), CAST(c1 AS TEXT), ... FROM "t" ORDER BY <id>
//
// for idPK, the PK columns replace <id> and ORDER BY lists all of them.
// typeof gates matching: 'null' and 'blob' columns never match. Wide
// tables are scanned in column batches, sized to keep each query's
// result-column count within maxResultColumns, SQLite's own result-column
// limit; batch cursors are merged in lockstep so output
// stays row-major (one row's matches, in column order, before the next
// row's) and identity-ordered, exactly as a single unbounded query would
// produce. When wantRow is set, the attached row always carries every
// searchable column — not just the ones cols scopes matching to — via
// its own, separately batched raw-value queries merged the same way.
//
// For an idPK table the declared key's non-NULLness is re-verified inside
// the scan transaction before anything is emitted, so a table whose
// identity a concurrent writer has broken yields no output at all rather
// than a prefix of one (errNoStableIdentity).
//
// Matches go to emit as they are found rather than accumulating in a
// slice, so a table's whole result set never has to fit in memory; emit
// may block, which is how the caller bounds how far ahead of the output
// a scan may run.
func scanTable(db *database, ti tableInfo, re *regexp.Regexp, cols []string, maxCount int, wantRow bool, emit func(match)) error {
	if ti.ident == idNone {
		return nil
	}
	scanCols := filterColumns(ti.cols, cols)
	if len(scanCols) == 0 {
		return nil
	}

	usePK := ti.ident == idPK
	var identExprs []string
	if usePK {
		for _, c := range ti.pkCols {
			identExprs = append(identExprs, quoteIdent(c))
		}
	} else {
		identExprs = []string{ti.idExpr}
	}

	// Every match-batch query repeats the identity expressions, so a wide
	// composite PK must shrink the data-column batch size to keep total
	// result columns within SQLite's limit; matchBatchSize errors out
	// rather than let a PK too wide for even one data column silently
	// loop forever.
	matchSize, err := matchBatchSize(len(identExprs))
	if err != nil {
		return fmt.Errorf("%s: %w", ti.name, err)
	}

	// A transaction gives every batch query the same consistent snapshot,
	// so their shared ORDER BY produces identical row sequences to merge
	// against — required for the lockstep advance below to be sound.
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Verify the declared PK inside the scan's own snapshot, before a
	// single match is emitted. catalog()'s identical check ran before this
	// transaction opened, leaving a window for a concurrent writer to add a
	// NULL-keyed row; re-running it here closes that window for good,
	// because this query is the transaction's first read and so fixes the
	// very snapshot the batch cursors below will read. Per-row checking
	// alone cannot do this: a NULL-keyed row need not sort first under a
	// composite key, and -m can stop the scan before it is ever visited, so
	// the table's earlier matches would already have been emitted under an
	// identity the table cannot actually support. WITHOUT ROWID tables are
	// exempt on the same grounds catalog() exempts them — SQLite enforces
	// NOT NULL on their key columns.
	if usePK && !ti.withoutRowid {
		ok, err := pkAllNonNull(tx, ti.name, ti.pkCols)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s: %w", ti.name, errNoStableIdentity)
		}
	}

	matchBatches := batchColumns(scanCols, matchSize)
	matchCursors := make([]*sql.Rows, 0, len(matchBatches))
	defer func() {
		for _, c := range matchCursors {
			_ = c.Close()
		}
	}()
	for _, batch := range matchBatches {
		rows, err := tx.Query(matchBatchQuery(ti.name, identExprs, batch))
		if err != nil {
			return err
		}
		matchCursors = append(matchCursors, rows)
	}

	var rowBatches []colBatch
	var rowCursors []*sql.Rows
	if wantRow {
		// Row-fetch queries carry no identity expressions (see
		// rowBatchQuery), so every result column is available for data.
		rowBatches = batchColumns(ti.cols, maxResultColumns)
		defer func() {
			for _, c := range rowCursors {
				_ = c.Close()
			}
		}()
		for _, batch := range rowBatches {
			rows, err := tx.Query(rowBatchQuery(ti.name, identExprs, batch))
			if err != nil {
				return err
			}
			rowCursors = append(rowCursors, rows)
		}
	}

	emitted := 0
	for {
		hasNext, err := advanceAll(ti.name, matchCursors, rowCursors)
		if err != nil {
			return err
		}
		if !hasNext {
			break
		}

		var rowMap map[string]any
		if wantRow {
			rowMap = make(map[string]any, len(ti.cols))
			for bi, batch := range rowBatches {
				raws := make([]any, len(batch))
				dest := make([]any, len(batch))
				for i := range batch {
					dest[i] = &raws[i]
				}
				if err := rowCursors[bi].Scan(dest...); err != nil {
					return err
				}
				for i, c := range batch {
					rowMap[c.name] = raws[i]
				}
			}
		}

		var rowidVal int64
		pkVals := make([]any, len(identExprs))
		var pk []pkVal
		done := false
		for bi, batch := range matchBatches {
			dest := make([]any, len(identExprs)+2*len(batch))
			if bi == 0 {
				if usePK {
					for i := range identExprs {
						dest[i] = &pkVals[i]
					}
				} else {
					dest[0] = &rowidVal
				}
			} else {
				// Identity is re-selected in every batch (query shape
				// mirrors the single-query form per column group) but
				// only batch 0's values are kept; later batches decode
				// into throwaway destinations.
				discard := make([]any, len(identExprs))
				for i := range identExprs {
					dest[i] = &discard[i]
				}
			}

			typeofs := make([]string, len(batch))
			casts := make([]sql.NullString, len(batch))
			base := len(identExprs)
			for j := range batch {
				off := base + j*2
				dest[off] = &typeofs[j]
				dest[off+1] = &casts[j]
			}

			if err := matchCursors[bi].Scan(dest...); err != nil {
				return err
			}
			if bi == 0 && usePK {
				pk = make([]pkVal, len(identExprs))
				for i, c := range ti.pkCols {
					// Backstop for the in-transaction check above, which
					// has already ruled this out for the snapshot being
					// read: a NULL here would mean the row cannot be
					// addressed by its declared PK, so nothing in the
					// table can be reported with a stable identity, and
					// abandoning it (the caller warns and skips) is the
					// only correct answer — emitting the row would attach
					// it to a pk=() that addresses nothing. database/sql
					// decodes SQL NULL into a nil interface value, so this
					// is the same test a sql.Null* destination's Valid
					// flag would make.
					if pkVals[i] == nil {
						return fmt.Errorf("%s: %w", ti.name, errNoStableIdentity)
					}
					pk[i] = pkVal{col: c, val: pkVals[i]}
				}
			}

			for j, c := range batch {
				t := typeofs[j]
				if t == "null" || t == "blob" {
					continue
				}
				if !casts[j].Valid {
					continue
				}
				text := casts[j].String
				idxs := re.FindAllStringIndex(text, -1)
				if len(idxs) == 0 {
					continue
				}
				spans := make([][2]int, len(idxs))
				for k, s := range idxs {
					spans[k] = [2]int{s[0], s[1]}
				}
				m := match{
					table:  ti.name,
					column: c.name,
					value:  text,
					spans:  spans,
					row:    rowMap,
				}
				if usePK {
					m.pk = pk
				} else {
					m.rowid = rowidVal
				}
				emit(m)
				emitted++
				if maxCount > 0 && emitted >= maxCount {
					done = true
					break
				}
			}
			if done {
				break
			}
		}
		if done {
			break
		}
	}

	for _, c := range matchCursors {
		if err := c.Err(); err != nil {
			return err
		}
	}
	for _, c := range rowCursors {
		if err := c.Err(); err != nil {
			return err
		}
	}

	return nil
}

// tableResult is one scoped table's final outcome, delivered over its
// stream's done channel once every match it produced has been sent:
// either a stable-identity warning (the table was skipped), a scan
// error, or neither. scanned reports whether the worker actually
// invoked scanTable — false for a table warned off at catalog time
// (idNone) and for one a -c selector resolved to zero columns — so
// --stats' "tables scanned" count reflects queries actually issued, not
// every table merely in scope.
type tableResult struct {
	err     error
	warned  string // non-empty => this table was skipped for lack of identity
	scanned bool
}

// tableStream carries one scoped table's matches to the emitter as they
// are scanned. matches is closed when the table's scan ends, after which
// done holds exactly one outcome. name is everything the emitter needs to
// know about the table itself, so the streams it is handed, and not a
// shared slice indexed in parallel, are the whole of its input.
//
// A stream is created when its table takes a scan slot and becomes
// garbage once the emitter has drained it, so the buffers alive at any
// moment number at most the pool size — a table waiting its turn costs
// nothing.
type tableStream struct {
	name    string
	matches chan match
	done    chan tableResult
}

// tableMatchBuffer is how many matches one table's scan may run ahead of
// the emitter. It exists to keep a worker busy across the emitter's
// per-match formatting and write, not to hold results: a scan that
// outruns output by more than this blocks, so concurrent scans of huge
// tables cost a bounded number of buffered matches each instead of
// whole result sets.
const tableMatchBuffer = 256

// startScans scans every table in scoped, at most cap(slots) at a time,
// and delivers one stream per table on the returned channel in catalog
// order. The reader must drain each stream's matches, read its done
// value, and then take one value out of slots: that release is what
// admits the next table, so buffering across the whole search stays
// bounded by the pool (see cmdSearch, the only caller that isn't a test).
//
// The order of the two steps inside the loop is load-bearing in both
// directions. A table's scan is started *before* its stream is handed
// over, because the reader is normally still draining an earlier table
// and not yet ready to receive: making a scan wait for the hand-off would
// serialize every scan behind the reader, leaving one table in flight no
// matter how large the pool. And streams is buffered to the pool size so
// the hand-off itself never blocks — a stream sitting in that buffer is
// holding a slot, so the buffer can never hold more than cap(slots)
// streams, and the memory bound is untouched.
//
// This goroutine is the sole sender on streams and works through scoped
// in order, so the reader's input is catalog-ordered by construction; it
// needs no index and no reordering.
func startScans(scoped []tableInfo, slots chan struct{}, scan func(tableInfo, func(match)) tableResult) <-chan tableStream {
	streams := make(chan tableStream, cap(slots))
	go func() {
		defer close(streams)
		for _, ti := range scoped {
			slots <- struct{}{}
			st := tableStream{
				name:    ti.name,
				matches: make(chan match, tableMatchBuffer),
				done:    make(chan tableResult, 1),
			}
			go func() {
				res := scan(ti, func(m match) { st.matches <- m })
				close(st.matches)
				st.done <- res
			}()
			streams <- st
		}
	}()
	return streams
}

// scopeTables applies the search subcommand's table scope (spec "Search
// semantics"): kind "table" non-shadow plus fts5 non-shadow by default;
// --all-tables widens that to every non-view kind, shadow included. -t
// keeps a table matching any include glob; -T then drops any table
// matching an exclude glob. Both use path.Match against the bare table
// name. Glob syntax (-t, -T, and -c) is validated by the caller
// (validateGlobs), before db.catalog() ever runs — not here.
func scopeTables(cat []tableInfo, o searchOpts) []tableInfo {
	var base []tableInfo
	for _, ti := range cat {
		switch {
		case ti.kind == "view":
			continue
		case o.allTables:
			base = append(base, ti)
		case ti.shadow:
			continue
		case ti.kind == "table" || ti.kind == "fts5":
			base = append(base, ti)
		}
	}

	if len(o.tables) > 0 {
		var kept []tableInfo
		for _, ti := range base {
			if matchesAnyGlob(o.tables, ti.name) {
				kept = append(kept, ti)
			}
		}
		base = kept
	}
	if len(o.notTables) > 0 {
		var kept []tableInfo
		for _, ti := range base {
			if !matchesAnyGlob(o.notTables, ti.name) {
				kept = append(kept, ti)
			}
		}
		base = kept
	}
	return base
}

// matchesAnyGlob reports whether name matches any of globs via path.Match.
// Glob syntax is assumed already validated (validateGlobs), so a match
// error here can only mean the pattern doesn't match — never a malformed
// pattern — and is safely ignored.
func matchesAnyGlob(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

// validateGlob reports whether g is a syntactically valid path.Match glob.
func validateGlob(g string) error {
	if _, err := path.Match(g, ""); err != nil {
		return fmt.Errorf("invalid glob pattern %q: %v", g, err)
	}
	return nil
}

// validateGlobs checks every -t, -T, and -c glob (the COLGLOB half of a
// -c value, not its optional TABLE qualifier) for syntax errors, via the
// same path.Match probe the schema subcommand uses.
func validateGlobs(o searchOpts) error {
	for _, g := range o.tables {
		if err := validateGlob(g); err != nil {
			return err
		}
	}
	for _, g := range o.notTables {
		if err := validateGlob(g); err != nil {
			return err
		}
	}
	for _, spec := range o.columns {
		_, colGlob := splitColumnSpec(spec)
		if err := validateGlob(colGlob); err != nil {
			return err
		}
	}
	return nil
}

// splitColumnSpec splits a -c value into its optional TABLE qualifier and
// column glob: "events.msg*" -> ("events", "msg*"); a bare "msg*" ->
// ("", "msg*").
func splitColumnSpec(spec string) (table, colGlob string) {
	if i := strings.IndexByte(spec, '.'); i >= 0 {
		return spec[:i], spec[i+1:]
	}
	return "", spec
}

// resolveColsForTable expands o.columns into the concrete searchable
// column names of ti that scanTable should match against. A bare COLGLOB
// applies to every table; a TABLE.COLGLOB applies only when TABLE names ti
// (compared with SQLite's own ASCII-only fold). Once any -c value is
// supplied, column selection becomes opt-in: a table no -c value reaches
// resolves to an empty list, which the caller treats as "search nothing"
// rather than scanTable's own nil-means-unrestricted convention.
func resolveColsForTable(o searchOpts, ti tableInfo) []string {
	if len(o.columns) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, spec := range o.columns {
		table, colGlob := splitColumnSpec(spec)
		if table != "" && !asciiEqualFold(table, ti.name) {
			continue
		}
		for _, c := range ti.cols {
			if seen[c.name] {
				continue
			}
			if ok, _ := path.Match(colGlob, c.name); ok {
				seen[c.name] = true
				out = append(out, c.name)
			}
		}
	}
	return out
}

// searchColorEnabled reports whether match highlighting should be on:
// text mode (never JSON) writing to a terminal.
func searchColorEnabled(stdout io.Writer, jsonOut bool) bool {
	if jsonOut {
		return false
	}
	f, ok := stdout.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// cmdSearch implements the search subcommand: scope resolution, at most
// GOMAXPROCS tables scanned concurrently, and deterministic emission
// (catalog order, not completion order) in one of four modes. See
// scopeTables and resolveColsForTable for scoping; tableResult.warned
// carries a skipped table's notice through the same per-table stream as
// its scan outcome, so the emitter loop is the single place that both
// prints warnings and prints matches, still in catalog order. Every write
// to stdout is checked: the first one that fails (e.g. a broken pipe) is
// treated exactly like a scan error — reported to stderr, exit 2 — rather
// than silently truncating output while still exiting 0 or 1.
//
// Scans stream their matches into bounded per-table channels instead of
// returning finished slices, so peak memory is a function of how far
// scans may run ahead of output, never of how much a table matches. A
// table may only start against one of GOMAXPROCS scan slots (startScans);
// its stream is created as that slot is taken, and the slot is given back
// here, once the emitter has drained the stream — not when the scan
// itself ends. Bounding the streams the emitter has yet to reach, and not
// just each stream's depth, is what makes poolSize × tableMatchBuffer a
// bound for the whole search: otherwise a scan racing through small
// tables leaves an unbounded pile of finished-but-undrained streams
// behind it. Tying a stream's creation to its slot bounds the empty
// buffers too — a thousand-table database allocates poolSize of them, not
// a thousand.
//
// Slots are taken in catalog order by startScans and released in catalog
// order by the emitter below, so the tables holding one are always a
// contiguous run starting at the lowest table the emitter has not yet
// drained. That table's scan is therefore always already running, and
// draining it always makes progress — no deadlock. (Letting each scan
// take its own slot after being handed its table would forfeit this: two
// could race between hand-off and acquisition, letting a later table win
// the last slot while an earlier table waits for one and the emitter
// waits for that earlier table.)
//
// The one behavior this trades away is atomicity per table: a table that
// fails partway through has already had its earlier matches printed,
// where buffering could have suppressed them. The exit code is
// unchanged (2, with the error on stderr).
func cmdSearch(db *database, inv *invocation, stdout, stderr io.Writer) int {
	o := inv.opts

	re, err := buildMatcher(inv.pattern, o)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	// Glob syntax (-t, -T, -c) is checked before catalog() runs: catalog()
	// issues identity-verification queries per table, work a malformed
	// glob — a pure usage error — should never pay for.
	if err := validateGlobs(o); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	cat, err := db.catalog()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	scoped := scopeTables(cat, o)

	start := time.Now()

	scan := func(ti tableInfo, emit func(match)) tableResult {
		if ti.ident == idNone {
			return tableResult{warned: ti.name}
		}
		cols := resolveColsForTable(o, ti)
		if len(o.columns) > 0 && len(cols) == 0 {
			return tableResult{}
		}
		err := scanTable(db, ti, re, cols, o.maxCount, o.row, emit)
		if errors.Is(err, errNoStableIdentity) {
			// The scan found what catalog()'s check could not: an
			// identity value that is NULL now. Same skip, same warning.
			return tableResult{warned: ti.name, scanned: true}
		}
		return tableResult{err: err, scanned: true}
	}

	slots := make(chan struct{}, min(runtime.GOMAXPROCS(0), len(scoped)))
	streams := startScans(scoped, slots, scan)

	p := &printer{
		w:          stdout,
		jsonOut:    o.jsonOut,
		color:      searchColorEnabled(stdout, o.jsonOut),
		maxColumns: o.maxColumns,
	}

	var firstErr error
	totalMatches, matchedTables, tablesScanned := 0, 0, 0
	for st := range streams {
		// Every table's stream is drained to completion, even in modes
		// that only tally or in a run already doomed by an earlier error:
		// a stream left unread would block its scan forever.
		count := 0
		for m := range st.matches {
			count++
			if !o.listTables && !o.count {
				if err := p.printMatch(m); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		res := <-st.done
		// This table is fully consumed: give its stream slot back, which
		// is what lets the next table start — and lets this stream's
		// buffer be collected. Releasing here rather than when the scan
		// ended is what bounds buffering across the whole search instead
		// of merely per table.
		<-slots
		if res.scanned {
			tablesScanned++
		}
		if res.warned != "" {
			fmt.Fprintf(stderr, "warning: skipping %s: no stable row identity (rowid aliases shadowed, primary key nullable or absent)\n", res.warned)
			continue
		}
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		if count == 0 {
			continue
		}
		matchedTables++
		totalMatches += count
		switch {
		case o.listTables, o.count:
			if err := emitTableSummary(stdout, st.name, count, o.listTables, o.count, o.jsonOut); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	if firstErr != nil {
		fmt.Fprintln(stderr, firstErr)
		return exitError
	}

	if o.stats {
		if rc := emitStats(stdout, stderr, totalMatches, matchedTables, tablesScanned, time.Since(start), o.jsonOut); rc != exitMatch {
			return rc
		}
	}

	if totalMatches == 0 {
		return exitNoMatch
	}
	return exitMatch
}

// emitTableSummary prints one table's -l or --count line, text or JSON.
func emitTableSummary(w io.Writer, name string, count int, listTables, countMode, jsonOut bool) error {
	switch {
	case listTables:
		if jsonOut {
			return json.NewEncoder(w).Encode(map[string]any{"table": name})
		}
		_, err := fmt.Fprintln(w, name)
		return err
	case countMode:
		if jsonOut {
			return json.NewEncoder(w).Encode(map[string]any{"table": name, "count": count})
		}
		_, err := fmt.Fprintf(w, "%s:%d\n", name, count)
		return err
	}
	return nil
}

// emitStats prints the --stats summary line, text or JSON.
func emitStats(w, stderr io.Writer, totalMatches, matchedTables, tablesScanned int, elapsed time.Duration, jsonOut bool) int {
	if jsonOut {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"stats": map[string]any{
				"matches":       totalMatches,
				"tables":        matchedTables,
				"tablesScanned": tablesScanned,
				"elapsed":       elapsed.String(),
			},
		}); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		return exitMatch
	}
	if _, err := fmt.Fprintf(w, "%d matches in %d tables (%d tables scanned) in %s\n",
		totalMatches, matchedTables, tablesScanned, elapsed); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	return exitMatch
}

// filterColumns returns the subset of cols whose names match one of the
// wanted names, case-insensitively per SQLite's ASCII-only identifier
// fold. nil/empty wanted returns all columns.
func filterColumns(all []colInfo, wanted []string) []colInfo {
	if len(wanted) == 0 {
		return all
	}
	var out []colInfo
	for _, c := range all {
		for _, w := range wanted {
			if asciiEqualFold(c.name, w) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
