package main

import (
	"bytes"
	"strings"
	"testing"
)

func runCmd(t *testing.T, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(argv, &out, &errb)
	return out.String(), errb.String(), code
}

func TestRunNoArgsIsUsageError(t *testing.T) {
	_, stderr, code := runCmd(t)
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "usage") {
		t.Errorf("stderr = %q, want usage text", stderr)
	}
}

func TestRunUnknownSubcommandPathIsError(t *testing.T) {
	// One positional arg (no db path) is a usage error for search.
	_, _, code := runCmd(t, "pattern-only")
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
}

func TestRunHelpPrintsFullHelp(t *testing.T) {
	stdout, _, code := runCmd(t, "-h")
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d", code, exitMatch)
	}
	// The standalone-FTS condition is about the rowid being unreachable,
	// which an INTEGER PRIMARY KEY DESC column does not fix — help must
	// not promise otherwise by saying merely "no INTEGER PRIMARY KEY".
	for _, want := range []string{`\b`, "--count", "--fts", "no rowid-aliasing INTEGER PRIMARY KEY"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q; stdout = %q", want, stdout)
		}
	}
}

func TestRunQuickstartPrintsGuide(t *testing.T) {
	stdout, stderr, code := runCmd(t, "quickstart")
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, exitMatch, stderr)
	}
	for _, want := range []string{"litefind quickstart", "tables", "schema", "--fts", "first steps"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q; stdout = %q", want, stdout)
		}
	}
}
