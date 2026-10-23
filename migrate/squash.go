package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
)

// squashSchema holds keel_migrations in the scratch databases, so the dump leaves it out.
const squashSchema = "keel_squash"

// Dir is a directory of migration files, named as its Source.
type Dir struct {
	Name string
	Path string
}

// SquashOptions configure Squash.
type SquashOptions struct {
	// Admin is a DSN for any database on the server; Squash creates and drops scratch databases
	// through it.
	Admin string
	// Dirs are the migration directories; the baseline is written to the first.
	Dirs []Dir
	// To is the newest version squashed; default the newest version of the directories.
	To string
	// Dump is the pg_dump command, such as ["docker", "exec", "keelpg", "pg_dump", "-U", "keel",
	// "-d", "{db}"]; default ["pg_dump"]. "{dsn}" and "{db}" in an argument are replaced by the
	// scratch database's DSN and name; without either, --dbname={dsn} is appended.
	Dump []string
	// Options configure the migrations applied to the scratch databases, such as After.
	Options Options
}

// SquashResult is what Squash wrote.
type SquashResult struct {
	// Baseline is the path of the baseline file.
	Baseline string
	// Version is the baseline's version, the newest squashed version.
	Version string
	// Squashed are the migrations the baseline replaces.
	Squashed []Migration
	// Removed are the files deleted.
	Removed []string
}

