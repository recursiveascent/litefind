package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

type tableRow struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Rows *int64 `json:"rows"`
	Cols int    `json:"cols"`
}

// stickyWriter remembers the first write failure and refuses every write
// after it. Introspection output is built from many small Fprintf calls
// whose individual return values would be noise to check; routing them
// all through one of these turns "did any of this actually reach the
// user?" into a single question the caller asks per object — the same
// contract cmdSearch enforces inline, so a truncated `tables` or
// `schema` run exits 2 instead of pretending it printed everything.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.w.Write(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

// failed reports the first write failure to stderr, if there was one.
func (s *stickyWriter) failed(stderr io.Writer) bool {
	if s.err == nil {
		return false
	}
	_, _ = fmt.Fprintf(stderr, "error writing output: %v\n", s.err)
	return true
}

func cmdTables(db *database, opts searchOpts, stdout, stderr io.Writer) int {
	// Get catalog
	tables, err := db.catalog()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error reading catalog: %v\n", err)
		return exitError
	}

	out := &stickyWriter{w: stdout}

	// Filter and format
	for _, t := range tables {
		// Skip shadow tables unless --all-tables
		if t.shadow && !opts.allTables {
			continue
		}

		// Count rows for tables/fts5, null for views and with --no-counts
		var rows *int64
		if !opts.noCounts && (t.kind == "table" || t.kind == "fts5") {
			// Query count
			count, err := countRows(db, t.name)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "error counting rows: %v\n", err)
				return exitError
			}
			rows = &count
		}

		// Format output
		if opts.jsonOut {
			// JSON output
			obj := tableRow{
				Name: t.name,
				Kind: t.kind,
				Rows: rows,
				Cols: len(t.cols),
			}
			data, err := json.Marshal(obj)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "error encoding JSON: %v\n", err)
				return exitError
			}
			_, _ = fmt.Fprintf(out, "%s\n", data)
		} else {
			// Text output
			rowStr := "-"
			if rows != nil {
				rowStr = fmt.Sprintf("%d", *rows)
			}
			_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%d\n", t.name, t.kind, rowStr, len(t.cols))
		}
		if out.failed(stderr) {
			return exitError
		}
	}

	return exitMatch
}

// countRows returns the number of rows in a table or FTS5 table.
func countRows(db *database, tableName string) (int64, error) {
	var count int64
	query := `SELECT count(*) FROM ` + quoteIdent(tableName)
	err := db.sql.QueryRow(query).Scan(&count)
	return count, err
}

// schemaColumn represents a column in JSON schema output.
type schemaColumn struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"notnull"`
	Default any    `json:"default"`
	PK      int    `json:"pk"`
}

// schemaIndex represents an index in JSON schema output. DDL carries the
// index's CREATE INDEX statement verbatim, the only place a partial
// index's WHERE predicate, a DESC key, a COLLATE clause, or an
// expression key survives: PRAGMA index_info reports column names and
// nothing else. It is empty only for indexes SQLite created itself
// (sqlite_autoindex_*), which have no statement to show.
type schemaIndex struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique"`
	Columns []string `json:"columns"`
	DDL     string   `json:"ddl,omitempty"`
}

// schemaFK represents a foreign key in JSON schema output.
type schemaFK struct {
	Table string `json:"table"`
	From  string `json:"from"`
	To    any    `json:"to"` // null if implicit PK reference
}

// schemaObject represents a table schema in JSON output.
type schemaObject struct {
	Name        string         `json:"name"`
	DDL         string         `json:"ddl"`
	Columns     []schemaColumn `json:"columns"`
	Indexes     []schemaIndex  `json:"indexes"`
	ForeignKeys []schemaFK     `json:"foreignKeys"`
}

