package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestReorderArgs(t *testing.T) {
	vf := map[string]bool{"t": true, "m": true, "fts": true, "max-columns": true}
	cases := []struct {
		name     string
		in, want []string
	}{
		{"interspersed", []string{"timeout", "app.db", "-t", "events", "--json"},
			[]string{"-t", "events", "--json", "timeout", "app.db"}},
		{"already ordered", []string{"-F", "x", "app.db"}, []string{"-F", "x", "app.db"}},
		// "--" must survive reordering so post-"--" args that start with
		// "-" stay positional when stdlib flag parses the result.
		{"double dash preserved", []string{"--", "-t", "app.db"}, []string{"--", "-t", "app.db"}},
		{"flags hoisted before double dash", []string{"-i", "a.db", "--", "-x"},
			[]string{"-i", "--", "a.db", "-x"}},
		{"value flag keeps its value", []string{"a.db", "--fts", "cats NEAR dogs"},
			[]string{"--fts", "cats NEAR dogs", "a.db"}},
		{"eq form needs no lookahead", []string{"a.db", "--max-columns=80"},
			[]string{"--max-columns=80", "a.db"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reorderArgs(c.in, vf); !reflect.DeepEqual(got, c.want) {
				t.Errorf("reorderArgs(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestParseInvocation(t *testing.T) {
	inv, err := parseInvocation([]string{"timeout", "app.db", "-t", "events", "-i", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.sub != "search" || inv.pattern != "timeout" || inv.dbPath != "app.db" {
		t.Errorf("got %+v", inv)
	}
	if !inv.opts.ignoreCase || !inv.opts.jsonOut || inv.opts.tables[0] != "events" {
		t.Errorf("opts = %+v", inv.opts)
	}

	inv, err = parseInvocation([]string{"tables", "app.db", "--no-counts"})
	if err != nil || inv.sub != "tables" || !inv.opts.noCounts {
		t.Fatalf("tables parse: %+v, %v", inv, err)
	}

	// Flags may precede the subcommand — it is recognized as the first
	// positional, not only at argv[0].
	inv, err = parseInvocation([]string{"--json", "tables", "app.db"})
	if err != nil || inv.sub != "tables" || !inv.opts.jsonOut {
		t.Fatalf("flags-before-subcommand parse: %+v, %v", inv, err)
	}

	// "--" makes a leading-dash pattern searchable.
	inv, err = parseInvocation([]string{"--", "-t", "app.db"})
	if err != nil || inv.sub != "search" || inv.pattern != "-t" {
		t.Fatalf("dash pattern parse: %+v, %v", inv, err)
	}

	inv, err = parseInvocation([]string{"schema", "app.db", "ev*"})
	if err != nil || inv.sub != "schema" || inv.glob != "ev*" {
		t.Fatalf("schema parse: %+v, %v", inv, err)
	}

	// --fts replaces PATTERN: no positional pattern expected.
	inv, err = parseInvocation([]string{"--fts", "cats NEAR dogs", "app.db"})
	if err != nil || inv.pattern != "" || inv.opts.fts == "" {
		t.Fatalf("fts parse: %+v, %v", inv, err)
	}

	// quickstart: no db, no flags.
	inv, err = parseInvocation([]string{"quickstart"})
	if err != nil || inv.sub != "quickstart" {
		t.Fatalf("quickstart parse: %+v, %v", inv, err)
	}
}

func TestParseInvocationFTSFlagConflicts(t *testing.T) {
	// Spec "Flag compatibility": -F, -i, -S, -w, -c, --all-tables are
	// usage errors with --fts.
	for _, bad := range [][]string{
		{"--fts", "q", "app.db", "-F"},
		{"--fts", "q", "app.db", "-i"},
		{"--fts", "q", "app.db", "-S"},
		{"--fts", "q", "app.db", "-w"},
		{"--fts", "q", "app.db", "-c", "events.msg"},
		{"--fts", "q", "app.db", "--all-tables"},
	} {
		if _, err := parseInvocation(bad); err == nil {
			t.Errorf("parseInvocation(%v): want error, got nil", bad)
		} else if !strings.Contains(err.Error(), "--fts") {
			t.Errorf("error %q should name --fts", err)
		}
	}
}

func TestParseInvocationInvalidFlags(t *testing.T) {
	// Flags not allowed for a subcommand should error, naming the flag as
	// it is actually spelled: one dash for a single-character flag, two
	// for a long one. Where a flag has both spellings the canonical short
	// one is reported (--ignore-case is named -i).
	for _, bad := range []struct {
		argv []string
		flag string
	}{
		{[]string{"tables", "app.db", "-i"}, "-i"},
		{[]string{"tables", "app.db", "--ignore-case"}, "-i"},
		{[]string{"schema", "app.db", "--no-counts"}, "--no-counts"},
		{[]string{"pattern", "app.db", "--no-counts"}, "--no-counts"},
		{[]string{"tables", "app.db", "--max-columns", "80"}, "--max-columns"},
		{[]string{"schema", "app.db", "--all-tables"}, "--all-tables"},
		{[]string{"quickstart", "--json"}, "--json"},
	} {
		_, err := parseInvocation(bad.argv)
		if err == nil {
			t.Errorf("parseInvocation(%v): want error, got nil", bad.argv)
			continue
		}
		if !strings.Contains(err.Error(), "not valid") {
			t.Errorf("error %q should indicate invalid flag", err)
		}
		if want := "flag " + bad.flag + " is not valid"; !strings.Contains(err.Error(), want) {
			t.Errorf("parseInvocation(%v) error = %q, want it to contain %q", bad.argv, err, want)
		}
	}
}

func TestParseInvocationEmptyFTSQuery(t *testing.T) {
	// --fts with empty query should error.
	for _, bad := range [][]string{
		{"--fts", "", "app.db"},
		{"--fts=", "app.db"},
	} {
		if _, err := parseInvocation(bad); err == nil {
			t.Errorf("parseInvocation(%v): want error for empty --fts, got nil", bad)
		} else if !strings.Contains(err.Error(), "non-empty") {
			t.Errorf("error %q should mention non-empty", err)
		}
	}
}

func TestParseInvocationQuickstartRejectsArgs(t *testing.T) {
	// quickstart takes no database and no extra positionals.
	for _, bad := range [][]string{
		{"quickstart", "extra"},
		{"quickstart", "events.db"},
	} {
		if _, err := parseInvocation(bad); err == nil {
			t.Errorf("parseInvocation(%v): want error, got nil", bad)
		}
	}
}
