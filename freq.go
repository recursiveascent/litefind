package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type columnTarget struct {
	table, column, declType string
}

func resolveColumnTarget(mode string, cat []tableInfo, o searchOpts) (columnTarget, error) {
	if len(o.columns) == 0 {
		return columnTarget{}, fmt.Errorf("--%s requires exactly one column via -c", mode)
	}

	var targets []columnTarget
	for _, ti := range scopeTables(cat, o) {
		for _, column := range resolveColsForTable(o, ti) {
			for _, c := range ti.cols {
				if c.name == column {
					targets = append(targets, columnTarget{table: ti.name, column: c.name, declType: c.declType})
					break
				}
			}
		}
	}

	switch len(targets) {
	case 0:
		return columnTarget{}, fmt.Errorf("--%s column scope matched no columns", mode)
	case 1:
		return targets[0], nil
	default:
		names := make([]string, len(targets))
		for i, target := range targets {
			names[i] = target.table + "." + target.column
		}
		return columnTarget{}, fmt.Errorf("--%s requires exactly one column; matched %s", mode, strings.Join(names, ", "))
	}
}

func writeFrequency(w io.Writer, target columnTarget, value string, count int64, jsonOut, tsvOut bool) error {
	if jsonOut {
		return json.NewEncoder(w).Encode(struct {
			Table  string `json:"table"`
			Column string `json:"column"`
			Value  string `json:"value"`
			Count  int64  `json:"count"`
		}{
			Table: target.table, Column: target.column, Value: value, Count: count,
		})
	}

	if tsvOut {
		return writeTSVRecord(w,
			tsvTextField(target.table),
			tsvTextField(target.column),
			tsvTextField(value),
			fmt.Sprint(count),
		)
	}

	p := printer{}
	_, err := fmt.Fprintf(w, "%s\t%d\n", p.renderUnbounded(value, nil), count)
	return err
}

func cmdFreq(db *database, inv *invocation, stdout, stderr io.Writer) int {
	o := inv.opts
	if err := validateGlobs(o); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	var matcher *regexp.Regexp
	if inv.hasPattern {
		re, err := buildMatcher(inv.pattern, o)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return exitError
		}
		matcher = re
	}

	cat, err := db.catalog()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	target, err := resolveColumnTarget("freq", cat, o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	column := quoteIdent(target.column)
	query := `SELECT CAST(` + column + ` AS TEXT) AS value, COUNT(*) AS frequency
		FROM ` + quoteIdent(target.table) + `
		WHERE typeof(` + column + `) NOT IN ('null', 'blob')
		GROUP BY CAST(` + column + ` AS TEXT) COLLATE BINARY
		ORDER BY frequency DESC, value COLLATE BINARY ASC`
	var args []any
	if !inv.hasPattern && o.limit > 0 {
		query += ` LIMIT ?`
		args = append(args, o.limit)
	}

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	emitted := 0
	for rows.Next() {
		var value string
		var count int64
		if err := rows.Scan(&value, &count); err != nil {
			_ = rows.Close()
			_, _ = fmt.Fprintln(stderr, err)
			return exitError
		}
		if matcher != nil && !matcher.MatchString(value) {
			continue
		}
		if err := writeFrequency(stdout, target, value, count, o.jsonOut, o.tsvOut); err != nil {
			_ = rows.Close()
			_, _ = fmt.Fprintln(stderr, err)
			return exitError
		}
		emitted++
		if inv.hasPattern && o.limit > 0 && emitted >= o.limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	if err := rows.Close(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	if emitted == 0 {
		return exitNoMatch
	}
	return exitMatch
}
