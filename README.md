# litefind

[![CI](https://github.com/recursiveascent/litefind/actions/workflows/ci.yml/badge.svg)](https://github.com/recursiveascent/litefind/actions/workflows/ci.yml)

ripgrep for SQLite databases. Read-only regex, fixed-string, and FTS5 search,
frequency distribution, numeric aggregation, plus schema introspection. Tables and
columns replace paths and globs as the scoping vocabulary.

## Install

### curl | sh

```
curl -fsSL https://litefind.dev/install.sh | sh
```

Installs the latest release binary for your platform, verifying it against
the release checksums. No `sudo`: installs to `/usr/local/bin` if writable by
the current user, otherwise `~/.local/bin`. Override the destination with
`LITEFIND_INSTALL_DIR`.

### Homebrew

```
brew install recursiveascent/tap/litefind
```

### Go

```
go install github.com/recursiveascent/litefind@latest
```

Installs to `$(go env GOPATH)/bin`. Pin a version with `@v1.2.3` instead of
`@latest`.

### Nix

Run it once without installing:

```
nix run github:recursiveascent/litefind -- timeout events.db
```

Drop into a shell with it on `$PATH`:

```
nix shell github:recursiveascent/litefind
```

Install into your profile:

```
nix profile install github:recursiveascent/litefind
```

Or add it as a flake input:

```nix
{
  inputs.litefind.url = "github:recursiveascent/litefind";
  # ...
  environment.systemPackages = [ inputs.litefind.packages.${system}.default ];
}
```

### Release binaries

Prebuilt tarballs for macOS and Linux (amd64 and arm64) are on the
[releases page](https://github.com/recursiveascent/litefind/releases). Download,
verify, extract, and put `litefind` somewhere on your `$PATH`:

```
tar xzf litefind_1.2.3_darwin_arm64.tar.gz
install -m755 litefind /usr/local/bin/
```

### From source

```
git clone https://github.com/recursiveascent/litefind
cd litefind
# optionally, `nix develop` first
make build
```

Requires Go 1.26 or later.

### Verify

```
litefind --version
```

### Agent skill

Install the bundled skill for your agent:

```
litefind --skill install
```

This writes to `~/.agents/skills/litefind/SKILL.md`. Use `--target project` for
`./skills`, or `--target both` for both locations. Check an installed copy with
`litefind --skill status`; print the embedded file for another installer with
`litefind --skill print`.

## Usage

```text
litefind PATTERN <db> [flags]                 search (regex by default; -F for literal)
litefind --freq -c [TABLE.]COLUMN [PATTERN] <db> [flags]
                                               count distinct values
litefind --agg KIND -c [TABLE.]COLUMN [PATTERN] <db> [flags]
                                               aggregate numeric values
litefind --tables <db> [flags]                list tables: name, kind, row count, column count
litefind --schema <db> [table-glob] [flags]   show DDL, columns, indexes, foreign keys
```

Flags may appear anywhere on the command line, rg-style. Introspection is
selected only by `--tables` or `--schema`; without either option, the first
positional string is always the search pattern.

```
litefind --json timeout db.sqlite  ==  litefind timeout db.sqlite --json
```

### Examples

```
litefind timeout events.db                         regex search, all tables
litefind -F 'error: 42' events.db                  literal string match
litefind -t events -c message timeout events.db    scope to a table + column
litefind --freq -t events -c level events.db         most common event levels
litefind --freq -i -t events -c message error events.db
litefind --agg stats -t events -c duration_ms events.db # numeric statistics
litefind --agg avg -t events -c duration_ms timeout events.db
litefind --fts 'NEAR(timeout retry, 3)' events.db  FTS5 query syntax
litefind --tables events.db                        table inventory
litefind --schema events.db 'user*'                DDL for tables matching a glob
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
| `--tsv` | tab-separated output for Unix pipelines |
| `--stats` | print a search statistics summary line |
| `--max-columns N` | truncate displayed values to N chars (default 200; 0 disables) |
| `--head N` | preview the first N lines in text/JSON output (0 disables) |
| `--all-tables` | include `sqlite_*` and FTS5 shadow tables (hidden by default) |
| `--fts QUERY` | FTS5 match syntax; replaces PATTERN |

In ripgrep, `-c` means count. In litefind, `-c` scopes columns — the more common
gesture here — and count is long-only `--count`.

### Frequency distribution

`--freq` requires `-c` to resolve to exactly one concrete column. Values are
counted using SQLite's textual representation (`CAST(column AS TEXT)`), so
integer `1` and text `"1"` share a group; NULL and BLOB values are excluded.
Results sort by count descending, then value ascending under binary collation.

`--limit N` defaults to 20 and `--limit 0` disables the limit. An optional
pattern filters grouped values with the same regex, fixed-string, case, and word
semantics as ordinary search. The limit applies after filtering.

Text output is `<escaped-value>\t<count>`. `--json` emits JSONL objects with
`table`, `column`, `value`, and `count`. `--tsv` emits those four fields as one
tab-separated record.

### Numeric aggregation

`--agg avg|sum|min|max|stats` requires `-c` to resolve to exactly one column
with INTEGER, REAL, or NUMERIC affinity. Only rows whose target value has
runtime INTEGER or REAL storage are included; NULL, BLOB, and nonnumeric stored
values are excluded. `stats` computes all four aggregates plus the included
value count in one query.

An optional pattern filters the target column's SQLite text rendering with the
same regex, fixed-string, case, and word semantics as search. It does not search
other columns in the row. `--json` emits one object; aggregate mode does not
support TSV.

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

- `litefind --tables <db>` — one line per table: name, kind (table/view/fts5),
  row count, column count. Use `--no-counts` to skip the `COUNT(*)` row
  counts on huge databases; views always show `-` since counting a view
  would execute it.
- `litefind --schema <db> [table-glob]` — `CREATE` DDL plus structured column
  detail (type, notnull, default, pk), indexes, and foreign keys.

Both take `--json`.

### Output

Use `--head N` for line-oriented previews of multiline values:

```sh
litefind --head 5 -t events -c message timeout events.db
```

Text (default):

```
table.column:rowid: snippet
```

`pk=(v1,v2)` appears in place of `rowid` for `WITHOUT ROWID` tables and
rowid-fallback tables. `--head N` emits the first N physical lines and an
omitted-line marker; `--head 0` keeps the full multiline value. It cannot be
combined with `--max-columns` or `--tsv`.

JSON (`--json`):

```
{table, column, rowid|pk, value, spans}    # "row" added by --row; "truncated_lines" by --head
```

TSV (`--tsv`) emits full, unhighlighted values:

```
table<TAB>column<TAB>identity<TAB>value
```

Backslashes, tabs, newlines, carriage returns, control bytes, and invalid UTF-8
bytes are escaped so every match remains one record. With `--row`, narrow the
search to one table with `-t`; row values follow in schema order. `--tsv` is
mutually exclusive with `--json` and `--stats`.

FTS output is row-level, not column-span-level:

```
table:rowid: snippet                        # text
{table, rowid, snippet, rank}               # json
table<TAB>rowid<TAB>snippet                 # tsv
```

Frequency output:

```
<escaped-value>\t<count>                     # text
{table, column, value, count}               # json
table<TAB>column<TAB>value<TAB>count        # tsv
```

Aggregate output:

```
table.column: avg=... sum=... min=... max=... count=...  # text, stats
table.column avg: ...                                      # text, one aggregate
{table, column, avg, sum, min, max, count}                 # json, stats
{table, column, <kind>, count}                             # json, one aggregate
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
| 0 | at least one match or result found |
| 1 | no matches or results found |
| 2 | usage or runtime error |
