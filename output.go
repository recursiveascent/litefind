package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// printer renders matches to w. maxColumns is the default text-only
// truncation; an explicitly set headLines previews raw lines in text and JSON.
type printer struct {
	w          io.Writer
	jsonOut    bool
	tsvOut     bool
	color      bool // ANSI highlight; true only for tty text mode
	maxColumns int  // 0 = no truncation, else a rune budget
	headSet    bool
	headLines  int
}

// identityString renders rowid or pk=(v1,v2) per the output contract.
func identityString(m match) string {
	if m.pk != nil {
		vals := make([]string, len(m.pk))
		for i, pv := range m.pk {
			vals[i] = fmt.Sprint(pv.val)
		}
		return "pk=(" + strings.Join(vals, ",") + ")"
	}
	return strconv.FormatInt(m.rowid, 10)
}

func tsvTextField(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			b.WriteString(`\\`)
			i++
		case '\t':
			b.WriteString(`\t`)
			i++
		case '\n':
			b.WriteString(`\n`)
			i++
		case '\r':
			b.WriteString(`\r`)
			i++
		default:
			if s[i] < 0x20 || s[i] == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, s[i])
				i++
				continue
			}
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				fmt.Fprintf(&b, `\x%02x`, s[i])
				i++
				continue
			}
			b.WriteString(s[i : i+size])
			i += size
		}
	}
	return b.String()
}

