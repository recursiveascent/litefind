package main

import (
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func aggregateFixturePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aggregate.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, stmt := range []string{
		`CREATE TABLE metrics (
			integer_value INTEGER,
			real_value REAL,
			numeric_value DECIMAL,
			text_value TEXT,
			blob_value,
			charint_value CHARINT,
			floating_point_value "FLOATING POINT",
			varchar_value VARCHAR(20),
			double_value DOUBLE,
			boolean_value BOOLEAN,
			none_value NONE
		)`,
		`INSERT INTO metrics(integer_value, real_value, numeric_value) VALUES
			(1, 1.5, 10),
			(2, 2.5, 20),
			(3, NULL, 21),
			(NULL, NULL, NULL),
			('bad', NULL, NULL),
			(x'01', NULL, NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture statement failed: %v\n%s", err, stmt)
		}
	}
	return path
}

func TestAggregateStatsText(t *testing.T) {
	out, stderr, code := runCmd(t, "--agg", "stats", "-t", "metrics", "-c", "integer_value", aggregateFixturePath(t))
	if code != exitMatch || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if want := "metrics.integer_value: avg=2 sum=6 min=1 max=3 count=3\n"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestAggregateKindsAndJSON(t *testing.T) {
	path := aggregateFixturePath(t)
	for _, tc := range []struct {
		kind string
		want string
	}{
		{"avg", "metrics.real_value avg: 2\n"},
		{"sum", "metrics.real_value sum: 4\n"},
		{"min", "metrics.real_value min: 1.5\n"},
		{"max", "metrics.real_value max: 2.5\n"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			out, stderr, code := runCmd(t, "--agg", tc.kind, "-t", "metrics", "-c", "real_value", path)
			if code != exitMatch || stderr != "" || out != tc.want {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
			}
		})
	}

	out, stderr, code := runCmd(t, "--agg", "stats", "--json", "-t", "metrics", "-c", "real_value", path)
	if code != exitMatch || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got struct {
		Table  string  `json:"table"`
		Column string  `json:"column"`
		Avg    float64 `json:"avg"`
		Sum    float64 `json:"sum"`
		Min    float64 `json:"min"`
		Max    float64 `json:"max"`
		Count  int64   `json:"count"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Table != "metrics" || got.Column != "real_value" || got.Avg != 2 || got.Sum != 4 || got.Min != 1.5 || got.Max != 2.5 || got.Count != 2 {
		t.Fatalf("result = %+v", got)
	}
}

func TestAggregatePatternFiltersTargetValues(t *testing.T) {
	out, stderr, code := runCmd(t, "--agg", "stats", "-t", "metrics", "-c", "numeric_value", "^2", aggregateFixturePath(t))
	if code != exitMatch || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if want := "metrics.numeric_value: avg=20.5 sum=41 min=20 max=21 count=2\n"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestAggregateAffinity(t *testing.T) {
	path := aggregateFixturePath(t)
	for _, column := range []string{"integer_value", "real_value", "numeric_value", "charint_value", "floating_point_value", "double_value", "boolean_value", "none_value"} {
		t.Run("accept "+column, func(t *testing.T) {
			_, stderr, code := runCmd(t, "--agg", "stats", "-t", "metrics", "-c", column, path)
			if code == exitError {
				t.Fatalf("column %s rejected: %s", column, stderr)
			}
		})
	}
	for _, column := range []string{"text_value", "blob_value", "varchar_value"} {
		t.Run("reject "+column, func(t *testing.T) {
			out, stderr, code := runCmd(t, "--agg", "stats", "-t", "metrics", "-c", column, path)
			if code != exitError || out != "" || !strings.Contains(stderr, "affinity") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
			}
		})
	}
}

func TestAggregateNoNumericValuesExitsNoMatch(t *testing.T) {
	out, stderr, code := runCmd(t, "--agg", "stats", "-t", "metrics", "-c", "boolean_value", aggregateFixturePath(t))
	if code != exitNoMatch || out != "" || stderr != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
}

func TestAggregateTargetErrors(t *testing.T) {
	path := aggregateFixturePath(t)
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"none", []string{"--agg", "stats", "-t", "metrics", "-c", "missing", path}, "matched no columns"},
		{"multiple", []string{"--agg", "stats", "-t", "metrics", "-c", "*_value", path}, "requires exactly one column"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr, code := runCmd(t, tc.argv...)
			if code != exitError || out != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
			}
		})
	}
}

func TestAggregatePatternMatcherSemantics(t *testing.T) {
	path := aggregateFixturePath(t)
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"regex", []string{"."}, "metrics.integer_value sum: 6\n"},
		{"fixed", []string{"-F", "."}, ""},
		{"word", []string{"-w", "2"}, "metrics.integer_value sum: 2\n"},
		{"empty", []string{""}, "metrics.integer_value sum: 6\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := []string{"--agg", "sum", "-t", "metrics", "-c", "integer_value"}
			argv = append(argv, tc.argv...)
			argv = append(argv, path)
			out, stderr, code := runCmd(t, argv...)
			wantCode := exitMatch
			if tc.want == "" {
				wantCode = exitNoMatch
			}
			if code != wantCode || out != tc.want || stderr != "" {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
			}
		})
	}

	out, stderr, code := runCmd(t, "--agg", "sum", "-t", "metrics", "-c", "integer_value", "([", path)
	if code != exitError || out != "" || !strings.Contains(stderr, "invalid regex") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
}

func TestAggregateIndividualJSON(t *testing.T) {
	out, stderr, code := runCmd(t, "--agg", "sum", "--json", "-t", "metrics", "-c", "integer_value", aggregateFixturePath(t))
	if code != exitMatch || stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got struct {
		Table  string `json:"table"`
		Column string `json:"column"`
		Sum    int64  `json:"sum"`
		Count  int64  `json:"count"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Table != "metrics" || got.Column != "integer_value" || got.Sum != 6 || got.Count != 3 {
		t.Fatalf("result = %+v", got)
	}
}

func TestAggregateSumOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overflow.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE metrics(value INTEGER); INSERT INTO metrics VALUES (9223372036854775807), (1)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"sum", "stats"} {
		t.Run(kind, func(t *testing.T) {
			out, stderr, code := runCmd(t, "--agg", kind, "-t", "metrics", "-c", "value", path)
			if code != exitError || out != "" || !strings.Contains(stderr, "overflow") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
			}
		})
	}
}

func TestAggregateStdoutWriteErrorExitsError(t *testing.T) {
	var stderr strings.Builder
	code := run([]string{"--agg", "stats", "-t", "metrics", "-c", "integer_value", aggregateFixturePath(t)}, failWriter{}, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "simulated write failure") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
}

func TestAggregateRejectsNonFiniteResult(t *testing.T) {
	path := aggregateFixturePath(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE metrics SET real_value = ? WHERE integer_value = 1`, math.Inf(1)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := runCmd(t, "--agg", "max", "-t", "metrics", "-c", "real_value", path)
	if code != exitError || out != "" || !strings.Contains(stderr, "not finite") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
}
