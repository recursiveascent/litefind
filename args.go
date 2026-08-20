package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

type searchOpts struct {
	tables, notTables, columns                                             []string
	fixed, ignoreCase, smartCase, wordRegexp                               bool
	fts                                                                    string
	listTables, count, row, jsonOut, stats, allTables, immutable, noCounts bool
	maxCount                                                               int
	maxColumns                                                             int
}

type invocation struct {
	sub     string
	pattern string
	dbPath  string
	glob    string
	opts    searchOpts
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// valueFlags lists every flag that consumes a following argument, for
// reorderArgs. Keep in sync with the flag definitions below.
var valueFlags = map[string]bool{
	"t": true, "table": true, "T": true, "not-table": true,
	"c": true, "column": true, "fts": true, "m": true, "max-count": true,
	"max-columns": true,
}

func reorderArgs(argv []string, valueFlags map[string]bool) []string {
	var flags, pos []string
	sawDashDash := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			pos = append(pos, argv[i+1:]...)
			sawDashDash = true
			i = len(argv)
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if found := strings.Contains(name, "="); found {
				break // --flag=value carries its own value
			}
			if valueFlags[name] && i+1 < len(argv) {
				i++
				flags = append(flags, argv[i])
			}
		default:
			pos = append(pos, a)
		}
	}
	// Keep "--" ahead of every positional so stdlib flag never re-parses
	// a leading-dash positional as a flag.
	if sawDashDash {
		flags = append(flags, "--")
	}
	return append(flags, pos...)
}

// flagCanonical maps all flag names (short and long) to their canonical form.
var flagCanonical = map[string]string{
	"t": "t", "table": "t",
	"T": "T", "not-table": "T",
	"c": "c", "column": "c",
	"F": "F", "fixed-strings": "F",
	"i": "i", "ignore-case": "i",
	"S": "S", "smart-case": "S",
	"w": "w", "word-regexp": "w",
	"l": "l", "tables-with-matches": "l",
	"m": "m", "max-count": "m",
	"fts":         "fts",
	"count":       "count",
	"row":         "row",
	"json":        "json",
	"stats":       "stats",
	"max-columns": "max-columns",
	"all-tables":  "all-tables",
	"immutable":   "immutable",
	"no-counts":   "no-counts",
	"tables":      "tables",
	"schema":      "schema",
}

// allowedFlags defines which canonical flags are allowed for each mode.
var allowedFlags = map[string]map[string]bool{
	"search": {
		"t": true, "T": true, "c": true, "F": true, "i": true, "S": true,
		"w": true, "l": true, "m": true, "count": true, "row": true,
		"json": true, "stats": true, "max-columns": true, "all-tables": true,
		"immutable": true, "fts": true,
	},
	"tables": {
		"tables": true, "json": true, "no-counts": true, "all-tables": true, "immutable": true,
	},
	"schema": {
		"schema": true, "json": true, "immutable": true,
	},
}

