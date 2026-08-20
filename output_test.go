package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderValueTruncationWindow(t *testing.T) {
	p := &printer{maxColumns: 20}
	long := strings.Repeat("x", 50) + "timeout" + strings.Repeat("y", 50)
	got := p.renderValue(long, [][2]int{{50, 57}})
	if len(got) > 26 { // window + ellipses
		t.Errorf("len = %d: %q", len(got), got)
	}
	if !strings.Contains(got, "timeout") {
		t.Errorf("window must center on the match: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("trimmed ends need ellipses: %q", got)
	}
}

func TestRenderValueEscapesNewlines(t *testing.T) {
	p := &printer{maxColumns: 0}
	got := p.renderValue("line one\nline two", nil)
	if strings.ContainsRune(got, '\n') || !strings.Contains(got, `\n`) {
		t.Errorf("newlines must be escaped: %q", got)
	}
}

func TestPrintMatchText(t *testing.T) {
	var b bytes.Buffer
	p := &printer{w: &b, maxColumns: 200}
	if err := p.printMatch(match{table: "events", column: "message", rowid: 412,
		value: "connection timeout", spans: [][2]int{{11, 18}}}); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "events.message:412: connection timeout\n" {
		t.Errorf("text line = %q", got)
	}
	b.Reset()
	if err := p.printMatch(match{table: "config", column: "value",
		pk: []pkVal{{"key", "timeout"}}, value: "30"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "pk=(timeout)") {
		t.Errorf("pk identity rendering: %q", b.String())
	}
}

func TestPrintMatchJSON(t *testing.T) {
	var b bytes.Buffer
	p := &printer{w: &b, jsonOut: true}
	if err := p.printMatch(match{table: "events", column: "message", rowid: 412,
		value: "connection timeout", spans: [][2]int{{11, 18}}}); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(b.Bytes(), &obj); err != nil {
		t.Fatalf("not JSONL: %v: %q", err, b.String())
	}
	if obj["table"] != "events" || obj["rowid"] != float64(412) {
		t.Errorf("obj = %v", obj)
	}
	if _, hasPK := obj["pk"]; hasPK {
		t.Errorf("rowid identity must not carry pk: %v", obj)
	}
}

func TestPrintFTSMatch(t *testing.T) {
	var b bytes.Buffer
	p := &printer{w: &b}
	if err := p.printFTSMatch(ftsMatch{table: "notes", rowid: 2,
		snippet: "ripgrep \x01habits\x02 transfer", rank: -1.5}); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "notes:2: ripgrep habits transfer\n" {
		t.Errorf("fts text = %q (markers stripped without color)", got)
	}
	b.Reset()
	p.jsonOut = true
	if err := p.printFTSMatch(ftsMatch{table: "notes", rowid: 2, snippet: "a \x01b\x02", rank: -1.5}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"rank":-1.5`) || strings.ContainsRune(b.String(), '\x01') {
		t.Errorf("fts json = %q", b.String())
	}
}

func TestPrintFTSMatchTruncatesSnippet(t *testing.T) {
	var b bytes.Buffer
	p := &printer{w: &b, maxColumns: 20}
	long := strings.Repeat("x", 40) + "\x01hit\x02" + strings.Repeat("y", 40)
	if err := p.printFTSMatch(ftsMatch{table: "notes", rowid: 1, snippet: long}); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.Contains(got, "hit") || !strings.Contains(got, "…") {
		t.Errorf("snippet window must center on marker with ellipses: %q", got)
	}
	if len(got) > len("notes:1: ")+30 {
		t.Errorf("snippet not truncated: %q", got)
	}
}

// TestPrintFTSMatchTruncatesSnippetWithColor covers a review finding:
// highlighting must happen after the truncation window is computed over
// marker-free text, so ANSI bytes never eat into maxColumns and every
// opened highlight is closed, even when the window clips it.
func TestPrintFTSMatchTruncatesSnippetWithColor(t *testing.T) {
	var b bytes.Buffer
	p := &printer{w: &b, maxColumns: 20, color: true}
	long := strings.Repeat("x", 40) + "\x01hit\x02" + strings.Repeat("y", 40)
	if err := p.printFTSMatch(ftsMatch{table: "notes", rowid: 1, snippet: long}); err != nil {
		t.Fatal(err)
	}
	got := b.String()

	opens := strings.Count(got, "\x1b[1;31m")
	resets := strings.Count(got, "\x1b[0m")
	if opens == 0 {
		t.Fatalf("expected at least one highlight: %q", got)
	}
	if opens != resets {
		t.Errorf("unbalanced ANSI: %d opens vs %d resets: %q", opens, resets, got)
	}

	visible := strings.ReplaceAll(got, "\x1b[1;31m", "")
	visible = strings.ReplaceAll(visible, "\x1b[0m", "")
	visible = strings.TrimPrefix(visible, "notes:1: ")
	visible = strings.TrimSuffix(visible, "\n")
	if n := len([]rune(visible)); n > 22 { // maxColumns + 2 ellipses
		t.Errorf("ANSI bytes must not count toward the window: %d visible runes: %q", n, got)
	}
	if !strings.Contains(got, "hit") {
		t.Errorf("expected match text present: %q", got)
	}
}

// TestRenderValueMultibyteRuneWindow covers a review finding: maxColumns is
// a rune (character) budget, not a byte budget, so multibyte text must not
// be truncated too short. The old byte-based code satisfied a "<= 10
// runes" check too (it kept ~3 runes), so this asserts the exact expected
// 10-rune window rather than just an upper bound.
func TestRenderValueMultibyteRuneWindow(t *testing.T) {
	p := &printer{maxColumns: 10}
	value := strings.Repeat("あ", 10) + "い" + strings.Repeat("う", 10) // 21 runes
	spanStart := len(strings.Repeat("あ", 10))
	spanEnd := spanStart + len("い")
	got := p.renderValue(value, [][2]int{{spanStart, spanEnd}})

	want := "…" + strings.Repeat("あ", 5) + "い" + strings.Repeat("う", 4) + "…"
	if got != want {
		t.Errorf("window mismatch:\n got  = %q\n want = %q", got, want)
	}
}

// TestRenderValueBoundedOnHugeValue covers a review finding: rendering a
// small window must not scan or escape the whole value. A ~5MB value with
// a small maxColumns should still produce the correct centered window —
// the point isn't the size (this doesn't profile allocations) but that
// the windowing logic is expressed as bounded passes around the match,
// not a whole-value transform, so this is safe to run at MB scale at all.
func TestRenderValueBoundedOnHugeValue(t *testing.T) {
	p := &printer{maxColumns: 20}
	const half = 2 * 1024 * 1024
	prefix := strings.Repeat("x", half)
	suffix := strings.Repeat("y", half)
	value := prefix + "timeout" + suffix
	spanStart := len(prefix)
	spanEnd := spanStart + len("timeout")

	got := p.renderValue(value, [][2]int{{spanStart, spanEnd}})

	if !strings.Contains(got, "timeout") {
		t.Errorf("window must center on the match: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("trimmed ends need ellipses: %q", got)
	}
	visible := strings.Trim(got, "…")
	if n := len([]rune(visible)); n > 20 {
		t.Errorf("window should be ~20 runes, got %d: %q", n, got)
	}
}

// TestRenderValueEscapedNewlineCountsTowardWindow covers a review finding:
// the window is measured on the rendered (escaped) text, so an expanded
// \n/\r/\t escape sequence counts its full width toward maxColumns.
func TestRenderValueEscapedNewlineCountsTowardWindow(t *testing.T) {
	p := &printer{maxColumns: 10}
	value := strings.Repeat("a", 5) + "\n" + strings.Repeat("b", 20)
	got := p.renderValue(value, nil)

	visible := strings.TrimSuffix(got, "…")
	visible = strings.TrimPrefix(visible, "…")
	if n := len([]rune(visible)); n > 10 {
		t.Errorf("escaped runes must count toward the window: %d runes: %q", n, got)
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("newline should still be escaped within the window: %q", got)
	}
}

// TestRenderValueCentersOnEscapedRunePosition covers a review finding: the
// window's center must come from the match's own escaped-rune endpoints,
// not a byte offset translated through the map. A byte offset only lands
// on the true rune-count midpoint when every byte inside the match renders
// 1:1 — a multibyte rune (あ, 3 bytes / 1 rune) or an escaped control
// character (\n, 1 byte / 2 rendered runes) inside the match breaks that,
// biasing the window toward whichever end has the narrower bytes-per-rune
// ratio. The match here ("あ\nB", 5 bytes / 4 escaped runes: あ, \, n, B)
// makes the byte midpoint (byte 2, inside あ) and the true rune midpoint
// (between \n and B) land in different places; want is hand-computed from
// the escaped-rune math, not from the old buggy behavior.
func TestRenderValueCentersOnEscapedRunePosition(t *testing.T) {
	p := &printer{maxColumns: 3}
	matchText := "あ\nB"
	value := matchText + strings.Repeat("z", 50)
	got := p.renderValue(value, [][2]int{{0, len(matchText)}})

	want := "…" + `\n` + "B" + "…"
	if got != want {
		t.Errorf("got = %q, want = %q", got, want)
	}
}

// TestRenderValueCentersOnEscapedRuneMidpointOfLongSpan covers a review
// finding: the bounded raw region must be fetched around the byte position
// of the span's escaped-rune midpoint, not around the arithmetic midpoint of
// its byte offsets. Here the match is ten あ (3 bytes / 1 column each) then
// thirty ASCII characters — 60 bytes but 40 columns — and is far longer than
// the fetch region, so both endpoints clamp to the region's edges and the
// region's own placement decides the window. The byte midpoint (byte 30)
// sits at the boundary between the two halves and keeps あ characters the
// true midpoint (column 20, byte 40) leaves out.
func TestRenderValueCentersOnEscapedRuneMidpointOfLongSpan(t *testing.T) {
	p := &printer{maxColumns: 10}
	value := strings.Repeat("あ", 10) + strings.Repeat("x", 30)
	got := p.renderValue(value, [][2]int{{0, len(value)}})

	// 40 rendered columns, midpoint at column 20; a 10-column window
	// centered there is columns 15-24, all inside the ASCII run.
	want := "…" + strings.Repeat("x", 10) + "…"
	if got != want {
		t.Errorf("got = %q, want = %q", got, want)
	}
}

// TestRenderValueCentersOnTrueMidpointWhenSpanOverflowsRegion covers a
// review finding: once a span reaches past the fetched region, both its
// endpoints clamp to the region's edges, so averaging them centers the
// window on the *region's* escaped midpoint. That drifts from the span's
// true midpoint whenever escape widths sit unevenly across the region.
// Here the match is fifteen newlines (1 byte / 2 columns each) then thirty
// ASCII characters: 45 bytes, 60 columns, with the true midpoint (column
// 30) exactly at the newline/ASCII boundary, byte 15. The region fetched
// around it is bytes 5-25 — ten newlines (20 columns) then ten ASCII (10
// columns) — whose own midpoint is column 15 of 30, five columns of
// newlines short of the truth.
func TestRenderValueCentersOnTrueMidpointWhenSpanOverflowsRegion(t *testing.T) {
	p := &printer{maxColumns: 10}
	value := strings.Repeat("\n", 15) + strings.Repeat("x", 30)
	got := p.renderValue(value, [][2]int{{0, len(value)}})

	// Columns 25-34 of the escaped text. The leading column is the "n" of
	// an escape pair the window's edge cuts through: the budget counts
	// rendered columns, so a boundary can land inside a two-column escape.
	want := "…n" + `\n\n` + strings.Repeat("x", 5) + "…"
	if got != want {
		t.Errorf("got = %q, want = %q", got, want)
	}
}

// TestRenderValueSkipsZeroWidthSpanHighlight covers a review finding: a
// zero-width span (e.g. from a ^ or $ regex match) must not emit an empty
// ANSI open/reset pair around nothing.
func TestRenderValueSkipsZeroWidthSpanHighlight(t *testing.T) {
	p := &printer{color: true} // maxColumns 0 -> unbounded (single-pass) path
	got := p.renderValue("hello world", [][2]int{{0, 5}, {5, 5}})

	if n := strings.Count(got, "\x1b[1;31m"); n != 1 {
		t.Errorf("expected exactly one highlight open, got %d: %q", n, got)
	}
	if n := strings.Count(got, "\x1b[0m"); n != 1 {
		t.Errorf("expected exactly one highlight reset, got %d: %q", n, got)
	}
	want := "\x1b[1;31mhello\x1b[0m world"
	if got != want {
		t.Errorf("got = %q, want = %q", got, want)
	}
}
