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
	for _, want := range []string{`\b`, "--count", "--freq", "--limit", "--fts", "--tables", "--schema", "no rowid-aliasing INTEGER PRIMARY KEY"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q; stdout = %q", want, stdout)
		}
	}
}

func TestRunVersionPrintsVersionAndExits(t *testing.T) {
	for _, arg := range []string{"-V", "--version"} {
		t.Run(arg, func(t *testing.T) {
			stdout, stderr, code := runCmd(t, arg)
			if code != exitMatch {
				t.Fatalf("exit = %d, want %d", code, exitMatch)
			}
			want := "litefind " + version() + "\n"
			if stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestRunVersionShortCircuitsBeforePositionalValidation(t *testing.T) {
	// --version works even with args that would otherwise be a usage
	// error (missing db path) — it short-circuits before validation.
	stdout, _, code := runCmd(t, "--version", "no-db-here")
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d", code, exitMatch)
	}
	if !strings.Contains(stdout, version()) {
		t.Errorf("stdout = %q, want version %q", stdout, version())
	}
}

// version() resolves in priority order: override, then the VCS-stamped
// module version, then the embedded VERSION file. Under go test the
// module version is "(devel)", so version() falls back to the file.
func TestVersionResolution(t *testing.T) {
	// No override, (devel) module version → embedded VERSION file.
	prev := versionOverride
	versionOverride = ""
	got := version()
	if want := strings.TrimSpace(versionFile); got != want {
		t.Errorf("version() fallback = %q, want %q", got, want)
	}

	// Override wins over everything else.
	versionOverride = "v9.9.9"
	if got := version(); got != "v9.9.9" {
		t.Errorf("version() with override = %q, want v9.9.9", got)
	}
	versionOverride = prev
}
