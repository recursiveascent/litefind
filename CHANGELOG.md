# Changelog

## Unreleased

- Added `--tsv` output for search, FTS, frequency, `-l`, and `--count`, with
  escaped one-record-per-line values and schema-ordered `--row` fields.

## 0.1.1

- Introspection is now selected by flags: `--tables` and `--schema` replace the
  `tables` and `schema` subcommands. Without either flag, the first positional is
  always the search pattern — no more disambiguation tricks like `[t]ables`.
- Removed the `quickstart` subcommand.
- Added `--version` / `-V` to print `litefind <version>` and exit.
- Added `--skill` to help with installing and maintaining agent skills for the
  tool.
- Windows support: release archives for windows/amd64 and arm64 (zip), and
  backslash paths now round-trip through the SQLite URI parser.
- Expanded install and usage documentation (Homebrew, Nix, release binaries,
  building from source, version verification).

## 0.1.0

- Initial version.