func tsvRowField(v any) string {
	switch v := v.(type) {
	case nil:
		return `\N`
	case []byte:
		return `\B` + hex.EncodeToString(v)
	case string:
		return tsvTextField(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return tsvTextField(fmt.Sprint(v))
	}
}

func tsvPKToken(v any) string {
	switch v := v.(type) {
	case nil:
		return "n:"
	case []byte:
		return "b:" + hex.EncodeToString(v)
	case string:
		return "t:" + tsvTextField(v)
	case int64:
		return "i:" + strconv.FormatInt(v, 10)
	case float64:
		return "r:" + strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return "t:" + tsvTextField(fmt.Sprint(v))
	}
}

func tsvIdentity(m match) string {
	if m.pk == nil {
		return strconv.FormatInt(m.rowid, 10)
	}
	tokens := make([]string, len(m.pk))
	for i, pv := range m.pk {
		tokens[i] = tsvPKToken(pv.val)
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		panic(err)
	}
	return "pk=" + string(encoded)
}

func writeTSVRecord(w io.Writer, fields ...string) error {
	_, err := io.WriteString(w, strings.Join(fields, "\t")+"\n")
	return err
}

func headPreview(value string, lines int) (string, int) {
	if lines <= 0 || value == "" {
		return value, 0
	}
	cut := 0
	for range lines {
		i := strings.IndexByte(value[cut:], '\n')
		if i < 0 {
			return value, 0
		}
		cut += i + 1
	}
	if cut == len(value) {
		return value, 0
	}
	rest := value[cut:]
	omitted := strings.Count(rest, "\n")
	if !strings.HasSuffix(rest, "\n") {
		omitted++
	}
	return value[:cut], omitted
}

func clipSpans(spans [][2]int, limit int) [][2]int {
	clipped := make([][2]int, 0, len(spans))
	for _, span := range spans {
		start, end := span[0], span[1]
		if start == end {
			if start >= 0 && start <= limit {
				clipped = append(clipped, span)
			}
			continue
		}
		if end <= 0 || start >= limit {
			continue
		}
		start = max(start, 0)
		end = min(end, limit)
		if start < end {
			clipped = append(clipped, [2]int{start, end})
		}
	}
	return clipped
}

// renderValue single-lines value (\n -> \\n), truncates to a window of
// maxColumns runes centered on the first span (ellipsis on trimmed ends),
// and highlights spans with ANSI when color is on. Returns the rendered
// string.
//
// The window is measured in runes of the *escaped* text, not raw bytes:
// truncation is a display-width budget, so a multibyte rune costs one slot
// like any other, and an expanded \n/\r/\t escape costs its full two runes.
func (p *printer) renderValue(value string, spans [][2]int) string {
	if p.maxColumns <= 0 {
		return p.renderUnbounded(value, spans)
	}
	return p.renderWindowed(value, spans)
}

// renderUnbounded escapes and highlights the entire value with no
// truncation window, in a single forward pass over the source bytes. It
// never allocates a []rune copy or a byte->rune offset map — nothing needs
// rune-coordinate translation, since there's no window to position — only
// a builder sized to the (unavoidable) output and a cursor into spans.
func (p *printer) renderUnbounded(value string, spans [][2]int) string {
	return p.renderFull(value, spans, false)
}

func (p *printer) renderHeadValue(value string, spans [][2]int) string {
	return p.renderFull(value, spans, true)
}

func (p *printer) renderFull(value string, spans [][2]int, multiline bool) string {
	var b strings.Builder
	cur := 0
	inSpan := false
	for i := 0; i <= len(value); {
		// A span's close and the next span's open can land on the same
		// byte offset (adjacent or zero-width spans), so drain every
		// boundary event at i before advancing past it.
		for {
			if inSpan {
				if cur < len(spans) && spans[cur][1] == i {
					if p.color {
						b.WriteString("\x1b[0m")
					}
					inSpan = false
					cur++
					continue
				}
			} else if cur < len(spans) && spans[cur][0] == i {
				// A zero-width (or otherwise empty) span has nothing to
				// wrap: opening and immediately closing highlight codes
				// around no characters would just litter the output
				// with empty ANSI pairs, e.g. from a ^ or $ regex match.
				if spans[cur][1] <= spans[cur][0] {
					cur++
					continue
				}
				if p.color {
					b.WriteString("\x1b[1;31m")
				}
				inSpan = true
				continue
			}
			break
		}
		if i == len(value) {
			break
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if multiline && r == '\n' {
			b.WriteByte('\n')
		} else if esc := escapeRune(r); esc != "" {
			b.WriteString(esc)
		} else {
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// renderWindowed truncates value to a maxColumns-rune window centered on
// the first span (or the start of the text when there are none), escaping
// and highlighting only that window.
//
// It never touches the whole value: it first walks value backward and
// forward from the span's midpoint by at most maxColumns *original* runes
// each way (stepBackRunes/stepForwardRunes) to fetch a bounded raw region
// — since escaping only ever expands a rune's width, that original-rune
// budget always covers at least maxColumns escaped runes on each side, so
// the true window can never reach the fetched region's edges (unless
// those edges are value's own start/end). Only that bounded region is
// escaped and rune-indexed; the rest of value, however large, is never
// read.
//
// The region is fetched around the byte position of the span's
// *escaped-rune* midpoint (spanEscapedMidByte), not around the arithmetic
// midpoint of its byte offsets, and the window is centered on that same
// position whenever the span reaches past the region. For a span longer
// than the region with an uneven bytes-per-escaped-rune ratio across it —
// say ten あ followed by thirty ASCII characters — a byte midpoint would
// fetch over the byte-heavy half of the match instead.
func (p *printer) renderWindowed(value string, spans [][2]int) string {
	var spanStart, spanEnd int
	if len(spans) > 0 {
		spanStart, spanEnd = spans[0][0], spans[0][1]
	}
	mid := spanEscapedMidByte(value, spanStart, spanEnd)

	rawStart := stepBackRunes(value, mid, p.maxColumns)
	rawEnd := stepForwardRunes(value, mid, p.maxColumns)
	raw := value[rawStart:rawEnd]

	// Keep only the spans (clipped to the fetched region's byte range)
	// that escapeToRunes' byteToRune map below will actually have keys
	// for; a span reaching outside [rawStart, rawEnd) is either the
	// centering span (handled via mid, not its own offsets) or too far
	// from the window to matter.
	var rawSpans [][2]int
	for _, sp := range spans {
		a, b := sp[0], sp[1]
		if b <= rawStart || a >= rawEnd {
			continue
		}
		if a < rawStart {
			a = rawStart
		}
		if b > rawEnd {
			b = rawEnd
		}
		rawSpans = append(rawSpans, [2]int{a - rawStart, b - rawStart})
	}

	rendered, byteToRune := escapeToRunes(raw)
	total := len(rendered)

	// Center on the escaped-rune positions of the first span's own
	// endpoints, not on a byte offset translated through the map: a byte
	// offset only lands on the true rune-count midpoint when every byte
	// between spanStart and spanEnd renders 1:1, which a multibyte rune
	// or an escaped control character inside the match breaks. Averaging
	// the endpoints' own escaped-rune positions (runeWindow does the
	// averaging) stays correct regardless of what's inside the span.
	//
	// That averaging only works while both endpoints are inside the
	// fetched region. Once either falls outside, escapedRunePos clamps it
	// to the region's edge, and averaging the edges yields the *region's*
	// escaped midpoint — which drifts from the span's true midpoint
	// whenever escape widths are distributed unevenly across the region
	// (say newlines before the midpoint and ASCII after: the newline half
	// occupies twice the columns per byte, pulling the region's midpoint
	// toward it). mid already holds the exact byte position of the true
	// midpoint, so translate that one position into region-local
	// coordinates and center on it directly.
	var centerStart, centerEnd int
	if len(spans) > 0 {
		if spanStart < rawStart || spanEnd > rawEnd {
			c := escapedRunePos(byteToRune, mid-rawStart, len(raw), total)
			centerStart, centerEnd = c, c
		} else {
			centerStart = escapedRunePos(byteToRune, spanStart-rawStart, len(raw), total)
			centerEnd = escapedRunePos(byteToRune, spanEnd-rawStart, len(raw), total)
		}
	}
	start, end := runeWindow(total, centerStart, centerEnd, p.maxColumns)

	// Re-anchor spans to the window (in rune coordinates) and clip any
	// that straddle its edges, so a match cut by truncation still gets
	// a highlight for the part that survived.
	var windowSpans [][2]int
	for _, sp := range rawSpans {
		a, b := byteToRune[sp[0]], byteToRune[sp[1]]
		if b <= start || a >= end {
			continue
		}
		if a < start {
			a = start
		}
		if b > end {
			b = end
		}
		windowSpans = append(windowSpans, [2]int{a - start, b - start})
	}

	out := highlightRunes(rendered[start:end], windowSpans, p.color)
	trimmedStart := rawStart > 0 || start > 0
	trimmedEnd := rawEnd < len(value) || end < total
	return addEllipses(out, trimmedStart, trimmedEnd)
}

func (p *printer) writeHeadMatch(prefix, value string, spans [][2]int) error {
	value, omitted := headPreview(value, p.headLines)
	spans = clipSpans(spans, len(value))
	line := prefix + p.renderHeadValue(value, spans)
	if omitted > 0 {
		line += fmt.Sprintf("[... %d more lines]", omitted)
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, err := io.WriteString(p.w, line)
	return err
}

// printMatch emits either "table.column:identity: rendered" or the JSONL
// object {table, column, rowid|pk, value, spans[, row]}.
func (p *printer) printMatch(m match) error {
	if p.jsonOut {
		return p.encodeMatchJSON(m)
	}
	if p.tsvOut {
		fields := []string{
			tsvTextField(m.table),
			tsvTextField(m.column),
			tsvIdentity(m),
			tsvTextField(m.value),
		}
		for _, value := range m.rowValues {
			fields = append(fields, tsvRowField(value))
		}
		return writeTSVRecord(p.w, fields...)
	}
	prefix := m.table + "." + m.column + ":" + identityString(m) + ": "
	if p.headSet {
		return p.writeHeadMatch(prefix, m.value, m.spans)
	}
	line := prefix + p.renderValue(m.value, m.spans) + "\n"
	_, err := io.WriteString(p.w, line)
	return err
}

func (p *printer) encodeMatchJSON(m match) error {
	value, spans := m.value, m.spans
	omitted := 0
	if p.headSet {
		value, omitted = headPreview(value, p.headLines)
		spans = clipSpans(spans, len(value))
	}
	obj := map[string]any{
		"table":  m.table,
		"column": m.column,
		"value":  value,
	}
	if m.pk != nil {
		pk := make(map[string]any, len(m.pk))
		for _, pv := range m.pk {
			pk[pv.col] = pv.val
		}
		obj["pk"] = pk
	} else {
		obj["rowid"] = m.rowid
	}
	if len(spans) > 0 {
		obj["spans"] = spans
	}
	if omitted > 0 {
		obj["truncated_lines"] = omitted
	}
	if m.row != nil {
		obj["row"] = m.row
	}
	return json.NewEncoder(p.w).Encode(obj)
}

// ftsMatch is one FTS5 hit: a snippet with \x01/\x02 marker bytes bracketing
// the matched terms, ranked by SQLite's bm25-derived rank (more negative is
// a better match).
type ftsMatch struct {
	table     string
	rowid     int64
	snippet   string // with \x01 \x02 marker bytes around matched terms
	rank      float64
	row       map[string]any
	rowValues []any
	rowSource string
}

// printFTSMatch emits "table:rowid: snippet" (markers -> ANSI or removed)
// or JSONL {table, rowid, snippet, rank[, row]} with markers removed.
//
// The marker bytes are stripped into ordinary spans over the marker-free
// text first (extractMarkerSpans), so the snippet goes through the exact
// same rune-windowing/highlighting pipeline as a match value: the
// truncation window is computed over marker-free, ANSI-free text (ANSI
// bytes never eat into the maxColumns budget), and highlighting is applied
// only to whatever survives inside the window — always as balanced
// open/reset pairs, even when a span is clipped by the window edge.
func (p *printer) printFTSMatch(m ftsMatch) error {
	if p.jsonOut {
		return p.encodeFTSMatchJSON(m)
	}
	text, spans := extractMarkerSpans(m.snippet)
	if p.tsvOut {
		fields := []string{
			tsvTextField(m.table),
			strconv.FormatInt(m.rowid, 10),
			tsvTextField(text),
		}
		if m.row != nil {
			fields = append(fields, tsvTextField(m.rowSource))
			for _, value := range m.rowValues {
				fields = append(fields, tsvRowField(value))
			}
		}
		return writeTSVRecord(p.w, fields...)
	}
	prefix := m.table + ":" + strconv.FormatInt(m.rowid, 10) + ": "
	if p.headSet {
		return p.writeHeadMatch(prefix, text, spans)
	}
	line := prefix + p.renderValue(text, spans) + "\n"
	_, err := io.WriteString(p.w, line)
	return err
}

func (p *printer) encodeFTSMatchJSON(m ftsMatch) error {
	text, _ := extractMarkerSpans(m.snippet)
	omitted := 0
	if p.headSet {
		text, omitted = headPreview(text, p.headLines)
	}
	obj := map[string]any{
		"table":   m.table,
		"rowid":   m.rowid,
		"snippet": text,
		"rank":    m.rank,
	}
	if omitted > 0 {
		obj["truncated_lines"] = omitted
	}
	if m.row != nil {
		obj["row"] = m.row
	}
	return json.NewEncoder(p.w).Encode(obj)
}

// extractMarkerSpans strips an FTS5 snippet's \x01/\x02 marker bytes and
// returns the marker-free text together with the byte-offset spans (into
// that text) they bracketed — the same shape as a match's value/spans, so
// an FTS snippet can be rendered through renderValue's truncation and
// highlighting pipeline unchanged.
func extractMarkerSpans(snippet string) (text string, spans [][2]int) {
	var b strings.Builder
	openPos := -1
	for i := 0; i < len(snippet); {
		switch snippet[i] {
		case '\x01':
			openPos = b.Len()
			i++
		case '\x02':
			if openPos >= 0 {
				spans = append(spans, [2]int{openPos, b.Len()})
				openPos = -1
			}
			i++
		default:
			r, size := utf8.DecodeRuneInString(snippet[i:])
			b.WriteRune(r)
			i += size
		}
	}
	return b.String(), spans
}

// stepBackRunes returns the byte offset reached by stepping backward from
// from by at most n runes of s, clamped at 0 (s's start).
func stepBackRunes(s string, from, n int) int {
	i := from
	for c := 0; c < n && i > 0; c++ {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return i
}

// stepForwardRunes returns the byte offset reached by stepping forward
// from from by at most n runes of s, clamped at len(s) (s's end).
func stepForwardRunes(s string, from, n int) int {
	i := from
	for c := 0; c < n && i < len(s); c++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i
}

// alignToRuneStart moves i backward, if needed, to the start of the rune
// it falls inside — stepBackRunes/stepForwardRunes require a rune-boundary
// starting offset to decode correctly, but a span midpoint computed by
// byte-offset arithmetic can land mid-rune.
func alignToRuneStart(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// spanEscapedMidByte returns the byte offset in value of the rune boundary
// at (or just before) the midpoint of the span's *escaped* rune count — the
// point renderWindowed centers its bounded fetch region on.
//
// It streams the span's bytes once, decoding runes and adding each one's
// escaped width, so it allocates nothing and costs O(span length) no matter
// how long the span is; only the span is read, never the rest of value.
// Counting escaped widths (a multibyte rune is one column, a \n/\r/\t escape
// is two) is what makes the result a display-space midpoint rather than a
// byte-space one.
func spanEscapedMidByte(value string, spanStart, spanEnd int) int {
	if spanStart < 0 {
		spanStart = 0
	}
	if spanEnd > len(value) {
		spanEnd = len(value)
	}
	start := alignToRuneStart(value, spanStart)
	if spanEnd <= start {
		return start
	}
	total := 0
	for i := start; i < spanEnd; {
		r, size := utf8.DecodeRuneInString(value[i:])
		total += escapedRuneWidth(r)
		i += size
	}
	half := total / 2
	count := 0
	i := start
	for i < spanEnd {
		r, size := utf8.DecodeRuneInString(value[i:])
		w := escapedRuneWidth(r)
		if count+w > half {
			break
		}
		count += w
		i += size
	}
	return i
}

// escapeRune returns the escape sequence a rune renders as in text mode, or
// "" for a rune that renders as itself. It is the single definition of what
// escaping does, shared by the width count, the unbounded pass, and the
// windowed pipeline's escapeToRunes.
func escapeRune(r rune) string {
	switch r {
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	}
	return ""
}

// escapedRuneWidth is the number of rendered runes r occupies after
// escaping: 1 for anything that prints as itself (multibyte runes included),
// else the length of its escape sequence, which is pure ASCII so its byte
// length is its rune count.
func escapedRuneWidth(r rune) int {
	if esc := escapeRune(r); esc != "" {
		return len(esc)
	}
	return 1
}

// escapedRunePos translates a byte offset local to the fetched raw region
// (rawLen bytes long, total escaped runes) into escaped-rune coordinates
// via byteToRune. An offset outside [0, rawLen] — from a span endpoint
// that reaches past the bounded fetch — clamps to the region's own edge
// (0 or total) rather than a missing map lookup, which would silently
// return zero and misplace the window.
func escapedRunePos(byteToRune map[int]int, localOffset, rawLen, total int) int {
	switch {
	case localOffset <= 0:
		return 0
	case localOffset >= rawLen:
		return total
	default:
		return byteToRune[localOffset]
	}
}

// escapeToRunes renders value for text-mode output: \n, \r, \t become their
// two-rune escape sequences (\n, \r, \t as literal backslash pairs) so a
// value always prints on one line. It returns the resulting runes and a map
// from every rune-boundary byte offset in the original value to that
// rune's position in the rendered output — the coordinate translation that
// lets span byte offsets (always original-value rune boundaries) drive
// windowing and highlighting over the escaped text.
func escapeToRunes(value string) (rendered []rune, byteToRune map[int]int) {
	rendered = make([]rune, 0, len(value))
	byteToRune = make(map[int]int, len(value)+1)
	for i := 0; i < len(value); {
		byteToRune[i] = len(rendered)
		r, size := utf8.DecodeRuneInString(value[i:])
		if esc := escapeRune(r); esc != "" {
			for _, er := range esc {
				rendered = append(rendered, er)
			}
		} else {
			rendered = append(rendered, r)
		}
		i += size
	}
	byteToRune[len(value)] = len(rendered)
	return rendered, byteToRune
}

// runeWindow returns the rune range [start, end) of a total-length rendered
// text to keep, centered on [centerStart, centerEnd), sized to maxColumns
// runes. maxColumns <= 0, or text already within it, keeps everything —
// operating purely in rune counts makes every window boundary rune-safe by
// construction, no separate trimming pass needed.
func runeWindow(total, centerStart, centerEnd, maxColumns int) (start, end int) {
	if maxColumns <= 0 || total <= maxColumns {
		return 0, total
	}
	mid := (centerStart + centerEnd) / 2
	half := maxColumns / 2
	start = mid - half
	end = start + maxColumns
	if start < 0 {
		end -= start
		start = 0
	}
	if end > total {
		start -= end - total
		end = total
		if start < 0 {
			start = 0
		}
	}
	return start, end
}

// addEllipses prepends/appends "…" where the window trimmed text off the
// start/end.
func addEllipses(s string, trimmedStart, trimmedEnd bool) string {
	if trimmedStart {
		s = "…" + s
	}
	if trimmedEnd {
		s += "…"
	}
	return s
}

// highlightRunes renders window (already the truncated slice) as a string,
// wrapping each span in ANSI highlight codes when color is on. spans must
// already be rune offsets into window, clipped to its bounds by the
// caller — every span that's emitted is opened and reset in full, even one
// truncation clipped down to a partial match, so the terminal is never left
// mid-highlight.
func highlightRunes(window []rune, spans [][2]int, color bool) string {
	if !color || len(spans) == 0 {
		return string(window)
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		a, e := sp[0], sp[1]
		if a < last {
			a = last
		}
		if e > len(window) {
			e = len(window)
		}
		if a >= e {
			continue
		}
		b.WriteString(string(window[last:a]))
		b.WriteString("\x1b[1;31m")
		b.WriteString(string(window[a:e]))
		b.WriteString("\x1b[0m")
		last = e
	}
	b.WriteString(string(window[last:]))
	return b.String()
}
