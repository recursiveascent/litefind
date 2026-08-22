---
name: litefind
description: Use when searching, aggregating, or introspecting a SQLite database file (.db, .sqlite, .sqlite3, Chrome history, Core Data stores) — finding rows matching a pattern, counting frequent column values, listing tables, or reading schema/DDL. ripgrep-style read-only access where tables and columns replace paths and globs. Use instead of hand-writing SQL SELECT queries for these operations. Do not use for writing data, running arbitrary SQL, or modifying a database.
---

# litefind — ripgrep for SQLite databases

`litefind` is a search, aggregation, and introspection tool for SQLite database files — the tool to reach for when you would reach for `rg` on a directory, but the target is a `.db`/`.sqlite`/`.sqlite3` file. Tables and columns replace paths and globs.

Four modes:

```text
litefind PATTERN <db> [flags]                 search (regex by default; -F for literal)
litefind --freq -c [TABLE.]COLUMN [PATTERN] <db> [flags]
                                               count distinct values
litefind --tables <db> [flags]                list tables: name, kind, row count, column count
litefind --schema <db> [table-glob] [flags]   show DDL, columns, indexes, foreign keys
```

This skill describes litefind v0.1.1. Run `litefind --version` to confirm; if it
differs, `litefind --help` is authoritative and this file may be stale.

Flags may appear anywhere on the command line, rg-style: `litefind --json timeout db.sqlite` == `litefind timeout db.sqlite --json`. Introspection is selected only by `--tables` or `--schema`; without either option, the first positional string is always the search pattern.

If `litefind` is not on PATH, stop and tell your human partner how to install it
(`brew install recursiveascent/tap/litefind`). Do not install it yourself.

## Orient-then-search workflow

In an unfamiliar database, orient before searching:

1. `litefind --tables <db>` — inventory. On a large database (roughly >1 GB, or if this takes more than a few seconds), use --no-counts; row counts require a full COUNT(*) per table.
2. `litefind --schema <db> [table-glob]` — DDL and column detail for the tables you care about.
3. `litefind <pattern> <db> [flags]` — locate matching rows.
4. `litefind --freq -t <table> -c <column> <db>` — when the question is which values are most common.

Mirrors `ls` → `cat` → `rg` on a directory, and is faster than guessing table names.

## Quick reference

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
| `--row` | attach the full row to each match; JSON keeps types, TSV appends values in schema order |
| `--json` | JSONL output, one object per match |
| `--tsv` | tab-separated output for Unix pipelines |
| `--stats` | print a search statistics summary line |
| `--max-columns N` | truncate displayed values to N chars in text output (default 200; 0 disables; JSON values are not truncated) |
| `--all-tables` | include `sqlite_*` and FTS5 shadow tables (hidden by default) |
| `--fts QUERY` | FTS5 match syntax; replaces PATTERN |

### Frequency flags

| Flag | Meaning |
|------|---------|
| `--freq` | count distinct textual values in exactly one `-c` column |
| `--limit N` | emit at most N values (default 20; 0 disables) |
| `-t, -T, -c` | scope the single concrete target |
| `-F, -i, -S, -w` | filter grouped values when PATTERN is supplied |
| `--json` | emit `{table,column,value,count}` JSONL |
| `--tsv` | emit `table`, `column`, `value`, `count` as tab-separated fields |
| `--all-tables` | include shadow tables in target resolution |
| `--immutable` | caller asserts the database cannot change |

`--tables` and `--schema` do not accept the search flags above. `--tables` accepts `--json`, `--no-counts`, `--all-tables`, `--immutable`; `--schema` accepts `--json`, `--immutable`.

Run `litefind --help` for the exhaustive, always-current flag reference.

### Output formats

```
text:   table.column:rowid: snippet                 (pk=(v1,v2) in place of rowid for WITHOUT ROWID and rowid-fallback tables)
json:   {table, column, rowid|pk, value, spans}     ("row" added when --row is set)
tsv:    table<TAB>column<TAB>identity<TAB>value     (--row appends one scoped table's values in schema order)
fts:    table:rowid: snippet                        (text) / {table, rowid, snippet, rank[, row]} (json) / table<TAB>rowid<TAB>snippet (tsv)
freq:   <escaped-value>\t<count>                    (text) / {table, column, value, count} (json) / table<TAB>column<TAB>value<TAB>count (tsv)
```

