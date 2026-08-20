# litefind design

litefind is a read-only search and introspection tool for SQLite databases,
modeled on ripgrep's command interface and ergonomics. Where ripgrep scopes
search by file path and glob, litefind scopes by table and column. This
document describes the high-level architecture; the source files are the
ground truth.

## Scope and contract

Invocation modes:

- **search** (default) — regex, fixed-string, or FTS5 search. Regex/fixed
  search scans the textual representation (`CAST(c AS TEXT)`) of every
  searchable, non-NULL, non-BLOB column value — `typeof` of `null` or `blob`
  is skipped, so integers, reals, and text are all matched against the
  pattern. FTS5 search queries existing indexes over their indexed text.
  Both support table globs (`-t`/`-T`); column globs (`-c`) apply only to
  regex/fixed search — FTS5 rejects `-c` and scopes columns with its own
  query syntax (e.g. `--fts '{col}: query'`).
- **`--tables`** — table inventory: name, kind, row count, column count.
- **`--schema`** — DDL plus structured column, index, and foreign-key detail.

The database is opened **read-only** (`mode=ro`). litefind never writes to a
database, never creates an FTS5 index, and never assumes immutability (the
`--immutable` flag is an explicit, caller-asserted opt-in). Exit codes mirror
ripgrep for **search** and **`--schema`**: 0 = match found, 1 = no match,
2 = usage or runtime error. **`--tables`** returns 0 on success and 2 on error.
`--help` prints to stdout and returns 0 on a valid invocation; its `fmt.Fprint`
result is not checked, so a stdout write failure is not propagated as an error
(unlike search, tables, and schema, which check every write). `-h`/`--help`
short-circuits all remaining parsing and semantic validation (it prints usage
and returns 0) once reached; the only thing that takes precedence is a parsing
error encountered *before* it, such as an unknown flag. Extra positional args
or unsupported *known* flags that appear before help are not subsequently
rejected — help wins. Absent help, other invalid invocations fail during
parsing or per-mode validation and return 2 before the write.

## Package layout

All code is in a single `main` package — no internal libraries. The module
depends only on the Go stdlib and `modernc.org/sqlite` (a pure-Go SQLite
driver). Symbols are unexported unless the tool's entry point requires
otherwise.

| File       | Responsibility                                                     |
|------------|-------------------------------------------------------------------|
| `main.go`  | Entry point, `run` dispatcher, usage text.                         |
| `args.go`  | Argument reordering, flag parsing, mode and flag validation.       |
| `db.go`    | Read-only open + diagnostics, catalog, DDL tokenizer, identity resolution. |
| `search.go`| Regex/fixed search: matching, batching, streaming, scoping, command. |
| `fts.go`   | FTS5 search: target resolution, query, setup-SQL generation, command. |
| `introspect.go` | `--tables` and `--schema` modes.                              |
| `output.go`| Text/JSON rendering, truncation, ANSI highlighting.                 |

## Control flow

`run` (main.go) parses the invocation, opens the database read-only, and
dispatches to search, tables, or schema according to the parsed mode. The
command signatures differ: `cmdSearch`/`cmdSearchFTS` take the full
`*invocation`; `cmdTables` takes just `searchOpts`; `cmdSchema` takes the glob
and the JSON flag. All receive the `*database` and `stdout`/`stderr` writers
and return an exit code. Opening the database happens after parsing, so parse
errors never touch the file.

### Argument handling (args.go)

litefind accepts flags in any position, rg-style (`--json timeout db` ==
`timeout db --json`). `reorderArgs` walks the argv once, separating flags from
positionals while keeping each value-taking flag with its argument. When the
caller supplied an explicit `--`, it is moved ahead of the positionals so the
stdlib `flag` parser treats everything after it as positional; `reorderArgs`
only relocates a supplied `--`, it never inserts one, so a leading-dash
pattern without an explicit `--` is still parsed as a flag (use `--` to pass
such patterns). After parsing, `parseInvocation` selects tables or schema only
when the corresponding `--tables` or `--schema` flag was supplied; supplying
both is a usage error. Otherwise every positional string is handled by search,
including the literal patterns `tables`, `schema`, and `quickstart`.

Flag validation is layered: `checkFTSFlagCompat` rejects regex-oriented flags
combined with `--fts`; `validateFlagsForMode` enforces a per-mode
allowlist, using a different (narrower) allowlist for search+FTS. The
canonical-flag map (`flagCanonical`) normalizes short/long spellings so error
messages name a flag the user can actually type.

### Database access (db.go)

`openRO` stats the file (distinguishing "no such file" from other stat
errors), builds a `file:` DSN with `mode=ro` and a busy timeout, and forces a
real read (`SELECT count(*) FROM sqlite_master`) so header validation and WAL
setup happen immediately. `diagnoseOpen` maps SQLite's error strings onto
one-line messages with remedies: not a database, locked, or the live-WAL +
read-only-directory condition (detected by `walBlocked`, which checks for a
`-wal` sidecar alongside a CANTOPEN-style message).

