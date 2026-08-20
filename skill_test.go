package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillBytes is the embedded SKILL.md content, reused across tests.
func skillBytes(t *testing.T) []byte {
	t.Helper()
	b, err := skillFS.ReadFile("skills/litefind/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded skill: %v", err)
	}
	return b
}

// writeFile is a small helper to create dir/file in one step.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseSkillPrint(t *testing.T) {
	inv, err := parseInvocation([]string{"--skill", "print"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if inv.sub != "skill" || inv.skill.sub != "print" {
		t.Errorf("got sub=%q skill=%+v, want sub=skill print", inv.sub, inv.skill)
	}
}

func TestParseSkillStatus(t *testing.T) {
	inv, err := parseInvocation([]string{"--skill", "status"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if inv.sub != "skill" || inv.skill.sub != "status" {
		t.Errorf("got sub=%q skill=%+v, want sub=skill status", inv.sub, inv.skill)
	}
}

func TestParseSkillInstall(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want target
		dir  string
	}{
		{"default target user", []string{"--skill", "install"}, targetUser, ""},
		{"explicit user", []string{"--skill", "install", "--target", "user"}, targetUser, ""},
		{"project", []string{"--skill", "install", "--target", "project"}, targetProject, ""},
		{"both", []string{"--skill", "install", "--target", "both"}, targetBoth, ""},
		{"dir", []string{"--skill", "install", "--dir", "/tmp/sk"}, targetUser, "/tmp/sk"},
		{"force", []string{"--skill", "install", "--force"}, targetUser, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inv, err := parseInvocation(c.argv)
			if err != nil {
				t.Fatalf("parse %v: %v", c.argv, err)
			}
			if inv.sub != "skill" || inv.skill.sub != "install" {
				t.Fatalf("got sub=%q skill=%+v", inv.sub, inv.skill)
			}
			if inv.skill.target != c.want {
				t.Errorf("target = %v, want %v", inv.skill.target, c.want)
			}
			if inv.skill.dir != c.dir {
				t.Errorf("dir = %q, want %q", inv.skill.dir, c.dir)
			}
		})
	}
}

func TestParseSkillEqForm(t *testing.T) {
	inv, err := parseInvocation([]string{"--skill=print"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if inv.sub != "skill" || inv.skill.sub != "print" {
		t.Errorf("got sub=%q skill=%+v", inv.sub, inv.skill)
	}
}

func TestParseSkillRequiresSubcmd(t *testing.T) {
	if _, err := parseInvocation([]string{"--skill"}); err == nil {
		t.Fatal("want error for --skill without subcommand")
	}
}

func TestParseSkillRejectsUnknownSubcmd(t *testing.T) {
	if _, err := parseInvocation([]string{"--skill", "frobnicate"}); err == nil {
		t.Fatal("want error for unknown --skill subcommand")
	}
}

func TestParseSkillInstallRejectsBadTarget(t *testing.T) {
	if _, err := parseInvocation([]string{"--skill", "install", "--target", "nowhere"}); err == nil {
		t.Fatal("want error for bad --target")
	}
}

func TestParseSkillInstallRejectsDirWithBoth(t *testing.T) {
	if _, err := parseInvocation([]string{"--skill", "install", "--dir", "/x", "--target", "both"}); err == nil {
		t.Fatal("want error for --dir combined with --target both")
	}
}

func TestParseSkillInstallRejectsPositionals(t *testing.T) {
	if _, err := parseInvocation([]string{"--skill", "install", "extra"}); err == nil {
		t.Fatal("want error for --skill install with positional")
	}
}

func TestParseSkillPrintRejectsFlags(t *testing.T) {
	if _, err := parseInvocation([]string{"--skill", "print", "--force"}); err == nil {
		t.Fatal("want error for --skill print with --force")
	}
}

func TestParseSkillMutuallyExclusiveWithMode(t *testing.T) {
	if _, err := parseInvocation([]string{"--skill", "print", "--tables", "db"}); err == nil {
		t.Fatal("want error for --skill combined with --tables")
	}
	if _, err := parseInvocation([]string{"--skill", "print", "--schema", "db"}); err == nil {
		t.Fatal("want error for --skill combined with --schema")
	}
	if _, err := parseInvocation([]string{"--skill", "print", "pattern", "db"}); err == nil {
		t.Fatal("want error for --skill combined with search positionals")
	}
}

func TestRunSkillPrint(t *testing.T) {
	stdout, stderr, code := runCmd(t, "--skill", "print")
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d", code, exitMatch)
	}
	want := skillBytes(t)
	if stdout != string(want) {
		t.Errorf("stdout does not match embedded SKILL.md (got %d bytes, want %d)", len(stdout), len(want))
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestRunSkillInstallDir(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "litefind", "SKILL.md")
	stdout, stderr, code := runCmd(t, "--skill", "install", "--dir", dir)
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitMatch, stderr)
	}
	if !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want path %q", stdout, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read installed file: %v", err)
	}
	if !bytes.Equal(got, skillBytes(t)) {
		t.Errorf("installed SKILL.md does not match embedded")
	}
}

