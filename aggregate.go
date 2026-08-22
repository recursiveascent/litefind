package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type aggregateResult struct {
	avg, sum, min, max any
	count              int64
}

var (
	sqlMatchers     sync.Map
	sqlMatcherToken atomic.Uint64
)

func addSQLMatcher(re *regexp.Regexp) (string, func()) {
	token := strconv.FormatUint(sqlMatcherToken.Add(1), 10)
	sqlMatchers.Store(token, re)
	return token, func() { sqlMatchers.Delete(token) }
}

func sqliteMatch(token, value string) (bool, error) {
	v, ok := sqlMatchers.Load(token)
	if !ok {
		return false, fmt.Errorf("unknown litefind matcher token")
	}
	return v.(*regexp.Regexp).MatchString(value), nil
}

type affinity string

const (
	affinityInteger affinity = "INTEGER"
	affinityText    affinity = "TEXT"
	affinityBlob    affinity = "BLOB"
	affinityReal    affinity = "REAL"
	affinityNumeric affinity = "NUMERIC"
)

func columnAffinity(declType string) affinity {
	t := asciiFold(declType)
	switch {
	case strings.Contains(t, "int"):
		return affinityInteger
	case strings.Contains(t, "char"), strings.Contains(t, "clob"), strings.Contains(t, "text"):
		return affinityText
	case t == "" || strings.Contains(t, "blob"):
		return affinityBlob
	case strings.Contains(t, "real"), strings.Contains(t, "floa"), strings.Contains(t, "doub"):
		return affinityReal
	default:
		return affinityNumeric
	}
}

func aggregateQuery(target columnTarget, kind string, patterned bool) string {
	column := quoteIdent(target.column)
	where := `typeof(` + column + `) IN ('integer','real')`
	if patterned {
		where = `CASE WHEN ` + where + ` THEN litefind_match(?, CAST(` + column + ` AS TEXT)) ELSE 0 END`
	}

	var selectList string
	if kind == "stats" {
		selectList = `AVG(` + column + `), SUM(` + column + `), MIN(` + column + `), MAX(` + column + `), COUNT(` + column + `)`
	} else {
		selectList = strings.ToUpper(kind) + `(` + column + `), COUNT(` + column + `)`
	}
	return `SELECT ` + selectList + ` FROM ` + quoteIdent(target.table) + ` WHERE ` + where
}

func readAggregate(db *database, target columnTarget, kind string, matcher *regexp.Regexp) (aggregateResult, error) {
	var args []any
	if matcher != nil {
		token, remove := addSQLMatcher(matcher)
		defer remove()
		args = append(args, token)
	}

	row := db.sql.QueryRow(aggregateQuery(target, kind, matcher != nil), args...)
	var result aggregateResult
	if kind == "stats" {
		if err := row.Scan(&result.avg, &result.sum, &result.min, &result.max, &result.count); err != nil {
			return aggregateResult{}, err
		}
		return result, validateAggregateResult(kind, result)
	}

	var value any
	if err := row.Scan(&value, &result.count); err != nil {
		return aggregateResult{}, err
	}
	switch kind {
	case "avg":
		result.avg = value
	case "sum":
		result.sum = value
	case "min":
		result.min = value
	case "max":
		result.max = value
	}
	return result, validateAggregateResult(kind, result)
}

func validateAggregateResult(kind string, result aggregateResult) error {
	if result.count == 0 {
		return nil
	}
	values := []any{result.avg, result.sum, result.min, result.max}
	if kind != "stats" {
		values = values[map[string]int{"avg": 0, "sum": 1, "min": 2, "max": 3}[kind]:][:1]
	}
	for _, value := range values {
		switch n := value.(type) {
		case int64:
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return fmt.Errorf("aggregate result is not finite")
			}
		default:
			return fmt.Errorf("aggregate result is not a number")
		}
	}
	return nil
}

func aggregateValue(kind string, result aggregateResult) any {
	switch kind {
	case "avg":
		return result.avg
	case "sum":
		return result.sum
	case "min":
		return result.min
	default:
		return result.max
	}
}

func writeAggregate(w io.Writer, target columnTarget, kind string, result aggregateResult, jsonOut bool) error {
	if jsonOut {
		if kind == "stats" {
			return json.NewEncoder(w).Encode(struct {
				Table  string `json:"table"`
				Column string `json:"column"`
				Avg    any    `json:"avg"`
				Sum    any    `json:"sum"`
				Min    any    `json:"min"`
				Max    any    `json:"max"`
				Count  int64  `json:"count"`
			}{target.table, target.column, result.avg, result.sum, result.min, result.max, result.count})
		}
		return json.NewEncoder(w).Encode(map[string]any{
			"table": target.table, "column": target.column, kind: aggregateValue(kind, result), "count": result.count,
		})
	}

	if kind == "stats" {
		_, err := fmt.Fprintf(w, "%s.%s: avg=%v sum=%v min=%v max=%v count=%d\n",
			target.table, target.column, result.avg, result.sum, result.min, result.max, result.count)
		return err
	}
	_, err := fmt.Fprintf(w, "%s.%s %s: %v\n", target.table, target.column, kind, aggregateValue(kind, result))
	return err
}

func cmdAggregate(db *database, inv *invocation, stdout, stderr io.Writer) int {
	o := inv.opts
	if err := validateGlobs(o); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	cat, err := db.catalog()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	target, err := resolveColumnTarget("agg", cat, o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	if a := columnAffinity(target.declType); a == affinityText || a == affinityBlob {
		_, _ = fmt.Fprintf(stderr, "%s.%s has %s affinity, not numeric\n", target.table, target.column, a)
		return exitError
	}

	var matcher *regexp.Regexp
	if inv.hasPattern {
		matcher, err = buildMatcher(inv.pattern, o)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return exitError
		}
	}
	result, err := readAggregate(db, target, o.aggregate, matcher)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	if result.count == 0 {
		return exitNoMatch
	}
	if err := writeAggregate(stdout, target, o.aggregate, result, o.jsonOut); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	return exitMatch
}