Exit codes (ripgrep's contract): `0` match found, `1` no match, `2` usage/runtime error.

TSV emits full, unhighlighted values and escapes record delimiters. `--json` and
`--tsv` are mutually exclusive. `--row --tsv` requires one scoped table/FTS
target; use `-t`. TSV rejects `--stats`, `-l --count`, summary flags with
`--row`, and `--row --all-tables`.

## Recipes

```bash
# Orient
litefind --tables events.db                         # table inventory
litefind --schema events.db 'user*'                 # DDL for tables matching a glob

# Regex search (default)
litefind timeout events.db                        # all tables, all columns
litefind -t events -c message timeout events.db   # scope to a table + column
litefind -i error events.db                       # case-insensitive
litefind -w timeout events.db                     # word-boundaried

# Literal string (no regex metachars)
litefind -F 'error: 42' events.db

# Counting / locating
litefind --count error events.db                  # match count per table
litefind -l error events.db                       # just the table names with matches
litefind -m 5 timeout events.db                   # first 5 matches per table

# Frequency distribution
litefind --freq -t events -c level events.db      # most common levels
litefind --freq -i -t events -c message error events.db
litefind --freq --json --limit 10 -t events -c level events.db

# Full row for context
litefind --json --row -t events timeout events.db # typed JSON column values
litefind --tsv --row -t events timeout events.db  # schema-ordered TSV values

# TSV for Unix pipelines
litefind --tsv -t events timeout events.db | sort | uniq -c | sort -rn | head

# JSON for programmatic use
litefind --json -t events timeout events.db       # one JSON object per match

# FTS5 full-text search (index must already exist in the DB)
litefind --fts 'NEAR(timeout retry, 3)' events.db
litefind --fts '{body}: timeout' events.db        # FTS5 column-filter syntax
```

## Gotchas

**`-c` scopes columns, it is NOT count.** This is a deliberate divergence from rg, where `-c` means count. In litefind column scoping is the more common gesture, so `-c` means columns and count is long-only `--count`. The most common agent mistake is reaching for `-c` to count matches — use `--count`.

**Exit 1 means "no matches or results," not failure.** Do not retry, do not vary flags, do not report an error. Report that nothing matched or remained after filtering. Only exit 2 is an actual error.

**`--freq` needs exactly one concrete column.** Use `-t` and `-c` narrowly enough that they resolve to one `table.column`. Values group by `CAST(column AS TEXT)` under binary collation, so integer `1` and text `"1"` share a group; NULL and BLOB values are excluded. Matcher flags require an optional frequency pattern, and `--limit` applies after that filter.

**`--fts` is a different matching regime.** These flags are rejected with a usage error (exit 2) when combined with `--fts`: `-F -i -S -w -c --all-tables`. Case and tokenization are governed by the FTS5 index's own tokenizer, not by litefind flags; scope columns with FTS5's native syntax, e.g. `--fts '{body}: timeout'`. Compatible with `--fts`: `-t`/`-T`, `-l`, `-m`, `--count`, `--row`, `--json`, `--stats`, `--max-columns`, `--immutable`. FTS output is row-level (no column/spans); rows are rank-ordered per FTS index, not globally across indexes.

**FTS requires an index that already exists.** `--fts` queries existing FTS5 indexes; it never builds one. If you scope `-t <table>` that has no covering FTS5 index, litefind exits 2 with complete, runnable `CREATE VIRTUAL TABLE` + trigger SQL to set one up. That SQL is a write to the user's database — do not run it without explicit approval. Present the SQL to your human partner and let them decide; until then, continue with ordinary read-only regex/literal search. Exceptions: a table with no stable row identity or no indexable TEXT-affinity columns gets an explanatory diagnostic instead of setup SQL (no synchronized index can be generated for it), and a `WITHOUT ROWID` table (or a rowid table with every rowid alias shadowed and no rowid-aliasing `INTEGER PRIMARY KEY`) gets standalone-FTS setup SQL (rerun scoping the new index directly, e.g. `-t <table>_fts`). litefind itself never writes to the database.

**Read-only, always.** Databases open with `mode=ro`; there is no write path. A live WAL database needs readable `-wal`/`-shm` sidecars, or a directory writable enough for SQLite to create them — otherwise litefind reports the specific condition and remedy. `--immutable` is an explicit opt-in for genuinely static files (read-only media, network mounts); it is unsafe on a database that could be written concurrently. litefind never assumes immutability on its own.

**Command-like words are ordinary patterns.** Search for `tables`, `schema`, or any other word directly: `litefind tables mydb.sqlite`. Only the `--tables` and `--schema` options select introspection.

**Regex engine is Go RE2, not PCRE.** Near-parity with ripgrep's Rust regex engine — both are lookaround-free, linear-time finite-automata engines, so most rg habits transfer. Known divergence: Go's `\b` is ASCII-only, where Rust's (and rg's) is Unicode-aware. Reference: https://pkg.go.dev/regexp/syntax

**Views are not searchable.** They have no rowid, so the identity and ordering contracts can't hold. Views still appear in `litefind --tables` and `litefind --schema` output.

**BLOBs and NULLs never match.** Non-BLOB values are matched against SQLite's own text rendition (`CAST(col AS TEXT)`), so numeric formatting is exactly what `sqlite3` would print (e.g. REAL `1.0` renders `'1.0'`). NULLs are excluded; BLOBs are skipped entirely.
