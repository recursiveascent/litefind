# litefind design

litefind is a read-only search, aggregation, and introspection tool for SQLite
databases, modeled on ripgrep's command interface and ergonomics. Where
ripgrep scopes search by file path and glob, litefind scopes by table and
column. This document describes the high-level architecture; the source
files are the ground truth.

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
- **`--freq`** — frequency distribution for exactly one scoped column. Values
  use `CAST(c AS TEXT)`, excluding NULL and BLOB storage classes, and group
  under binary collation. An optional ordinary matcher filters grouped values
  before `--limit` is applied.
- **`--tables`** — table inventory: name, kind, row count, column count.
- **`--schema`** — DDL plus structured column, index, and foreign-key detail.

The database is opened **read-only** (`mode=ro`). litefind never writes to a
database, never creates an FTS5 index, and never assumes immutability (the
`--immutable` flag is an explicit, caller-asserted opt-in). Exit codes mirror
ripgrep for **search**, **frequency**, and **`--schema`**: 0 = result found,
1 = no result, 2 = usage or runtime error. **`--tables`** returns 0 on
success and 2 on error.
`--help` prints to stdout and returns 0 on a valid invocation; its `fmt.Fprint`
result is not checked, so a stdout write failure is not propagated as an error
(unlike search, frequency, tables, and schema, which check every write).
`-h`/`--help`
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
| `freq.go`  | Frequency target resolution, grouping, filtering, and output.       |
| `fts.go`   | FTS5 search: target resolution, query, setup-SQL generation, command. |
| `introspect.go` | `--tables` and `--schema` modes.                              |
| `output.go`| Text/JSON rendering, truncation, ANSI highlighting.                 |

## Control flow

`run` (main.go) parses the invocation, opens the database read-only, and
dispatches to search, frequency, tables, or schema according to the parsed
mode. `cmdSearch`, `cmdSearchFTS`, and `cmdFreq` take the full `*invocation`;
`cmdTables` takes just `searchOpts`; `cmdSchema` takes the glob and the JSON
flag. All receive the `*database` and `stdout`/`stderr` writers
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
such patterns). After parsing, `parseInvocation` selects frequency, tables, or
schema only when the corresponding `--freq`, `--tables`, or `--schema` flag was
supplied; combining mode selectors is a usage error. Frequency accepts either
`<db>` or `<pattern> <db>`, records explicit pattern presence separately from
its value, and requires `-c`. Otherwise every positional string is handled by
search, including the literal patterns `tables`, `schema`, and `quickstart`.

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

#### Frequency distribution

`cmdFreq` validates globs, reuses `scopeTables` and `resolveColsForTable`, and
requires their expansion to produce exactly one concrete table-column pair.
It ignores row identity, so tables that regex search cannot address safely can
still be aggregated.

SQLite groups `CAST(column AS TEXT)` values after excluding `typeof` values
`null` and `blob`. Both grouping and tie-breaking use `BINARY` collation, so a
column's declared collation cannot merge distinct textual values. Results sort
by count descending and value ascending. Without a pattern, a positive
`--limit` is part of the SQL query. With a pattern, grouped rows stream in that
same order through `buildMatcher`, and litefind stops after the requested
number of matching groups, making the limit apply after filtering.

Text output is `<escaped-value>\t<count>` with control characters single-lined
through the existing renderer. JSONL output contains `table`, `column`,
`value`, and `count`. Query, scan, iteration, close, and output failures return
exit 2; no emitted groups returns exit 1.

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

A `printer` renders regex/fixed and FTS matches as text or JSONL objects.
Default text mode single-lines values (`\n`→`\n` etc.), truncates to a
`--max-columns`-rune window centered on the first match span (with ellipses), and highlights with
ANSI when stdout is a terminal. JSON output normally preserves the full,
unhighlighted value. The window is computed in **escaped-rune** coordinates so a multibyte
character costs one slot and an expanded control-character escape (`\n`,
`\r`, `\t`) costs two; the bounded fetch
region is centered on the span's escaped-rune midpoint, not a byte
midpoint, so uneven escape-width distributions don't skew the window.

