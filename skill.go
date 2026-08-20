package main

import (
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// skillMD is the path of the embedded skill file within skillFS.
const skillMD = "skills/litefind/SKILL.md"

type skillOpts struct {
	sub    string // install, print, status
	target target // user, project, both
	dir    string // --dir: skills-root override
	force  bool
}

type target int

const (
	targetUser target = iota
	targetProject
	targetBoth
)

func (t target) String() string {
	switch t {
	case targetUser:
		return "user"
	case targetProject:
		return "project"
	case targetBoth:
		return "both"
	}
	return "?"
}

// parseSkill parses the arguments following --skill. argv[0] is the
// subcommand; the rest are subcommand-specific flags.
func parseSkill(argv []string) (*invocation, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("--skill requires a subcommand: install, print, or status")
	}
	sub := argv[0]
	rest := argv[1:]
	inv := &invocation{sub: "skill", skill: skillOpts{sub: sub, target: targetUser}}

	switch sub {
	case "print":
		fs := flag.NewFlagSet("litefind --skill print", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if len(fs.Args()) > 0 {
			return nil, fmt.Errorf("--skill print takes no positional arguments")
		}
	case "status", "install":
		name := "litefind --skill " + sub
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var t string
		fs.StringVar(&t, "target", "user", "")
		fs.StringVar(&inv.skill.dir, "dir", "", "")
		if sub == "install" {
			fs.BoolVar(&inv.skill.force, "force", false, "")
		}
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		switch t {
		case "user":
			inv.skill.target = targetUser
		case "project":
			inv.skill.target = targetProject
		case "both":
			inv.skill.target = targetBoth
		default:
			return nil, fmt.Errorf(`--target must be "user", "project", or "both"`)
		}
		if inv.skill.dir != "" && inv.skill.target == targetBoth {
			return nil, fmt.Errorf("--dir cannot be combined with --target both")
		}
		if len(fs.Args()) > 0 {
			return nil, fmt.Errorf("--skill %s takes no positional arguments", sub)
		}
	default:
		return nil, fmt.Errorf(`--skill subcommand %q is not valid; use install, print, or status`, sub)
	}
	return inv, nil
}

// cmdSkill dispatches a parsed --skill invocation.
func cmdSkill(inv *invocation, stdout, stderr io.Writer) int {
	switch inv.skill.sub {
	case "print":
		return skillPrint(stdout, stderr)
	case "status":
		return skillStatus(inv.skill, stdout, stderr)
	case "install":
		return skillInstall(inv.skill, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "unknown --skill subcommand %q\n", inv.skill.sub)
	return exitError
}

func skillPrint(stdout, stderr io.Writer) int {
	data, err := skillFS.ReadFile(skillMD)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read embedded skill: %v\n", err)
		return exitError
	}
	_, _ = stdout.Write(data)
	return exitMatch
}

// skillRoots returns the skills-root directories to act on for a given
// invocation: --dir overrides everything; otherwise the target selects
// one or both default locations.
func skillRoots(o skillOpts) ([]string, error) {
	if o.dir != "" {
		return []string{o.dir}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	user := filepath.Join(home, ".agents", "skills")
	project, err := filepath.Abs(filepath.Join(".", "skills"))
	if err != nil {
		return nil, fmt.Errorf("resolve project skills dir: %w", err)
	}
	switch o.target {
	case targetUser:
		return []string{user}, nil
	case targetProject:
		return []string{project}, nil
	case targetBoth:
		return []string{user, project}, nil
	}
	return nil, fmt.Errorf("unknown target %q", o.target)
}

func skillInstall(o skillOpts, stdout, stderr io.Writer) int {
	data, err := skillFS.ReadFile(skillMD)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read embedded skill: %v\n", err)
		return exitError
	}
	roots, err := skillRoots(o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	// Pre-check: if any target exists and !force, abort before writing
	// anything so a multi-target install is atomic.
	for _, root := range roots {
		dst := filepath.Join(root, "litefind", "SKILL.md")
		if _, err := os.Stat(dst); err == nil && !o.force {
			_, _ = fmt.Fprintf(stderr, "%s already exists; use --force to overwrite\n", dst)
			return exitError
		}
	}

	for _, root := range roots {
		dst := filepath.Join(root, "litefind", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			_, _ = fmt.Fprintf(stderr, "create %s: %v\n", filepath.Dir(dst), err)
			return exitError
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "write %s: %v\n", dst, err)
			return exitError
		}
		_, _ = fmt.Fprintf(stdout, "installed %s\n", dst)
	}
	return exitMatch
}

func skillStatus(o skillOpts, stdout, stderr io.Writer) int {
	data, err := skillFS.ReadFile(skillMD)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read embedded skill: %v\n", err)
		return exitError
	}
	roots, err := skillRoots(o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	_, _ = fmt.Fprintf(stdout, "litefind %s\n", version())
	for _, root := range roots {
		dst := filepath.Join(root, "litefind", "SKILL.md")
		got, err := os.ReadFile(dst)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			_, _ = fmt.Fprintf(stdout, "  %s: not installed\n", dst)
		case err != nil:
			_, _ = fmt.Fprintf(stdout, "  %s: error: %v\n", dst, err)
		case bytes.Equal(got, data):
			_, _ = fmt.Fprintf(stdout, "  %s: installed, current\n", dst)
		default:
			_, _ = fmt.Fprintf(stdout, "  %s: installed, DRIFTED\n", dst)
		}
	}
	return exitMatch
}

// Ensure the embed.FS type is referenced for compile-time correctness.
var _ embed.FS