func TestRunSkillInstallConflictNoForce(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "litefind", "SKILL.md")
	writeFile(t, dst, []byte("old content"))

	_, stderr, code := runCmd(t, "--skill", "install", "--dir", dir)
	if code != exitError {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want 'already exists'", stderr)
	}
	// File must be unchanged.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "old content" {
		t.Errorf("file overwritten without --force")
	}
}

func TestRunSkillInstallForce(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "litefind", "SKILL.md")
	writeFile(t, dst, []byte("old content"))

	stdout, stderr, code := runCmd(t, "--skill", "install", "--dir", dir, "--force")
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitMatch, stderr)
	}
	if !strings.Contains(stdout, dst) {
		t.Errorf("stdout = %q, want path %q", stdout, dst)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, skillBytes(t)) {
		t.Errorf("installed SKILL.md does not match embedded after --force")
	}
	_ = stderr
}

func TestRunSkillInstallTargetProject(t *testing.T) {
	// --target project resolves to ./skills relative to the cwd.
	work := t.TempDir()
	t.Chdir(work)
	stdout, _, code := runCmd(t, "--skill", "install", "--target", "project")
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d", code, exitMatch)
	}
	want := filepath.Join(work, "skills", "litefind", "SKILL.md")
	if !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want path %q", stdout, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, skillBytes(t)) {
		t.Errorf("installed SKILL.md does not match embedded")
	}
}

func TestRunSkillInstallBothAtomicOnConflict(t *testing.T) {
	// --target both must abort without writing either location if one
	// already exists and --force is not set. Use a fake home so the
	// user location is isolated.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	// Pre-populate the project location.
	work := t.TempDir()
	t.Chdir(work)
	writeFile(t, filepath.Join(work, "skills", "litefind", "SKILL.md"), []byte("stale"))

	_, stderr, code := runCmd(t, "--skill", "install", "--target", "both")
	if code != exitError {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want 'already exists'", stderr)
	}
	// User location must NOT have been written.
	userPath := filepath.Join(home, ".agents", "skills", "litefind", "SKILL.md")
	if _, err := os.Stat(userPath); err == nil {
		t.Errorf("user location written despite conflict; install was not atomic")
	}
}

func TestRunSkillStatusNotFound(t *testing.T) {
	// Use --dir pointing at an empty dir: the skill is not installed there.
	dir := t.TempDir()
	stdout, stderr, code := runCmd(t, "--skill", "status", "--dir", dir)
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitMatch, stderr)
	}
	if !strings.Contains(stdout, "not installed") {
		t.Errorf("stdout = %q, want 'not installed'", stdout)
	}
}

func TestParseSkillStatusAcceptsTarget(t *testing.T) {
	inv, err := parseInvocation([]string{"--skill", "status", "--target", "project"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if inv.skill.target != targetProject {
		t.Errorf("target = %v, want project", inv.skill.target)
	}
}

func TestRunSkillStatusBothReportsEach(t *testing.T) {
	// --target both without --dir resolves to user + project roots.
	// We can't easily isolate the real home dir, so just confirm both
	// lines appear in the output.
	stdout, _, code := runCmd(t, "--skill", "status", "--target", "both")
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d", code, exitMatch)
	}
	if !strings.Contains(stdout, ".agents/skills/litefind/SKILL.md") {
		t.Errorf("stdout missing user path: %q", stdout)
	}
	if !strings.Contains(stdout, "skills/litefind/SKILL.md") {
		t.Errorf("stdout missing project path: %q", stdout)
	}
}

func TestRunSkillStatusCurrent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "litefind", "SKILL.md"), skillBytes(t))

	stdout, stderr, code := runCmd(t, "--skill", "status", "--dir", dir)
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitMatch, stderr)
	}
	if !strings.Contains(stdout, "current") {
		t.Errorf("stdout = %q, want 'current'", stdout)
	}
}

func TestRunSkillStatusDrifted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "litefind", "SKILL.md"), []byte("stale content"))

	stdout, stderr, code := runCmd(t, "--skill", "status", "--dir", dir)
	if code != exitMatch {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitMatch, stderr)
	}
	if !strings.Contains(stdout, "DRIFTED") {
		t.Errorf("stdout = %q, want 'DRIFTED'", stdout)
	}
}