// Squash replaces the migrations up to To with one baseline file, <version>_baseline.sql marked
// --keel:baseline. It applies them to a scratch database, dumps its schema with
// pg_dump --schema-only --no-owner --no-privileges, strips what does not re-apply cleanly (psql
// meta-commands, session settings, extension comments), and verifies the result by applying it to
// a second scratch database and comparing the two dumps. Repeatable migrations are not squashed.
// A database whose history already holds the squashed versions records the baseline as applied
// without running it; an empty database runs it.
func Squash(ctx context.Context, o SquashOptions) (SquashResult, error) {
	if len(o.Dirs) == 0 {
		return SquashResult{}, errors.New("migrate: squash: no directory")
	}
	sources := make([]Source, len(o.Dirs))
	paths := map[string]string{}
	for i, d := range o.Dirs {
		sources[i] = Source{Name: d.Name, FS: os.DirFS(d.Path)}
		paths[sourceName(d.Name)] = d.Path
	}
	set := Sources(sources...)
	if set.err != nil {
		return SquashResult{}, set.err
	}
	squashed, err := squashable(set, o.To)
	if err != nil {
		return SquashResult{}, err
	}
	res := SquashResult{Squashed: squashed, Version: squashed[len(squashed)-1].Version}
	admin, err := pgx.Connect(ctx, o.Admin)
	if err != nil {
		return res, fmt.Errorf("migrate: squash: connect: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()
	sq := squasher{admin: admin, o: o}
	body, err := sq.dump(ctx, newSet(squashed))
	if err != nil {
		return res, err
	}
	content := fmt.Sprintf("--keel:baseline\n-- Squashed by keel migrate squash: %d migrations, %s to %s.\n\n%s",
		len(squashed), squashed[0].ID(), squashed[len(squashed)-1].ID(), body)
	if err := sq.verify(ctx, content, body); err != nil {
		return res, err
	}
	res.Baseline = filepath.Join(o.Dirs[0].Path, res.Version+"_baseline.sql")
	if err := writeAtomic(res.Baseline, content); err != nil {
		return res, err
	}
	for _, m := range squashed {
		p := filepath.Join(paths[m.Source], m.File)
		if p == res.Baseline {
			continue
		}
		if err := os.Remove(p); err != nil {
			return res, fmt.Errorf("migrate: squash: %w", err)
		}
		res.Removed = append(res.Removed, p)
	}
	return res, nil
}

// sourceName returns the name Sources gives a source named name.
func sourceName(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

// squashable returns the migrations up to to, excluding repeatable ones.
func squashable(set Set, to string) ([]Migration, error) {
	if to == "" {
		to = set.Version()
	}
	if to == "" || !hasVersion(set.migrations, to) {
		return nil, fmt.Errorf("migrate: squash: no migration has version %q", to)
	}
	var out []Migration
	for _, m := range set.migrations {
		if m.Kind != KindRepeatable && m.Version <= to {
			out = append(out, m)
		}
	}
	if len(out) == 1 && out[0].Kind == KindBaseline {
		return nil, fmt.Errorf("migrate: squash: nothing to squash up to %s", to)
	}
	return out, nil
}

// squasher runs scratch databases for one Squash.
type squasher struct {
	admin *pgx.Conn
	o     SquashOptions
}

// dump applies set to a scratch database and returns its cleaned schema dump.
func (sq squasher) dump(ctx context.Context, set Set) (string, error) {
	db := "keel_squash_" + randomHex(6)
	if _, err := sq.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
		return "", fmt.Errorf("migrate: squash: %w", err)
	}
	defer func() { _ = dropDatabase(context.WithoutCancel(ctx), sq.admin, db) }()
	dsn, err := withDatabase(sq.o.Admin, db)
	if err != nil {
		return "", err
	}
	opts := sq.o.Options
	opts.Schema, opts.Observer, opts.AllowOutOfOrder = squashSchema, nil, true
	m, err := Open(ctx, dsn, set, opts)
	if err != nil {
		return "", err
	}
	_, err = m.Up(ctx, UpOptions{})
	if cerr := m.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("migrate: squash: apply: %w", err)
	}
	out, err := runDump(ctx, sq.o.Dump, dsn, db)
	if err != nil {
		return "", err
	}
	return cleanDump(out), nil
}

// verify applies the baseline to a second scratch database and compares its dump with want.
func (sq squasher) verify(ctx context.Context, content, want string) error {
	m, err := parseFile("verify", "19700101000000_baseline.sql", content)
	if err != nil {
		return fmt.Errorf("migrate: squash: %w", err)
	}
	got, err := sq.dump(ctx, newSet([]Migration{m}))
	if err != nil {
		return fmt.Errorf("migrate: squash: the baseline does not apply cleanly: %w", err)
	}
	if got != want {
		return fmt.Errorf("migrate: squash: the baseline re-applies to a different schema: %s", firstDiff(want, got))
	}
	return nil
}

// runDump runs pg_dump against the scratch database.
func runDump(ctx context.Context, cmd []string, dsn, db string) (string, error) {
	if len(cmd) == 0 {
		cmd = []string{"pg_dump"}
	}
	var args []string
	placed := false
	for _, a := range cmd[1:] {
		if strings.Contains(a, "{dsn}") || strings.Contains(a, "{db}") {
			placed = true
			a = strings.NewReplacer("{dsn}", dsn, "{db}", db).Replace(a)
		}
		args = append(args, a)
	}
	args = append(args, "--schema-only", "--no-owner", "--no-privileges", "--exclude-schema="+squashSchema)
	if !placed {
		args = append(args, "--dbname="+dsn)
	}
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, cmd[0], args...)
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("migrate: squash: %s: %w: %s", cmd[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// droppedSettings are pg_dump session settings left out of a baseline: keel sets its own
// timeouts, and row_security off would leak into Options.After in the same transaction.
var droppedSettings = map[string]bool{
	"statement_timeout": true, "lock_timeout": true, "idle_in_transaction_session_timeout": true,
	"transaction_timeout": true, "row_security": true,
}

// dumpHeaders start pg_dump comment lines that name versions, which would make a baseline differ
// between machines.
var dumpHeaders = []string{
	"-- PostgreSQL database dump", "-- Dumped from database version", "-- Dumped by pg_dump version",
}

// cleanDump makes a pg_dump script apply as one migration: it drops psql meta-commands
// (\restrict, \unrestrict, \connect), version comments and the bare "--" lines framing pg_dump's
// object comments, the empty search_path, extension comments
// (which need the extension's owner) and timeout settings, makes the remaining SET statements
// local to the migration's transaction, and collapses blank lines.
func cleanDump(dump string) string {
	var b strings.Builder
	blank := false
	for _, ln := range lex(dump).lines {
		text, ok := cleanLine(ln)
		if !ok {
			continue
		}
		if strings.TrimSpace(text) == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()) + "\n"
}

// cleanLine returns the line to keep in place of ln, or false to drop it.
func cleanLine(ln srcLine) (string, bool) {
	text := strings.TrimRight(ln.text, "\r")
	if !ln.code {
		return text, true
	}
	for _, h := range dumpHeaders {
		if strings.HasPrefix(text, h) {
			return "", false
		}
	}
	switch {
	case text == "--", strings.HasPrefix(text, `\`),
		strings.HasPrefix(text, "SELECT pg_catalog.set_config('search_path'"),
		strings.HasPrefix(text, "COMMENT ON EXTENSION "):
		return "", false
	case strings.HasPrefix(text, "SET ") && !strings.HasPrefix(text, "SET LOCAL "):
		if f := strings.Fields(text); len(f) > 1 && droppedSettings[f[1]] {
			return "", false
		}
		return "SET LOCAL " + strings.TrimPrefix(text, "SET "), true
	}
	return text, true
}

// firstDiff describes the first line where a and b differ.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range max(len(al), len(bl)) {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d: %q, re-applied %q", i+1, x, y)
		}
	}
	return "no difference"
}

// writeAtomic writes path through a temporary file in the same directory.
func writeAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keel-squash-*")
	if err != nil {
		return fmt.Errorf("migrate: squash: %w", err)
	}
	_, err = tmp.WriteString(content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("migrate: squash: %w", err)
	}
	return nil
}
