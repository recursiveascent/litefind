package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestResolveFreqTarget(t *testing.T) {
	cat := []tableInfo{
		{name: "events", kind: "table", cols: []colInfo{{name: "level"}, {name: "message"}}},
		{name: "logs", kind: "table", cols: []colInfo{{name: "level"}}},
		{name: "shadow_t", kind: "table", shadow: true, cols: []colInfo{{name: "value"}}},
		{name: "v_events", kind: "view", cols: []colInfo{{name: "level"}}},
	}

	t.Run("one qualified column", func(t *testing.T) {
		got, err := resolveFreqTarget(cat, searchOpts{columns: []string{"events.level"}})
		if err != nil {
			t.Fatal(err)
		}
		if got != (freqTarget{table: "events", column: "level"}) {
			t.Errorf("target = %+v", got)
		}
	})

	t.Run("duplicate specs deduplicate", func(t *testing.T) {
		got, err := resolveFreqTarget(cat, searchOpts{
			tables:  []string{"events"},
			columns: []string{"level", "events.level"},
		})
		if err != nil || got.table != "events" || got.column != "level" {
			t.Fatalf("target = %+v, err = %v", got, err)
		}
	})

	t.Run("no columns", func(t *testing.T) {
		_, err := resolveFreqTarget(cat, searchOpts{columns: []string{"missing"}})
		if err == nil || !strings.Contains(err.Error(), "matched no columns") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("multiple columns list pairs", func(t *testing.T) {
		_, err := resolveFreqTarget(cat, searchOpts{columns: []string{"level"}})
		if err == nil || !strings.Contains(err.Error(), "events.level, logs.level") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("table exclusion narrows target", func(t *testing.T) {
		got, err := resolveFreqTarget(cat, searchOpts{
			notTables: []string{"logs"},
			columns:   []string{"level"},
		})
		if err != nil || got.table != "events" {
			t.Fatalf("target = %+v, err = %v", got, err)
		}
	})

	t.Run("views and shadows excluded by default", func(t *testing.T) {
		for _, column := range []string{"v_events.level", "shadow_t.value"} {
			if _, err := resolveFreqTarget(cat, searchOpts{columns: []string{column}}); err == nil ||
				!strings.Contains(err.Error(), "matched no columns") {
				t.Fatalf("column %q error = %v", column, err)
			}
		}
	})

	t.Run("all tables admits shadow", func(t *testing.T) {
		got, err := resolveFreqTarget(cat, searchOpts{allTables: true, columns: []string{"shadow_t.value"}})
		if err != nil || got != (freqTarget{table: "shadow_t", column: "value"}) {
			t.Fatalf("target = %+v, err = %v", got, err)
		}
	})
}

func freqFixturePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "freq.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, stmt := range []string{
		`CREATE TABLE values_t (value)`,
		`INSERT INTO values_t VALUES
			('error'), ('error'), ('error'),
			('warn'), ('warn'), ('info'),
			(1), ('1'), (1.0), ('1.0'),
			(NULL), (x'626c6f62'), (''),
			('line' || char(10) || 'break'), ('tab' || char(9) || 'value')`,
		`CREATE TABLE nocase_t (value TEXT COLLATE NOCASE)`,
		`INSERT INTO nocase_t VALUES ('A'), ('a')`,
		`CREATE TABLE many_t (value TEXT)`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x + 1 FROM n WHERE x < 21)
		 INSERT INTO many_t SELECT printf('v%02d', x) FROM n`,
		`CREATE TABLE noid_t ("rowid" TEXT, "_rowid_" TEXT, "oid" TEXT, value TEXT)`,
		`INSERT INTO noid_t VALUES ('r', 'u', 'o', 'usable')`,
		`CREATE VIEW values_v AS SELECT value FROM values_t`,
		`CREATE VIRTUAL TABLE freq_fts USING fts5(value)`,
		`INSERT INTO freq_fts(value) VALUES ('fts'), ('fts')`,
		`CREATE TABLE patterns_t (value TEXT)`,
		`INSERT INTO patterns_t VALUES
			('hot'), ('hot'), ('hot'), ('hot'), ('hot'),
			('match-b'), ('match-b'), ('match-b'),
			('match-a'), ('match-a'),
			('Alpha'), ('alpha'), ('alphabet'), ('alpha beta'), ('a.b')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture statement failed: %v\n%s", err, stmt)
		}
	}
	return path
}

func TestFreqTextGroupingAndEscaping(t *testing.T) {
	out, stderr, code := runCmd(t, "--freq", "-t", "values_t", "-c", "value", "--limit", "0", freqFixturePath(t))
	if code != exitMatch || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	want := "error\t3\n" +
		"1\t2\n" +
		"1.0\t2\n" +
		"warn\t2\n" +
		"\t1\n" +
		"info\t1\n" +
		`line\nbreak` + "\t1\n" +
		`tab\tvalue` + "\t1\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if strings.Contains(out, "blob\t") {
		t.Errorf("BLOB value must be excluded:\n%s", out)
	}
}