When `--head N` is explicit, the printer instead keeps the first N logical
lines (`0` means unbounded), preserves their newline separators in text output,
and appends `[... N more lines]` when content was omitted. JSON truncates the
`value` or FTS `snippet` to the same raw prefix and adds `truncated_lines` only
when nonzero. Regular-search spans are clipped to the retained byte range.
`--head` is mutually exclusive with `--max-columns` and with TSV, whose contract
requires full values and one physical record per match.

FTS snippets carry `\x01`/`\x02` marker bytes around matched terms.
`extractMarkerSpans` strips these into ordinary byte-offset spans over the
marker-free text, so an FTS snippet flows through the same
truncation/highlighting pipeline as a regex match value.

Frequency text output calls `renderUnbounded` without spans or color to reuse
control-character escaping without truncation. Its JSONL path uses a dedicated
anonymous record containing the resolved target and aggregate count.

`--tsv` is a machine-oriented output mode accepted by regex/fixed search,
FTS search, and frequency mode. It is mutually exclusive with `--json`; the
`--tables` and `--schema` introspection modes reject it. Search's `-l` and
`--count` variants remain search output and do accept TSV. `--stats` is rejected
with TSV so a data stream never mixes records with different schemas. TSV also
rejects `-l` combined with `--count`, rejects `--row` combined with either
summary flag, and rejects `--row --all-tables` because shadow-table schemas are
not exposed by `--schema`. TSV emits no header, no ANSI highlighting, and, like
JSON, full values rather than the text renderer's `--max-columns` window. Full values and repeated `--row` payloads may
produce substantially more output than default text mode.

Fields are separated by literal tabs and records by literal newlines. Textual
fields escape backslash, tab, newline, and carriage return as `\\`, `\t`, `\n`,
and `\r`; other C0 control bytes, DEL, and invalid UTF-8 bytes become `\xhh`
with exactly two lowercase hexadecimal digits. Appended row values are textual, not generally type-preserving—callers needing
fully typed data use `--json`—but NULL and BLOB remain unambiguous: SQL NULL is
the reserved token `\N`, and BLOB is `\B` followed by lowercase hexadecimal.
INTEGER uses `strconv.FormatInt(v, 10)`; REAL uses
`strconv.FormatFloat(v, 'g', -1, 64)`, preserving signed zero and using Go's
lowercase exponent form when needed. Non-finite values, if returned by the
SQLite driver, therefore serialize as `NaN`, `+Inf`, or `-Inf`. Serialization
branches on SQLite storage class first: reserved NULL/BLOB tokens are emitted
directly, while every other value passes through text escaping. Literal TEXT
such as `\N`, `\Bff`, or an invalid byte rendered as `\xff` begins with an
escaped backslash (`\\...`) and cannot collide with a reserved token.

Record layouts are exact and mode-specific:

- regular match: `table`, `column`, `identity`, `value`
- regular match with `--row`: the four fields above, followed by every value
  included by the existing JSON `--row` payload, in schema order
- FTS match: `table`, `rowid`, `snippet`; rank is intentionally omitted to keep
  the issue's Unix-pipeline contract
- FTS match with `--row`: the three fields above, then `source_table`, followed
  by every value included by the existing JSON `--row` payload in source-table
  schema order
- frequency: `table`, `column`, `value`, `count`
- search `-l`: `table`
- search `--count`: `table`, `count`

Because `--row` records have table-dependent trailing fields and TSV has no
header, source resolution happens after catalog/FTS target resolution but before any
result-producing scan/query or stdout write. Regex/fixed `--row --tsv` requires `scopeTables` to contain
exactly one table, regardless of which tables would eventually match. FTS
`--row --tsv` requires `resolveFTS` to produce exactly one target: its source is
`tgt.source` for an external-content index, otherwise `tgt.index`. The resolved
source must exist in the catalog and supply the row projection; a missing
external-content source exits 2 before result output even if the query would
produce no matches. Any multi-target result exits 2 before output with guidance
to narrow `-t`.