// parseInvocation parses flags first, selects introspection when --tables or
// --schema was supplied, and otherwise treats the positionals as a search
// pattern and database path.
func parseInvocation(argv []string) (*invocation, error) {
	inv := &invocation{sub: "search"}

	fs := flag.NewFlagSet("litefind", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var tables, notTables, columns multiFlag
	fs.Var(&tables, "t", "")
	fs.Var(&tables, "table", "")
	fs.Var(&notTables, "T", "")
	fs.Var(&notTables, "not-table", "")
	fs.Var(&columns, "c", "")
	fs.Var(&columns, "column", "")
	fixed := fs.Bool("F", false, "")
	fs.BoolVar(fixed, "fixed-strings", false, "")
	ignoreCase := fs.Bool("i", false, "")
	fs.BoolVar(ignoreCase, "ignore-case", false, "")
	smartCase := fs.Bool("S", false, "")
	fs.BoolVar(smartCase, "smart-case", false, "")
	word := fs.Bool("w", false, "")
	fs.BoolVar(word, "word-regexp", false, "")
	list := fs.Bool("l", false, "")
	fs.BoolVar(list, "tables-with-matches", false, "")
	maxCount := fs.Int("m", 0, "")
	fs.IntVar(maxCount, "max-count", 0, "")
	fts := fs.String("fts", "", "")
	count := fs.Bool("count", false, "")
	row := fs.Bool("row", false, "")
	jsonOut := fs.Bool("json", false, "")
	stats := fs.Bool("stats", false, "")
	maxColumns := fs.Int("max-columns", 200, "")
	allTables := fs.Bool("all-tables", false, "")
	immutable := fs.Bool("immutable", false, "")
	noCounts := fs.Bool("no-counts", false, "")
	fs.Bool("tables", false, "")
	fs.Bool("schema", false, "")

	if err := fs.Parse(reorderArgs(argv, valueFlags)); err != nil {
		return nil, err
	}

	// Collect the set of flags that were explicitly supplied by the user.
	suppliedFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		canonical := flagCanonical[f.Name]
		suppliedFlags[canonical] = true
	})

	if suppliedFlags["tables"] && suppliedFlags["schema"] {
		return nil, fmt.Errorf("--tables and --schema cannot be combined")
	}

	// Check if --fts was supplied and validate it's not empty.
	if suppliedFlags["fts"] && *fts == "" {
		return nil, fmt.Errorf("--fts requires a non-empty query")
	}

	inv.opts = searchOpts{
		tables: tables, notTables: notTables, columns: columns,
		fixed: *fixed, ignoreCase: *ignoreCase, smartCase: *smartCase,
		wordRegexp: *word, fts: *fts, listTables: *list, count: *count,
		row: *row, jsonOut: *jsonOut, stats: *stats, allTables: *allTables,
		immutable: *immutable, noCounts: *noCounts,
		maxCount: *maxCount, maxColumns: *maxColumns,
	}

	pos := fs.Args()
	switch {
	case suppliedFlags["tables"]:
		inv.sub = "tables"
	case suppliedFlags["schema"]:
		inv.sub = "schema"
	}
	switch inv.sub {
	case "search":
		if inv.opts.fts != "" {
			if len(pos) != 1 {
				return nil, fmt.Errorf("--fts QUERY replaces PATTERN: usage: litefind --fts QUERY <db>")
			}
			inv.dbPath = pos[0]
			if err := checkFTSFlagCompat(inv.opts); err != nil {
				return nil, err
			}
		} else {
			if len(pos) != 2 {
				return nil, fmt.Errorf("usage: litefind PATTERN <db> [flags]")
			}
			inv.pattern, inv.dbPath = pos[0], pos[1]
		}
		// Validate search flags.
		if err := validateFlagsForMode(suppliedFlags, "search", inv.opts.fts != ""); err != nil {
			return nil, err
		}
	case "tables":
		if len(pos) != 1 {
			return nil, fmt.Errorf("usage: litefind --tables <db>")
		}
		inv.dbPath = pos[0]
		if err := validateFlagsForMode(suppliedFlags, "tables", false); err != nil {
			return nil, err
		}
	case "schema":
		if len(pos) < 1 || len(pos) > 2 {
			return nil, fmt.Errorf("usage: litefind --schema <db> [table-glob]")
		}
		inv.dbPath = pos[0]
		if len(pos) == 2 {
			inv.glob = pos[1]
		}
		if err := validateFlagsForMode(suppliedFlags, "schema", false); err != nil {
			return nil, err
		}
	}
	return inv, nil
}

// validateFlagsForMode checks that supplied flags are allowed for the
// given mode. For search with --fts, it allows a different set to exclude
// regex-only flags (checkFTSFlagCompat handles detailed validation).
func validateFlagsForMode(supplied map[string]bool, mode string, isFTS bool) error {
	allowed := allowedFlags[mode]
	// For search with --fts, remove regex-only flags from the allowed set.
	if mode == "search" && isFTS {
		// Allowed flags for search+fts: fts, t, T, l, m, count, row, json, stats, max-columns, immutable.
		allowed = map[string]bool{
			"fts": true, "t": true, "T": true, "l": true, "m": true,
			"count": true, "row": true, "json": true, "stats": true,
			"max-columns": true, "immutable": true,
		}
	}

	for flag := range supplied {
		if !allowed[flag] {
			return fmt.Errorf("flag %s is not valid for mode %q", formatFlagName(flag), mode)
		}
	}
	return nil
}

// formatFlagName renders a canonical flag name the way it is spelled on
// the command line: one dash for a single-character flag, two for a long
// one. The canonical name is the short spelling wherever a flag has one
// (--ignore-case is reported as -i), so the message names a flag the user
// can actually type, which "-max-columns" — neither spelling of anything
// — did not.
func formatFlagName(canonical string) string {
	if len(canonical) == 1 {
		return "-" + canonical
	}
	return "--" + canonical
}

// checkFTSFlagCompat enforces the spec's "Flag compatibility" section:
// regex-oriented flags cannot combine with --fts.
func checkFTSFlagCompat(o searchOpts) error {
	var bad []string
	if o.fixed {
		bad = append(bad, "-F")
	}
	if o.ignoreCase {
		bad = append(bad, "-i")
	}
	if o.smartCase {
		bad = append(bad, "-S")
	}
	if o.wordRegexp {
		bad = append(bad, "-w")
	}
	if len(o.columns) > 0 {
		bad = append(bad, "-c")
	}
	if o.allTables {
		bad = append(bad, "--all-tables")
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s cannot be combined with --fts: case and tokenization are governed by the FTS index; scope columns with FTS5 syntax, e.g. --fts '{col}: query'", strings.Join(bad, ", "))
	}
	return nil
}