### Catalog

`catalog` is the structural backbone. It returns every table and view in
`sqlite_master` order with:

- **kind** — `table`, `view`, or `fts5`, resolved structurally from
  `PRAGMA table_list` (not by guessing from DDL text). FTS5 is identified by
  parsing the `CREATE VIRTUAL TABLE ... USING fts5(...)` statement.
- **shadow** — `sqlite_*` internals and virtual-table shadow tables.
  `table_list`'s shadow classification is trusted, with one correction: a
  user table named `base_content` next to an external-content/contentless FTS5
  table `base` is reclassified as non-shadow, since FTS5 creates no
  `_content` table in those modes.
- **FTS5 metadata** — `content=` (external-content source) and
  `content_rowid=` options, parsed out of the virtual-table DDL by a
  purpose-built tokenizer.
- **columns** — from `PRAGMA table_xinfo`, including hidden columns (filtered
  by hidden class: plain, generated-virtual, and generated-stored are
  searchable; virtual-table hidden are not).
- **identity** — how a table's rows are addressed (see below).

A view whose columns can't be introspected (it selects from a dropped table)
is recorded with `xinfoErr` and returned intact — search ignores it, `tables`
still lists it, `schema` reports the error against that view alone. A table
failing xinfo is fatal.

### DDL tokenizer

`parseVirtualTableDDL` extracts the FTS5 module name and `content=` /
`content_rowid=` arguments from a `CREATE VIRTUAL TABLE` statement. It uses a
narrow DDL lexer (`tokenizeDDL`) that skips whitespace and comments,
distinguishes barewords from quoted identifiers (`"x"`, `[x]`, `` `x` ``) and
string literals (`'x'`), and tracks paren depth — so a quoted table name
containing the text `using fts5` is never mistaken for the real clause. All
identifier comparison uses `asciiFold`/`asciiEqualFold`, not `strings.ToLower`,
because SQLite folds only ASCII for identifiers.

### Identity resolution

`resolveIdentity` implements the spec's fallback chain, in order:

1. WITHOUT ROWID → declared PK (`idPK`).
2. Unshadowed `rowid`/`_rowid_`/`oid` alias (`idAlias`).
3. Lone `INTEGER PRIMARY KEY` (`idIntegerPK`) — unless declared `DESC`,
   which suppresses the rowid alias (detected via `hasPKIndex`: a true
   rowid alias gets no `origin='pk'` index).
4. Declared PK verified non-NULL via `pkAllNonNull` (`idPK`).
5. None → `idNone`, the table is skipped with a warning.

`pkAllNonNull` runs both at catalog time (advisory — saves starting a scan
that would be abandoned) and inside each scan's transaction (authoritative —
the snapshot it inspects is the one the scan reads).

### Search (search.go and fts.go)

#### Regex/fixed search

`cmdSearch` resolves scope, scans up to `GOMAXPROCS` tables concurrently, and
emits results in catalog order (not completion order).