Regex/fixed search appends the existing JSON `row` projection in catalog
order: columns from `PRAGMA table_xinfo` whose `hidden` value is `0`, `2`, or
`3`; virtual-table hidden columns (`hidden=1`) remain excluded. FTS emits the
resolved source name as `source_table`, then appends that same projection in
catalog order. FTS row retrieval selects the catalog-derived, individually
quoted column list rather than `SELECT *`; a rename/removal therefore fails the
query instead of silently reassigning positions. Generated columns (`hidden=2`
or `3`) are included in both modes, while virtual-table hidden columns are
included in neither.

The mapping is obtained with `litefind --schema <db> <table> --json`, using the
regular record's `table` or the FTS record's explicit `source_table`. Its existing
`columns` array is already the canonical row projection in catalog (`cid`) order:
`catalog()` excludes virtual-table hidden columns (`hidden=1`) and retains
ordinary/generated columns before schema output is built. Both TSV row modes use
every returned array entry, so no schema JSON change is required. An integration
test derives this projection from schema JSON and compares names, count, and
order to emitted trailing fields. Row payloads retain that order internally
alongside the map used by JSON, so all existing JSON behavior is unchanged. The projection is
fixed from the invocation's catalog result before scanning. Regular row queries
already select that explicit catalog column list; FTS adopts the same rule.
Concurrent DDL is not reconciled: if any projected column is renamed or removed,
SQLite rejects the explicit query and litefind exits 2 rather than emitting a
same-width but mislabeled row. Consumers mapping fields with a separate
`--schema` invocation require a database whose schema remains stable between
the two commands. SQLite rejects duplicate column names within one
table under its identifier rules, so the ordered projection cannot contain two
indistinguishable names; the ordered slice, not the JSON map iteration order,
controls TSV output.

A regular rowid identity is its base-10 integer. A non-rowid identity is `pk=`
followed by a JSON array of typed strings in declared key order. Tokens are
`n:`, `i:<decimal>`, `r:<shortest-round-trip-decimal>`, `t:<escaped-bytes>`, and
`b:<lowercase-hex>` for NULL, integer, real, text, and BLOB respectively. Text
bytes use the TSV escaping rules before JSON encoding, so invalid UTF-8 and
reserved-looking text remain lossless. This identity field is self-delimiting
and independent of the human renderer's `pk=(...)` representation.

There is intentionally no header or format-version record: every accepted
non-row command has one fixed record schema, and `--row` is restricted to one
source schema. Table names, column names, source names, regular match values,
FTS snippets, and frequency values are always TEXT fields. Regular search reads
`typeof(column)` beside `CAST(column AS TEXT)` and skips NULL/BLOB storage
classes before running the matcher; they can never produce a regular match
record. All other storage classes match and emit SQLite's cast string. Frequency applies the same `typeof(column) NOT IN ('null','blob')`
filter before grouping, groups by `CAST(column AS TEXT) COLLATE BINARY`, and
emits that same cast group key, so INTEGER `1`, REAL `1.0` (whose SQLite cast is
`'1.0'`), and TEXT values group exactly according to their cast strings; values
with the same cast string share one group regardless of original storage class.
Frequency sorts by count descending, then cast value ascending with `BINARY`
collation. NULL and BLOB frequency groups cannot exist. FTS emits SQLite's
marker-stripped `snippet(...)` text. These fields therefore represent SQLite's text conversion,
not the original storage class, and never interpret `\N` or `\B...` as typed
tokens. Rowid/count fields are decimal integers. Only trailing `--row` fields
use the storage-class-aware NULL/BLOB tokens, while the regular identity field
follows the rowid/typed-PK grammar above. TSV preserves each mode's existing
deterministic ordering: regular matches are catalog-table order, then row
identity, then catalog-column order; FTS targets are catalog order and matches
within each target are rank then rowid; `-l`/`--count` follow catalog/target
order; frequency uses the count/value ordering above.