// cmdSchema outputs schema information for tables matching glob.
func cmdSchema(db *database, glob string, jsonOut bool, stdout, stderr io.Writer) int {
	// Validate glob pattern upfront
	if glob != "" {
		if err := validateGlob(glob); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return exitError
		}
	}

	// Get catalog
	tables, err := db.catalog()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error reading catalog: %v\n", err)
		return exitError
	}

	// Filter tables by glob pattern
	var matched []tableInfo
	for _, t := range tables {
		// Skip shadow tables
		if t.shadow {
			continue
		}

		// If glob empty, include all non-shadow tables
		if glob == "" {
			matched = append(matched, t)
		} else {
			// Use path.Match for glob filtering
			if m, err := path.Match(glob, t.name); err == nil && m {
				matched = append(matched, t)
			}
		}
	}

	// Return exitNoMatch if no tables matched
	if len(matched) == 0 {
		return exitNoMatch
	}

	// Output
	out := &stickyWriter{w: stdout}
	for i, t := range matched {
		// A view whose columns couldn't be introspected (it selects from
		// a table that has since been dropped) is reported here, against
		// that view alone: its DDL is still printed, and every other
		// matched object is unaffected.
		if t.xinfoErr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: cannot read columns of view %v\n", t.xinfoErr)
		}
		if jsonOut {
			obj, err := buildSchemaObject(db, t)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "error building schema for %s: %v\n", t.name, err)
				return exitError
			}
			data, err := json.Marshal(obj)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "error encoding JSON: %v\n", err)
				return exitError
			}
			_, _ = fmt.Fprintf(out, "%s\n", data)
		} else {
			if i > 0 {
				_, _ = fmt.Fprintf(out, "\n")
			}
			if err := outputSchemaText(out, db, t, stderr); err != nil {
				return exitError
			}
		}
		if out.failed(stderr) {
			return exitError
		}
	}

	return exitMatch
}

// getDDL retrieves the CREATE TABLE/VIEW statement from sqlite_master.
func getDDL(db *database, tableName string) (string, error) {
	var sql string
	err := db.sql.QueryRow(
		`SELECT coalesce(sql, '') FROM sqlite_master WHERE name = ?`,
		tableName).Scan(&sql)
	return sql, err
}

// getIndexDDL retrieves the CREATE INDEX statement from sqlite_master.
func getIndexDDL(db *database, indexName string) (string, error) {
	var sql string
	err := db.sql.QueryRow(
		`SELECT coalesce(sql, '') FROM sqlite_master WHERE type='index' AND name = ?`,
		indexName).Scan(&sql)
	return sql, err
}

// schemaData is the DDL, indexes, and foreign keys for one table — fetched
// together by both the JSON and text schema renderers.
type schemaData struct {
	ddl     string
	indexes []schemaIndex
	fks     []schemaFK
}

// fetchSchema loads one table's DDL, indexes, and foreign keys. Both schema
// output paths need exactly this triple in this order; fetching it once
// keeps their error handling consistent.
func fetchSchema(db *database, name string) (schemaData, error) {
	ddl, err := getDDL(db, name)
	if err != nil {
		return schemaData{}, err
	}
	indexes, err := getIndexes(db, name)
	if err != nil {
		return schemaData{}, err
	}
	fks, err := getForeignKeys(db, name)
	if err != nil {
		return schemaData{}, err
	}
	return schemaData{ddl: ddl, indexes: indexes, fks: fks}, nil
}

// buildSchemaObject builds a schemaObject for JSON output.
func buildSchemaObject(db *database, t tableInfo) (schemaObject, error) {
	d, err := fetchSchema(db, t.name)
	if err != nil {
		return schemaObject{}, err
	}

	obj := schemaObject{
		Name:        t.name,
		DDL:         d.ddl,
		Columns:     []schemaColumn{},
		Indexes:     d.indexes,
		ForeignKeys: d.fks,
	}

	// Add columns
	for _, c := range t.cols {
		var defVal any
		if c.dflt.Valid {
			defVal = c.dflt.String
		}
		obj.Columns = append(obj.Columns, schemaColumn{
			Name:    c.name,
			Type:    c.declType,
			NotNull: c.notNull,
			Default: defVal,
			PK:      c.pk,
		})
	}

	return obj, nil
}

