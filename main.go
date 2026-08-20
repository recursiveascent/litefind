// litefind is ripgrep for SQLite databases: read-only regex, fixed-string,
// and FTS5 search scoped by tables and columns, plus schema introspection.
package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

const (
	exitMatch   = 0
	exitNoMatch = 1
	exitError   = 2
)

//go:embed VERSION
var versionFile string

// versionOverride is set at release time via -ldflags
// "-X 'main.versionOverride=v0.1.0'", so a tagged CI artifact or Nix
// build prints exactly its tag instead of a VCS-derived pseudo-version.
// (For the root main package the -X path is main.<sym>, not the
// module path — the latter is silently ignored by the linker.)
var versionOverride string

// version resolves, in priority order:
//  1. versionOverride — release build ldflags, when set.
//  2. the module version stamped by the Go toolchain from VCS
//     (bi.Main.Version), which is the tag for a tagged checkout or a
//     pseudo-version like v0.1.1-0.<timestamp>-<sha>+dirty otherwise.
//     This covers `go install ...@v0.1.0` and dev builds alike.
//  3. the embedded VERSION file — the last released version, always
//     present; the fallback when VCS stamping is unavailable (module
//     cache, -buildvcs=false, bare containers).
func version() string {
	if versionOverride != "" {
		return versionOverride
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if mv := bi.Main.Version; mv != "" && mv != "(devel)" {
			return mv
		}
	}
	return strings.TrimSpace(versionFile)
}