func TestFreqJSON(t *testing.T) {
	out, stderr, code := runCmd(t, "--freq", "--json", "-t", "values_t", "-c", "value", "--limit", "1", freqFixturePath(t))
	if code != exitMatch || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got struct {
		Table  string `json:"table"`
		Column string `json:"column"`
		Value  string `json:"value"`
		Count  int64  `json:"count"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Table != "values_t" || got.Column != "value" || got.Value != "error" || got.Count != 3 {
		t.Errorf("result = %+v", got)
	}
}

func TestFreqBinaryGroupingOverridesDeclaredCollation(t *testing.T) {
	out, _, code := runCmd(t, "--freq", "-t", "nocase_t", "-c", "value", "--limit", "0", freqFixturePath(t))
	if code != exitMatch || out != "A\t1\na\t1\n" {
		t.Fatalf("exit = %d, output = %q", code, out)
	}
}

func TestFreqDoesNotRequireStableIdentity(t *testing.T) {
	out, stderr, code := runCmd(t, "--freq", "-t", "noid_t", "-c", "value", freqFixturePath(t))
	if code != exitMatch || stderr != "" || out != "usable\t1\n" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
}

func TestFreqDefaultLimit(t *testing.T) {
	out, _, code := runCmd(t, "--freq", "-t", "many_t", "-c", "value", freqFixturePath(t))
	if code != exitMatch {
		t.Fatal(code)
	}
	if lines := strings.Count(out, "\n"); lines != 20 {
		t.Fatalf("lines = %d, want default limit 20:\n%s", lines, out)
	}
	if strings.Contains(out, "v21\t1") {
		t.Fatalf("default limit emitted v21:\n%s", out)
	}
}

func TestFreqLimitZeroIsUnlimited(t *testing.T) {
	out, _, code := runCmd(t, "--freq", "-t", "values_t", "-c", "value", "--limit", "0", freqFixturePath(t))
	if code != exitMatch {
		t.Fatal(code)
	}
	if lines := strings.Count(out, "\n"); lines != 8 {
		t.Fatalf("lines = %d, want 8:\n%s", lines, out)
	}
}

func TestFreqPatternLimitAppliesAfterFiltering(t *testing.T) {
	out, stderr, code := runCmd(t,
		"--freq", "-t", "patterns_t", "-c", "value", "--limit", "1", "^match", freqFixturePath(t))
	if code != exitMatch || stderr != "" || out != "match-b\t3\n" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
}

func TestFreqPatternUsesSearchMatcherSemantics(t *testing.T) {
	path := freqFixturePath(t)
	for _, tc := range []struct {
		name string
		argv []string
		want []string
		not  []string
	}{
		{"regex", []string{"^match-[ab]$"}, []string{"match-b\t3", "match-a\t2"}, []string{"hot\t5"}},
		{"fixed", []string{"-F", "a.b"}, []string{"a.b\t1"}, []string{"alpha\t1"}},
		{"ignore case", []string{"-i", "^alpha$"}, []string{"Alpha\t1", "alpha\t1"}, []string{"alphabet\t1"}},
		{"smart lower", []string{"-S", "^alpha$"}, []string{"Alpha\t1", "alpha\t1"}, []string{"alphabet\t1"}},
		{"smart uppercase", []string{"-S", "^Alpha$"}, []string{"Alpha\t1"}, []string{"alpha\t1"}},
		{"word", []string{"-w", "alpha"}, []string{"alpha\t1", "alpha beta\t1"}, []string{"alphabet\t1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := []string{"--freq", "-t", "patterns_t", "-c", "value", "--limit", "0"}
			argv = append(argv, tc.argv...)
			argv = append(argv, path)
			out, stderr, code := runCmd(t, argv...)
			if code != exitMatch || stderr != "" {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tc.not {
				if strings.Contains(out, unwanted) {
					t.Errorf("unexpected %q:\n%s", unwanted, out)
				}
			}
		})
	}
}

func TestFreqExplicitEmptyPattern(t *testing.T) {
	out, stderr, code := runCmd(t,
		"--freq", "-t", "patterns_t", "-c", "value", "--limit", "1", "", freqFixturePath(t))
	if code != exitMatch || stderr != "" || out != "hot\t5\n" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
}

func TestFreqPatternNoMatch(t *testing.T) {
	out, stderr, code := runCmd(t,
		"--freq", "-t", "patterns_t", "-c", "value", "nomatch", freqFixturePath(t))
	if code != exitNoMatch || out != "" || stderr != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
}

func TestFreqInvalidRegex(t *testing.T) {
	_, stderr, code := runCmd(t,
		"--freq", "-t", "patterns_t", "-c", "value", "([", freqFixturePath(t))
	if code != exitError || !strings.Contains(stderr, `invalid regex "(["`) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestFreqTargetErrors(t *testing.T) {
	path := freqFixturePath(t)
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"none", []string{"--freq", "-t", "values_t", "-c", "missing", path}, "matched no columns"},
		{"multiple", []string{"--freq", "-c", "value", path}, "requires exactly one column"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runCmd(t, tc.argv...)
			if code != exitError || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
		})
	}
}

func TestFreqStdoutWriteErrorExitsError(t *testing.T) {
	var stderr strings.Builder
	code := run([]string{"--freq", "-t", "values_t", "-c", "value", freqFixturePath(t)}, failWriter{}, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "simulated write failure") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
}

func TestFreqFTS5Table(t *testing.T) {
	out, stderr, code := runCmd(t, "--freq", "-t", "freq_fts", "-c", "value", freqFixturePath(t))
	if code != exitMatch || stderr != "" || out != "fts\t2\n" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
}
