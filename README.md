# litefind

ripgrep for SQLite databases. Read-only regex, fixed-string, and FTS5 search,
plus schema introspection. Tables and columns replace paths and globs as the
scoping vocabulary.

## Install

```
go install github.com/recursiveascent/litefind@latest
```

Or build from source:

```
make build
```

## Usage

```
litefind PATTERN <db> [flags]               search (regex by default; -F for literal)
litefind tables <db> [flags]                list tables: name, kind, row count, column count
litefind schema <db> [table-glob] [flags]   show DDL, columns, indexes, foreign keys
```

Flags may appear anywhere on the command line, rg-style:

```
litefind --json timeout db.sqlite  ==  litefind timeout db.sqlite --json
```

### Examples

```
litefind timeout events.db                        regex search, all tables
litefind -F 'error: 42' events.db                 literal string match
litefind -t events -c message timeout events.db    scope to a table + column
litefind --fts 'NEAR(timeout retry, 3)' events.db FTS5 query syntax
litefind tables events.db                         table inventory
litefind schema events.db 'user*'                 DDL for tables matching a glob
```

### Search flags

| Flag | Meaning |
|------|---------|
| `-t, --table GLOB` | include only matching tables (repeatable) |
| `-T, --not-table GLOB` | exclude matching tables (repeatable) |
| `-c, --column [TABLE.]GLOB` | scope to matching columns (repeatable) |
| `-F, --fixed-strings` | pattern is a literal string, not a regex |
| `-i, --ignore-case` | case-insensitive matching |
| `-S, --smart-case` | case-insensitive unless pattern has an uppercase letter |
| `-w, --word-regexp` | require word boundaries around the match |
| `-l, --tables-with-matches` | print only names of tables containing matches |
| `-m, --max-count N` | stop after N matches per table |
| `--count` | print match counts per table instead of matches |
| `--row` | attach the full row (typed values) to each match |
| `--json` | JSONL output, one object per match |
| `--stats` | print a search statistics summary line |
| `--max-columns N` | truncate displayed values to N chars (default 200; 0 disables) |
| `--all-tables` | include `sqlite_*` and FTS5 shadow tables (hidden by default) |
| `--fts QUERY` | FTS5 match syntax; replaces PATTERN |

In ripgrep, `-c` means count. In litefind, `-c` scopes columns — the more common
gesture here — and count is long-only `--count`.

### Regex engine

Default pattern syntax is Go's RE2 (`regexp/syntax`), not PCRE. It has
near-parity with ripgrep's Rust regex engine: both are lookaround-free,
linear-time finite-automata engines, so most rg habits transfer directly.
Known divergence: Go's `\b` is ASCII-only, where Rust's (and rg's) is
Unicode-aware. Reference: https://pkg.go.dev/regexp/syntax

### FTS5 search

`--fts` queries FTS5 indexes that already exist in the database; it never
builds one. A scoped table participates two ways: it is itself an FTS5 table
(queried directly), or it has an external-content FTS5 sibling
(`content='<table>'`), in which case a search scoped to the source table is
mapped onto the sibling index.

If no index covers a scoped table, litefind exits 2 with complete, runnable
`CREATE VIRTUAL TABLE` and trigger SQL to set one up. Run that SQL once, then
rerun litefind.

`--fts` is a different matching regime from regex/fixed search, so these flags
are rejected when combined with it: `-F`, `-i`, `-S`, `-w`, `-c`,
`--all-tables`. Case and tokenization are governed by the FTS5 index's own
tokenizer; scope columns with FTS5's native syntax instead, e.g.
`--fts '{body}: timeout'`.

### Introspection

- `litefind tables <db>` — one line per table: name, kind (table/view/fts5),
  row count, column count. Use `--no-counts` to skip the `COUNT(*)` row
  counts on huge databases; views always show `-` since counting a view
  would execute it.
- `litefind schema <db> [table-glob]` — `CREATE` DDL plus structured column
  detail (type, notnull, default, pk), indexes, and foreign keys.

Both take `--json`.

### Output

Text (default):

```
table.column:rowid: snippet
```

`pk=(v1,v2)` appears in place of `rowid` for `WITHOUT ROWID` tables and
rowid-fallback tables.

JSON (`--json`):

```
{table, column, rowid|pk, value, spans}    # "row" added when --row is set
```

FTS output is row-level, not column-span-level:

```
table:rowid: snippet                        # text
{table, rowid, snippet, rank}              # json
```

### Read-only access

Databases are opened read-only (`mode=ro`). A live WAL database needs readable
`-wal`/`-shm` sidecars, or a directory writable enough for SQLite to create
them. When that is unavailable, litefind reports the specific condition and
remedy.

`--immutable` is an explicit opt-in for genuinely static files (read-only
media, network mounts). It is unsafe on a database that could be written
concurrently. litefind never assumes immutability on its own.

### Exit codes

ripgrep's contract:

| Code | Meaning |
|------|---------|
| 0 | at least one match found |
| 1 | no matches found |
| 2 | usage or runtime error |

## Development

```
make test     # go test ./...
make lint     # go vet ./...
make fmt      # gofmt -w .
```

Go 1.26 or later.