const usage = `litefind — ripgrep for SQLite databases (read-only)

usage:
  litefind PATTERN <db> [flags]                 search (regex by default; -F for literal)
  litefind --tables <db> [flags]                list tables: name, kind, row count, column count
  litefind --schema <db> [table-glob] [flags]   show DDL, columns, indexes, foreign keys

Flags may appear anywhere on the command line, rg-style:
  litefind --json timeout db.sqlite  ==  litefind timeout db.sqlite --json

examples:
  litefind timeout events.db                           regex search, all tables
  litefind -F 'error: 42' events.db                    literal string match
  litefind -t events -c message timeout events.db      scope to a table + column
  litefind --fts 'NEAR(timeout retry, 3)' events.db    FTS5 query syntax
  litefind --tables events.db                          table inventory
  litefind --schema events.db 'user*'                  DDL for tables matching a glob

common flags:
  -V, --version              print version and exit
  -h, --help                 show this help

search flags:
  -t, --table GLOB           include only matching tables (repeatable)
  -T, --not-table GLOB       exclude matching tables (repeatable)
  -c, --column [TABLE.]GLOB  scope to matching columns (repeatable)
                              divergence from rg: there -c means count; here
                              -c scopes columns (the more common gesture) —
                              use --count for match counts.
  -F, --fixed-strings        PATTERN is a literal string, not a regex
  -i, --ignore-case          case-insensitive matching
  -S, --smart-case           case-insensitive unless PATTERN has an uppercase letter
  -w, --word-regexp          require word boundaries around the match
  -l, --tables-with-matches  print only the names of tables containing matches
  -m, --max-count N          stop after N matches per table
  --count                    print match counts per table instead of matches
  --row                      attach the full row (typed values) to each match
  --json                     JSONL output, one object per match
  --stats                    print a search statistics summary line
  --max-columns N            truncate displayed/snippet values to N chars
                              (default 200; 0 disables truncation)
  --all-tables               include sqlite_* and FTS5 shadow tables
                              (hidden by default)
  --fts QUERY                FTS5 match syntax (phrase "a b", NEAR(a b, N),
                              AND/OR/NOT, column filters); replaces PATTERN
                              — see fts below

introspection flags:
  --tables                   list tables
  --schema                   show schema; optional table glob follows <db>
  --json                     JSON output instead of text
  --no-counts                (--tables only) skip COUNT(*) row counts
  --all-tables               (--tables only) include shadow tables
  --immutable                (all modes) see immutable/wal below

regex engine:
  Default pattern syntax is Go's RE2 (regexp/syntax), not PCRE — near-parity
  with ripgrep's Rust regex engine (both are lookaround-free, linear-time
  finite-automata engines), so most rg habits transfer directly. Known
  divergence: Go's \b is ASCII-only, where Rust's (and rg's) \b is
  Unicode-aware. Reference: https://pkg.go.dev/regexp/syntax

fts: searching existing FTS5 indexes:
  --fts queries FTS5 indexes that already exist in the database; it never
  builds one. A scoped table participates two ways: it IS an fts5 table
  (queried directly), or it has an external-content FTS5 sibling
  (content='<table>'), in which case a search scoped to the source table
  (-t) is mapped onto the sibling index automatically. If no index covers a
  rowid-accessible scoped table, litefind exits 2 with complete, runnable
  CREATE VIRTUAL TABLE + trigger SQL to set one up — rerun once it exists.

  Exceptions to the above:
  - a directly-scoped contentless index (content='') stores no text, so it
    cannot satisfy the snippet/--row contract and is rejected outright —
    scope an external-content or regular FTS5 table instead.
  - a table with no stable row identity, or with no TEXT-affinity columns
    to index, gets an explanatory diagnostic instead of setup SQL — no
    synchronized index can be generated for it.
  - a WITHOUT ROWID table (or a rowid table with every rowid alias
    shadowed and no rowid-aliasing INTEGER PRIMARY KEY — one declared
    INTEGER PRIMARY KEY DESC does not alias the rowid) gets
    standalone-FTS setup SQL instead of external-content SQL, since
    external content requires a rowid source; litefind does not map
    -t <table> onto the result, so rerun scoping the new index
    directly, e.g. -t <table>_fts.

  --fts is a different matching regime from regex/fixed search, so these
  flags are rejected when combined with it (usage error, exit 2):
    -F  -i  -S  -w  -c  --all-tables
  Case and tokenization are governed by the FTS5 index's own tokenizer;
  scope columns with FTS5's native syntax instead, e.g. --fts '{body}: timeout'.
  Compatible with --fts: -t/-T, -l, -m, --count, --row, --json, --stats,
  --max-columns (truncates snippets instead of match values).

  FTS output differs from regex/fixed search: matches are row-level, not
  column-span-level — text: "table:rowid: snippet"; JSON:
  {table, rowid, snippet, rank} (no column, no spans).

output:
  text (default):   table.column:rowid: snippet
                    (pk=(v1,v2) in place of rowid for WITHOUT ROWID tables
                    and rowid-fallback tables)
  json (--json):    {table, column, rowid|pk, value, spans}
                    ("row" added when --row is set)
  fts text:         table:rowid: snippet
  fts json:         {table, rowid, snippet, rank}

immutable / wal:
  Databases are opened read-only (mode=ro). A live WAL database additionally
  needs readable -wal/-shm sidecars, or a directory writable enough for
  SQLite to create one; when that's unavailable, litefind reports the
  specific condition and remedy. --immutable is an explicit opt-in for
  genuinely static files (read-only media, network mounts) — it is unsafe
  on a database that could be written concurrently; litefind never assumes
  immutability on its own.

exit codes (ripgrep's contract):
  0   at least one match found
  1   no matches found
  2   usage or runtime error

Run 'litefind -h' to see this text again.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(argv []string, stdout, stderr io.Writer) int {
	inv, err := parseInvocation(argv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, usage)
			return exitMatch
		}
		_, _ = fmt.Fprintf(stderr, "%v\n%s", err, usage)
		return exitError
	}

	if inv.sub == "version" {
		_, _ = fmt.Fprintf(stdout, "litefind %s\n", version())
		return exitMatch
	}

	db, err := openRO(inv.dbPath, inv.opts.immutable)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return exitError
	}
	defer func() { _ = db.close() }()

	switch inv.sub {
	case "tables":
		return cmdTables(db, inv.opts, stdout, stderr)
	case "schema":
		return cmdSchema(db, inv.glob, inv.opts.jsonOut, stdout, stderr)
	case "search":
		if inv.opts.fts != "" {
			return cmdSearchFTS(db, inv, stdout, stderr)
		}
		return cmdSearch(db, inv, stdout, stderr)
	}

	_, _ = fmt.Fprint(stderr, "not implemented yet\n")
	return exitError
}