A decoder reverses TEXT escapes left-to-right: `\\`, `\t`, `\n`, `\r`, and
`\xhh` are the only valid escapes, and emitted hex digits are lowercase. Unknown
escapes, incomplete or non-hex `\xhh`, odd/non-hex BLOB payloads, malformed PK
JSON/tokens, or reserved tokens in a field whose layout declares TEXT are decode
errors rather than literals. The decoder exists only as an internal test helper
to prove the documented wire grammar; litefind ships no decode command or Go
API. The encoder contract remains public CLI behavior. A future incompatible
schema requires a new output flag rather than silently changing `--tsv`. TSV is
intended for Unix text tools, not spreadsheet import, and does not neutralize
formula-leading characters such as `=`, `+`, `-`, or `@`.

TSV changes rendering only. Search scanning, FTS querying, and frequency
aggregation retain their existing work and memory behavior: `--limit` bounds
frequency rows emitted, not the grouping query; `-m` bounds matches per table;
full TSV values and rows are not buffered again by the formatter. Litefind adds
no signal handling: an `io.Writer` error that reaches Go propagates through the
existing output path, may leave earlier complete records on stdout, prints the
write error to stderr, and exits 2; an operating-system SIGPIPE may instead
terminate the process according to Go/platform behavior. Tests cover returned
writer errors, not a universal closed-pipe exit status. In TSV-capable search
and frequency modes, no matches produce no stdout and exit 1. The non-TSV `--tables` introspection exception is
unchanged: a successful inventory exits 0 even when it contains no user tables.
Usage, SQLite, and write errors remain human diagnostics on stderr and exit 2;
stderr is never encoded as TSV.

**Goals:** unambiguous line-oriented fields for Unix pipelines, complete match
values, ordered single-source row payloads, and unchanged exit/error semantics.
**Non-goals:** preserving every SQLite storage type (use JSON), spreadsheet-safe
CSV behavior, headers, arbitrary multi-source row schemas, or changing query
execution.

Acceptance requires: every documented record layout is covered by CLI tests;
text/control escapes decode to their original bytes; NULL and BLOB decode
without colliding with TEXT; row schemas are stable and discoverable through
`--schema`; declared mode conflicts and multi-source rows exit 2 before output;
no-match runs exit 1 with empty stdout; returned writer errors exit 2 while
platform SIGPIPE behavior remains unchanged; TSV never contains ANSI; and all
existing text and JSON behavior/tests remain unchanged.

Implementation proceeds test-first in reviewable increments:

1. argument acceptance and conflicts, including `-l --count`, summary/row,
   `--row --all-tables`, and unchanged no-match/error diagnostics;
2. preserve ordered row values beside the existing JSON map, validate
   single-source and external-content source resolution before output, and test
   ordinary/generated/virtual-hidden plus shadow-table policy against the
   existing schema JSON projection;
3. serializer/decoder round-trip tests for valid and invalid UTF-8 TEXT, every
   control byte, trailing-row NULL, BLOB with identical bytes, integer/REAL edge
   cases, and composite text/BLOB primary keys;
4. regular match output and ordered row payloads, including tabs/newlines,
   multi-table rejection, color suppression, and returned-writer-error
   propagation without changing platform SIGPIPE handling;
5. FTS output and explicit catalog-derived row projection, including direct and
   external-content targets whose virtual/source schemas differ,
   missing/renamed-source-column failure, schema projection parity, plus color
   and write-error paths;
6. frequency, `-l`, and `--count` integration, with exact field counts and
   fixtures proving NULL/BLOB exclusion plus cast-string grouping and binary
   tie ordering across mixed INTEGER/REAL/TEXT storage classes;
7. usage text, README, `docs/design.md`, and embedded agent skill updates;
8. run a final end-to-end compatibility matrix across every record layout,
   declared conflict, and exit code; verify normal piped consumption succeeds
   and returned writer errors exit 2 without asserting a universal closed-pipe
   status; then run full project verification.

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
