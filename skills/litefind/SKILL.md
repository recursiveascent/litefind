---
name: litefind
description: Use when searching or introspecting a SQLite database file (.db, .sqlite, .sqlite3) — finding rows matching a pattern across tables, listing tables, or reading schema/DDL. ripgrep-style read-only search where tables and columns replace paths and globs. Use instead of hand-writing SQL SELECT queries when you just need to locate data. Do not use for writing data, running arbitrary SQL, or modifying a database — litefind is strictly read-only.
license: MIT
compatibility: Requires the `litefind` binary on PATH.
metadata:
  homepage: https://github.com/recursiveascent/litefind
  spec: https://agentskills.io/specification
allowed-tools: Bash(litefind:*)
---

# litefind — ripgrep for SQLite databases

`litefind` is a read-only search and introspection tool for SQLite database files — the tool to reach for when you would reach for `rg` on a directory, but the target is a `.db`/`.sqlite`/`.sqlite3` file. Tables and columns replace paths and globs. The database is opened read-only (`mode=ro`); there is no write path.

Three modes:

```text
litefind PATTERN <db> [flags]                 search (regex by default; -F for literal)
litefind --tables <db> [flags]                list tables: name, kind, row count, column count
litefind --schema <db> [table-glob] [flags]   show DDL, columns, indexes, foreign keys
```

Flags may appear anywhere on the command line, rg-style: `litefind --json timeout db.sqlite` == `litefind timeout db.sqlite --json`. Introspection is selected only by `--tables` or `--schema`; without either option, the first positional string is always the search pattern.

## Install

Install the binary:
```
go install github.com/recursiveascent/litefind@latest
```

Install this skill by copying `SKILL.md` into your agent harness's skills directory (e.g. `~/.agents/skills/litefind/SKILL.md`, `~/.claude/skills/litefind/SKILL.md`). Some harnesses read skills from the repo; commit the `skills/litefind/` directory so agents in your project discover it automatically.

## When to use

Use when you encounter a SQLite database file and need to find rows matching a pattern, list tables, or read schema/DDL.

Do NOT use for writing data, running arbitrary SQL, or joins/aggregations/projections — there is no SQL escape hatch. Use `sqlite3` or your SQL client for those.

## Orient-then-search workflow

In an unfamiliar database, orient before searching:

1. `litefind --tables <db>` — inventory: names, kinds, row/column counts.
2. `litefind --schema <db> [table-glob]` — DDL and column detail for the tables you care about.
3. `litefind <pattern> <db> [flags]` — now you know which tables/columns to scope.

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
| `--row` | attach the full row (typed values) to each match; only visible with `--json` |
| `--json` | JSONL output, one object per match |
| `--stats` | print a search statistics summary line |
| `--max-columns N` | truncate displayed values to N chars in text output (default 200; 0 disables; JSON values are not truncated) |
| `--all-tables` | include `sqlite_*` and FTS5 shadow tables (hidden by default) |
| `--fts QUERY` | FTS5 match syntax; replaces PATTERN |
| `--immutable` | treat the DB as genuinely static (read-only media, network mounts); unsafe if it could be written concurrently |

`--tables` and `--schema` do not accept the search flags above. `--tables` accepts `--json`, `--no-counts`, `--all-tables`, `--immutable`; `--schema` accepts `--json`, `--immutable`.

Run `litefind --help` for the exhaustive, always-current flag reference.

### Output formats

```
text:   table.column:rowid: snippet          (pk=(v1,v2) in place of rowid for WITHOUT ROWID and rowid-fallback tables)
json:   {table, column, rowid|pk, value, spans}   ("row" added when --row is set; --row is JSON-only)
fts:    table:rowid: snippet                  (text) / {table, rowid, snippet, rank[, row]} (json; row added when --row is set)
```

Exit codes (ripgrep's contract): `0` match found, `1` no match, `2` usage/runtime error.

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

# Full row for context (requires --json)
litefind --json --row -t events timeout events.db # attach typed column values

# JSON for programmatic use
litefind --json -t events timeout events.db       # one JSON object per match

# FTS5 full-text search (index must already exist in the DB)
litefind --fts 'NEAR(timeout retry, 3)' events.db
litefind --fts '{body}: timeout' events.db        # FTS5 column-filter syntax
```

## Gotchas

**`-c` scopes columns, it is NOT count.** This is a deliberate divergence from rg, where `-c` means count. In litefind column scoping is the more common gesture, so `-c` means columns and count is long-only `--count`. The most common agent mistake is reaching for `-c` to count matches — use `--count`.

**`--fts` is a different matching regime.** These flags are rejected with a usage error (exit 2) when combined with `--fts`: `-F -i -S -w -c --all-tables`. Case and tokenization are governed by the FTS5 index's own tokenizer, not by litefind flags; scope columns with FTS5's native syntax, e.g. `--fts '{body}: timeout'`. Compatible with `--fts`: `-t`/`-T`, `-l`, `-m`, `--count`, `--row`, `--json`, `--stats`, `--max-columns`, `--immutable`. FTS output is row-level (no column/spans); rows are rank-ordered per FTS index, not globally across indexes.

**FTS requires an index that already exists.** `--fts` queries existing FTS5 indexes; it never builds one. If you scope `-t <table>` that has no covering FTS5 index, litefind exits 2 with complete, runnable `CREATE VIRTUAL TABLE` + trigger SQL to set one up. That SQL is a write to the user's database — do not run it without explicit approval. Present the SQL to your human partner and let them decide; until then, continue with ordinary read-only regex/literal search. Exceptions: a table with no stable row identity or no indexable TEXT-affinity columns gets an explanatory diagnostic instead of setup SQL (no synchronized index can be generated for it), and a `WITHOUT ROWID` table (or a rowid table with every rowid alias shadowed and no rowid-aliasing `INTEGER PRIMARY KEY`) gets standalone-FTS setup SQL (rerun scoping the new index directly, e.g. `-t <table>_fts`). litefind itself never writes to the database.

**Read-only, always.** Databases open with `mode=ro`; there is no write path. A live WAL database needs readable `-wal`/`-shm` sidecars, or a directory writable enough for SQLite to create them — otherwise litefind reports the specific condition and remedy. `--immutable` is an explicit opt-in for genuinely static files (read-only media, network mounts); it is unsafe on a database that could be written concurrently. litefind never assumes immutability on its own.

**Command-like words are ordinary patterns.** Search for `tables`, `schema`, or any other word directly: `litefind tables mydb.sqlite`. Only the `--tables` and `--schema` options select introspection.

**Regex engine is Go RE2, not PCRE.** Near-parity with ripgrep's Rust regex engine — both are lookaround-free, linear-time finite-automata engines, so most rg habits transfer. Known divergence: Go's `\b` is ASCII-only, where Rust's (and rg's) is Unicode-aware. Reference: https://pkg.go.dev/regexp/syntax

**Views are not searchable.** They have no rowid, so the identity and ordering contracts can't hold. Views still appear in `litefind --tables` and `litefind --schema` output.

**BLOBs and NULLs never match.** Non-BLOB values are matched against SQLite's own text rendition (`CAST(col AS TEXT)`), so numeric formatting is exactly what `sqlite3` would print (e.g. REAL `1.0` renders `'1.0'`). NULLs are excluded; BLOBs are skipped entirely.
