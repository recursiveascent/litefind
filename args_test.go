package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestReorderArgs(t *testing.T) {
	vf := map[string]bool{"t": true, "c": true, "m": true, "fts": true, "max-columns": true, "limit": true}
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
		{"frequency limit keeps its value", []string{"app.db", "--freq", "--limit", "7", "-c", "level"},
			[]string{"--freq", "--limit", "7", "-c", "level", "app.db"}},
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

	inv, err = parseInvocation([]string{"--tables", "app.db", "--no-counts"})
	if err != nil || inv.sub != "tables" || !inv.opts.noCounts {
		t.Fatalf("tables parse: %+v, %v", inv, err)
	}

	// Flags may appear after the database path.
	inv, err = parseInvocation([]string{"app.db", "--tables", "--json"})
	if err != nil || inv.sub != "tables" || !inv.opts.jsonOut {
		t.Fatalf("trailing mode flag parse: %+v, %v", inv, err)
	}

	// "--" makes a leading-dash pattern searchable.
	inv, err = parseInvocation([]string{"--", "-t", "app.db"})
	if err != nil || inv.sub != "search" || inv.pattern != "-t" {
		t.Fatalf("dash pattern parse: %+v, %v", inv, err)
	}

	inv, err = parseInvocation([]string{"--schema", "app.db", "ev*"})
	if err != nil || inv.sub != "schema" || inv.glob != "ev*" {
		t.Fatalf("schema parse: %+v, %v", inv, err)
	}

	for _, pattern := range []string{"tables", "schema", "quickstart"} {
		inv, err = parseInvocation([]string{pattern, "app.db"})
		if err != nil || inv.sub != "search" || inv.pattern != pattern {
			t.Errorf("pattern %q parse: %+v, %v", pattern, inv, err)
		}
	}

	// --fts replaces PATTERN: no positional pattern expected.
	inv, err = parseInvocation([]string{"--fts", "cats NEAR dogs", "app.db"})
	if err != nil || inv.pattern != "" || inv.opts.fts == "" {
		t.Fatalf("fts parse: %+v, %v", inv, err)
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
	// Flags not allowed for a mode should error, naming the flag as
	// it is actually spelled: one dash for a single-character flag, two
	// for a long one. Where a flag has both spellings the canonical short
	// one is reported (--ignore-case is named -i).
	for _, bad := range []struct {
		argv []string
		flag string
	}{
		{[]string{"--tables", "app.db", "-i"}, "-i"},
		{[]string{"--tables", "app.db", "--ignore-case"}, "-i"},
		{[]string{"--schema", "app.db", "--no-counts"}, "--no-counts"},
		{[]string{"pattern", "app.db", "--no-counts"}, "--no-counts"},
		{[]string{"--tables", "app.db", "--max-columns", "80"}, "--max-columns"},
		{[]string{"--schema", "app.db", "--all-tables"}, "--all-tables"},
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

func TestParseInvocationTSV(t *testing.T) {
	inv, err := parseInvocation([]string{"timeout", "app.db", "--tsv"})
	if err != nil {
		t.Fatal(err)
	}
	if !inv.opts.tsvOut {
		t.Fatalf("opts = %+v, want tsvOut", inv.opts)
	}

	inv, err = parseInvocation([]string{"--freq", "-c", "level", "app.db", "--tsv"})
	if err != nil || !inv.opts.tsvOut {
		t.Fatalf("freq TSV parse: %+v, %v", inv, err)
	}
}

func TestParseInvocationTSVConflicts(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"needle", "app.db", "--tsv", "--json"}, "--json"},
		{[]string{"needle", "app.db", "--tsv", "--stats"}, "--stats"},
		{[]string{"needle", "app.db", "--tsv", "-l", "--count"}, "--count"},
		{[]string{"needle", "app.db", "--tsv", "--row", "-l"}, "--row"},
		{[]string{"needle", "app.db", "--tsv", "--row", "--count"}, "--row"},
		{[]string{"needle", "app.db", "--tsv", "--row", "--all-tables"}, "--all-tables"},
		{[]string{"--freq", "-c", "level", "app.db", "--tsv", "--json"}, "--json"},
		{[]string{"--tables", "app.db", "--tsv"}, "--tsv"},
		{[]string{"--schema", "app.db", "--tsv"}, "--tsv"},
	} {
		_, err := parseInvocation(tc.argv)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseInvocation(%v) error = %v, want substring %q", tc.argv, err, tc.want)
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

func TestParseInvocationRejectsMultipleModes(t *testing.T) {
	_, err := parseInvocation([]string{"--tables", "--schema", "app.db"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "--tables") || !strings.Contains(err.Error(), "--schema") {
		t.Errorf("error %q should name both mode flags", err)
	}
}

func TestParseInvocationFreq(t *testing.T) {
	inv, err := parseInvocation([]string{"--freq", "-t", "events", "-c", "level", "app.db"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.sub != "freq" || inv.dbPath != "app.db" || inv.hasPattern {
		t.Fatalf("freq without pattern: %+v", inv)
	}
	if inv.opts.limit != 20 || !reflect.DeepEqual(inv.opts.tables, []string{"events"}) ||
		!reflect.DeepEqual(inv.opts.columns, []string{"level"}) {
		t.Errorf("freq opts = %+v", inv.opts)
	}

	inv, err = parseInvocation([]string{"--freq", "-c", "level", "--limit", "0", "", "app.db"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.sub != "freq" || !inv.hasPattern || inv.pattern != "" || inv.dbPath != "app.db" || inv.opts.limit != 0 {
		t.Fatalf("freq with empty pattern: %+v", inv)
	}
}

func TestParseInvocationFreqValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"missing column", []string{"--freq", "app.db"}, "exactly one column"},
		{"missing db", []string{"--freq", "-c", "level"}, "usage"},
		{"too many positionals", []string{"--freq", "-c", "level", "a", "b", "c"}, "usage"},
		{"negative limit", []string{"--freq", "-c", "level", "--limit", "-1", "app.db"}, "non-negative"},
		{"matcher without pattern", []string{"--freq", "-c", "level", "-i", "app.db"}, "requires a frequency pattern"},
		{"search output flag", []string{"--freq", "-c", "level", "--row", "app.db"}, "--row"},
		{"search count flag", []string{"--freq", "-c", "level", "-m", "2", "app.db"}, "-m"},
		{"truncation flag", []string{"--freq", "-c", "level", "--max-columns", "10", "app.db"}, "--max-columns"},
		{"limit outside freq", []string{"needle", "app.db", "--limit", "2"}, "--limit"},
		{"freq and tables", []string{"--freq", "--tables", "-c", "level", "app.db"}, "cannot be combined"},
		{"freq and schema", []string{"--freq", "--schema", "-c", "level", "app.db"}, "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseInvocation(tc.argv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseInvocation(%v) error = %v, want substring %q", tc.argv, err, tc.want)
			}
		})
	}
}

func TestParseInvocationHead(t *testing.T) {
	for _, argv := range [][]string{
		{"needle", "app.db", "--head", "5"},
		{"--head=5", "needle", "app.db"},
		{"--fts", "needle", "--head", "5", "app.db"},
	} {
		inv, err := parseInvocation(argv)
		if err != nil {
			t.Fatalf("parseInvocation(%v): %v", argv, err)
		}
		if !inv.opts.headSet || inv.opts.headLines != 5 {
			t.Fatalf("parseInvocation(%v) opts = %+v, want explicit head 5", argv, inv.opts)
		}
	}

	inv, err := parseInvocation([]string{"needle", "app.db"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.opts.headSet || inv.opts.headLines != 0 || inv.opts.maxColumns != 200 {
		t.Fatalf("default opts = %+v", inv.opts)
	}
}

func TestParseInvocationHeadValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"negative", []string{"needle", "app.db", "--head", "-1"}, "non-negative"},
		{"explicit truncation conflict", []string{"needle", "app.db", "--head", "0", "--max-columns", "0"}, "cannot be combined"},
		{"tsv conflict", []string{"needle", "app.db", "--head", "1", "--tsv"}, "--tsv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseInvocation(tc.argv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseInvocation(%v) error = %v, want substring %q", tc.argv, err, tc.want)
			}
		})
	}
}