// getIndexes retrieves indexes for a table.
func getIndexes(db *database, tableName string) ([]schemaIndex, error) {
	rows, err := db.sql.Query(
		`PRAGMA index_list(` + quoteIdent(tableName) + `)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var indexes []schemaIndex
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return nil, err
		}

		// Skip auto-created indexes (origin='pk' for PRIMARY KEY)
		// Include user-created indexes (origin='c') and constraint-based ones (origin='u')
		if origin == "c" || origin == "u" {
			// Get columns for this index
			cols, err := getIndexColumns(db, name)
			if err != nil {
				return nil, err
			}
			// The statement is the whole truth about an index; the column
			// list is a lossy summary of it. Attach it whenever SQLite
			// has one to give — an index it created for a UNIQUE
			// constraint has sql NULL, which coalesces to empty here.
			ddl, err := getIndexDDL(db, name)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			indexes = append(indexes, schemaIndex{
				Name:    name,
				Unique:  unique == 1,
				Columns: cols,
				DDL:     ddl,
			})
		}
	}

	return indexes, rows.Err()
}

// getIndexColumns retrieves the indexed column names for an index. A key
// that is an expression rather than a column has no name (PRAGMA
// index_info reports NULL) and stands in as "<expr>"; the index's DDL
// carries what it actually indexes.
func getIndexColumns(db *database, indexName string) ([]string, error) {
	rows, err := db.sql.Query(
		`PRAGMA index_info(` + quoteIdent(indexName) + `)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var cols []string
	for rows.Next() {
		var seqno, cid int
		var name sql.NullString
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return nil, err
		}
		if name.Valid {
			cols = append(cols, name.String)
		} else {
			cols = append(cols, "<expr>")
		}
	}

	return cols, rows.Err()
}

// getForeignKeys retrieves foreign keys for a table.
func getForeignKeys(db *database, tableName string) ([]schemaFK, error) {
	rows, err := db.sql.Query(
		`PRAGMA foreign_key_list(` + quoteIdent(tableName) + `)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var fks []schemaFK
	for rows.Next() {
		var id, seq int
		var table, from string
		var to sql.NullString
		var onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return nil, err
		}
		// Handle implicit PK reference (to is NULL)
		var toVal any
		if to.Valid {
			toVal = to.String
		}
		fks = append(fks, schemaFK{
			Table: table,
			From:  from,
			To:    toVal,
		})
	}

	return fks, rows.Err()
}

// outputSchemaText outputs schema in text format.
func outputSchemaText(stdout io.Writer, db *database, t tableInfo, stderr io.Writer) error {
	d, err := fetchSchema(db, t.name)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error getting schema for %s: %v\n", t.name, err)
		return err
	}

	// Output DDL
	if d.ddl != "" {
		_, _ = fmt.Fprintf(stdout, "%s\n", d.ddl)
	}

	// Output columns
	for _, c := range t.cols {
		colStr := fmt.Sprintf("  %s %s", c.name, c.declType)
		if c.notNull {
			colStr += " NOT NULL"
		}
		if c.dflt.Valid {
			colStr += fmt.Sprintf(" DEFAULT %s", c.dflt.String)
		}
		if c.pk > 0 {
			colStr += " PK"
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", colStr)
	}

	// Output indexes
	if len(d.indexes) > 0 {
		_, _ = fmt.Fprintf(stdout, "\nINDEXES:\n")
		for _, idx := range d.indexes {
			unique := ""
			if idx.Unique {
				unique = " UNIQUE"
			}
			_, _ = fmt.Fprintf(stdout, "  %s%s: %s\n", idx.Name, unique, strings.Join(idx.Columns, ", "))
			// The statement, where SQLite has one, is what carries a
			// partial index's predicate, key direction, and collation.
			if idx.DDL != "" {
				_, _ = fmt.Fprintf(stdout, "    DDL: %s\n", idx.DDL)
			}
		}
	}

	// Output foreign keys
	if len(d.fks) > 0 {
		_, _ = fmt.Fprintf(stdout, "\nFOREIGN KEYS:\n")
		for _, fk := range d.fks {
			toStr := "(pk)"
			if fk.To != nil {
				toStr = fmt.Sprintf("(%v)", fk.To)
			}
			_, _ = fmt.Fprintf(stdout, "  %s -> %s%s\n", fk.From, fk.Table, toStr)
		}
	}

	return nil
}