**Scoping** (`scopeTables`, `resolveColsForTable`): default scope is non-shadow
tables and FTS5 tables; `--all-tables` widens to everything except views. `-t`
includes by glob, `-T` excludes by glob, `-c` opts into specific columns (a
`TABLE.COLGLOB` qualifier scopes a column glob to one table). Globs use
`path.Match` and are validated before catalog runs so a malformed glob —
a pure usage error — never pays for catalog and identity-resolution
queries (the read-only open's `sqlite_master` probe has already run by then).

**Matching** (`scanTable`): for each table, one transaction gives every batch
query the same consistent snapshot. The query shape is identity-first, then
`typeof(c), CAST(c AS TEXT)` per column. `typeof` gates matching: `null` and
`blob` columns never match; the CAST result is matched against the compiled
regexp. Wide tables are scanned in **column batches** sized to stay within
SQLite's `SQLITE_MAX_COLUMN` (2000) result-column limit, accounting for the
identity columns repeated in every batch. Batch cursors are merged in lockstep
(`advanceAll`) so output is row-major and identity-ordered — a mid-table
cursor divergence is an internal invariant violation (all cursors share one
transaction snapshot, so concurrent writes are invisible to the scan) and
aborts the scan.

**Memory bounding**: matches stream to the emitter through bounded per-table
channels (`tableMatchBuffer = 256`) as they are found, never accumulating a
whole result set. A table may only start against one of `GOMAXPROCS` scan
slots; the slot is released only when the emitter has drained that table's
stream, so live matches are bounded by the channel buffers (`poolSize ×
tableMatchBuffer`) plus a small per-worker constant: each scan goroutine
blocked on a full channel retains one pending match, and the emitter holds
one current match. Streams are created as slots are taken (in catalog order)
and become garbage once drained, so a thousand-table search keeps at most
`poolSize` stream buffers live concurrently, not a thousand.

**--row**: the full row is fetched via separately batched raw-value queries
(no identity columns, so the full `maxResultColumns` budget goes to data),
merged in lockstep with the match cursors on the shared `ORDER BY`.

**Output modes**: default (print matches), `-l` (table names only), `--count`
(per-table counts), `--stats` (summary line). Every stdout write is checked;
the first failure (e.g. broken pipe) is treated like a scan error → stderr,
exit 2.

#### FTS5 search

`cmdSearchFTS` resolves FTS5 targets, queries each in catalog order
(sequential — targets are processed one at a time, with no concurrency), and
prints per the FTS output contract (row-level, not column-span-level).

**Target resolution** (`resolveFTS`): with no `-t`, every non-shadow
non-contentless FTS5 table is a direct target. With `-t`, each matched table
resolves as: a scoped FTS5 table (direct target; contentless is an error), or
an ordinary table mapped onto its `content=` sibling index. Multiple siblings
→ ambiguity error. No index → `ftsMissError`, which emits a setup-SQL
template using fixed names (`<table>_fts`, `<table>_fts_ai/_ad/_au`) — runnable
as-is unless an object with one of those names already exists, in which case
rename before running (external-content for rowid-accessible tables;
standalone FTS5 for WITHOUT ROWID / rowid-inaccessible tables) — or a
diagnostic (no stable
identity, no TEXT-affinity columns). `-T` prunes before resolution. Targets
are deduplicated by index name, preferring the source-carrying entry so
`--row` fetches from the source table.

**Query** (`searchFTSTarget`): `SELECT rowid, snippet(...), rank FROM <idx>
WHERE <idx> MATCH ? ORDER BY rank, rowid`. `--row` fetches the full row from
the external-content source (or the index itself for standalone/internal
tables) keyed by `content_rowid`.

### Introspection (introspect.go)

`cmdTables` lists each table's name, kind, row count (skipped for views and
with `--no-counts`), and column count. `cmdSchema` prints DDL plus structured
columns, indexes, and foreign keys. Both use a `stickyWriter` that remembers
the first write failure, turning "did any of this reach the user?" into one
question the caller asks per object — so a truncated `tables` or `schema` run
exits 2 instead of pretending success. Index DDL is attached verbatim because
`PRAGMA index_info` reports only column names, losing partial-index
predicates, DESC keys, COLLATE clauses, and expression keys.

### Output (output.go)

A `printer` renders matches as text lines or JSONL objects. In text mode:
values are single-lined (`\n`→`\n` etc.), truncated to a `--max-columns`-rune
window centered on the first match span (with ellipses), and highlighted with
ANSI when stdout is a terminal. JSON output is always the full, unhighlighted
value. The window is computed in **escaped-rune** coordinates so a multibyte
character costs one slot and an expanded control-character escape (`\n`,
`\r`, `\t`) costs two; the bounded fetch
region is centered on the span's escaped-rune midpoint, not a byte
midpoint, so uneven escape-width distributions don't skew the window.

FTS snippets carry `\x01`/`\x02` marker bytes around matched terms.
`extractMarkerSpans` strips these into ordinary byte-offset spans over the
marker-free text, so an FTS snippet flows through the same
truncation/highlighting pipeline as a regex match value.

## Key design principles

1. **Read-only, never assumes immutability.** `mode=ro` always; `--immutable`
   is an explicit opt-in for genuinely static files.
2. **Structural classification over text guessing.** `PRAGMA table_list` is
   the source of truth for WITHOUT ROWID and kind, corrected only where its
   one confirmed blind spot (FTS5 external-content `_content` naming) is
   known.
3. **Bounded memory.** Regex/fixed search streams matches through bounded
   per-table channels, pool-tied stream lifetimes, and column batching, so
   the channels bound the match *count* in flight (`poolSize ×
   tableMatchBuffer`), not the result-set count — but they do not bound
   payload bytes. Each match carries the full `CAST(c AS TEXT)` value in
   `match.value`, so a single large value or a full batch of them can hold
   arbitrarily many bytes regardless of `--max-columns` (truncation only
   bounds additional rendering allocations, not the buffered value). The
   `--row` payload adds the full row (a map whose size grows with table
   width). FTS5 search accumulates each target's full result set in a slice
   before emission (it is sequential, with no concurrency: one target's slice
   is built, emitted, then the next, so peak memory is the largest single
   target's result set, not the sum across targets). Truncation and the
   bounded fetch region keep per-value *rendering* memory proportional to the
   window, not the value's full length — but only for truncated text-mode
   output; JSON output and `--max-columns 0` render the full value, so
   per-value memory is unbounded there.
4. **Snapshot consistency.** One transaction per table scan gives every
   batch cursor the same snapshot; identity is re-verified inside it so a
   concurrent writer can't break the identity a scan relies on.
5. **rg ergonomics.** Flags anywhere on the line, ripgrep's exit codes, the
   same `-t`/`-T`/`-i`/`-F`/`-w` vocabulary remapped to tables and columns.
6. **Diagnosable errors.** Open failures map to one-line messages with
   remedies; write failures propagate rather than truncating silently in the
   search, tables, and schema modes (`--help` is the documented exception —
   its stdout write is unchecked).
