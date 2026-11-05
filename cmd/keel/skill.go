package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/emoss08/keel/keelcli"
	"github.com/emoss08/keel/skills"
)

// skillUsage is keel skill's arguments, for keel help skill.
const skillUsage = "install [-user | -to dir] [-force] | print [file]"

// skillCommand is keel skill: it installs or prints the coding-agent skill embedded in this
// version of keel.
func skillCommand() keelcli.Command {
	return keelcli.Command{
		Name:    "skill",
		Usage:   skillUsage,
		Summary: "install the keel skill for coding agents (Claude Code) into .claude/skills, or print it",
		Run: func(_ context.Context, env *keelcli.Env, args []string) error {
			err := runSkill(args, env.Stdout, env.Stderr)
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		},
	}
}

// runSkill runs keel skill.
func runSkill(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return keelcli.Usagef("keel skill: name a subcommand: %s", skillUsage)
	}
	switch args[0] {
	case "install":
		return skillInstall(args[1:], stdout, stderr)
	case "print":
		return skillPrint(args[1:], stdout)
	default:
		return keelcli.Usagef("keel skill: unknown subcommand %q: %s", args[0], skillUsage)
	}
}

// skillInstall writes the skill into a skills directory: the project's .claude/skills by default.
func skillInstall(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("skill install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	user := flags.Bool("user", false, "install for every project of this user, in ~/.claude/skills")
	to := flags.String("to", "", "skills `directory` to install into (default .claude/skills at the module root)")
	force := flags.Bool("force", false, "write over a keel directory that is not a keel skill")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return keelcli.Usagef("keel skill install: unexpected argument %q", flags.Arg(0))
	}
	if *user && *to != "" {
		return keelcli.Usagef("keel skill install: -user and -to exclude each other")
	}
	dir := *to
	switch {
	case *user:
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".claude", "skills")
	case dir == "":
		root, err := projectRoot()
		if err != nil {
			return err
		}
		dir = filepath.Join(root, ".claude", "skills")
	}
	files, err := skills.Install(dir, *force)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, skills.Name)
	if v := keelVersion(); v != "" {
		fmt.Fprintf(stdout, "installed the keel skill for keel %s in %s (%d files)\n", v, target, len(files))
	} else {
		fmt.Fprintf(stdout, "installed the keel skill in %s (%d files)\n", target, len(files))
	}
	return nil
}

// skillPrint writes one file of the skill, SKILL.md by default, to stdout.
func skillPrint(args []string, stdout io.Writer) error {
	name := "SKILL.md"
	switch len(args) {
	case 0:
	case 1:
		name = args[0]
	default:
		return keelcli.Usagef("keel skill print: name at most one file")
	}
	b, err := skills.Read(name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid) {
		return keelcli.Usagef("keel skill print: the skill has no file %q", name)
	}
	if err != nil {
		return err
	}
	_, err = stdout.Write(b)
	return err
}

// projectRoot returns the nearest directory at or above the working directory that holds go.mod,
// or the working directory when none does.
func projectRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return wd, nil
		}
		dir = parent
	}
}

// keelVersion is the version of the keel module this binary was built from: the main module's
// under go run …@version, or the dependency's under go tool in an application's module.
func keelVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	const module = "github.com/emoss08/keel"
	if bi.Main.Path == module {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		return ""
	}
	for _, d := range bi.Deps {
		if d.Path == module {
			return d.Version
		}
	}
	return ""
}
