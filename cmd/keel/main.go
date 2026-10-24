// Command keel generates typed data-access packages for keel models and runs migrations.
//
//	keel gen [-out {dir}/{name}db] [-kinds keelkinds] [-check] ./internal/...
//	keel migrate -dsn postgres://... -dir migrations up|down|status
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/emoss08/keel/gen"
	"github.com/emoss08/keel/migrate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = runGen(os.Args[2:])
	case "migrate":
		err = runMigrate(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "keel:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  keel gen [-out {dir}/{name}db] [-kinds keelkinds] [-tags a,b] [-check] packages...
  keel migrate -dsn DSN -dir DIR up|down|status|create NAME`)
}

func runGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	out := fs.String("out", "{dir}/{name}db", "generated package directory, relative to the module root")
	kinds := fs.String("kinds", "keelkinds", "shared column kinds package directory, relative to the module root")
	tags := fs.String("tags", "", "comma-separated build tags")
	check := fs.Bool("check", false, "fail when generated files are out of date instead of writing them")
	acronyms := fs.String("acronyms", "", "comma-separated words labels write in capitals (default ID,URL,API,JSON,UUID)")
	_ = fs.Parse(args)
	patterns := fs.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	o := gen.Options{Patterns: patterns, Out: *out, Kinds: *kinds}
	if *tags != "" {
		o.Tags = strings.Split(*tags, ",")
	}
	if *acronyms != "" {
		o.Acronyms = strings.Split(*acronyms, ",")
	}
	files, err := gen.Generate(o)
	if err != nil {
		return err
	}
	if *check {
		if stale := gen.Stale(files); len(stale) > 0 {
			return fmt.Errorf("generated files are out of date; run keel gen:\n  %s", strings.Join(stale, "\n  "))
		}
		return nil
	}
	changed, err := gen.Write(files)
	for _, c := range changed {
		fmt.Println("wrote", c)
	}
	return err
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "connection string")
	dir := fs.String("dir", "migrations", "directory of .sql migrations")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("migrate needs up, down, status or create")
	}
	ctx := context.Background()
	if rest[0] == "create" {
		if len(rest) < 2 {
			return fmt.Errorf("migrate create needs a name")
		}
		paths, err := migrate.Create(*dir, rest[1], len(rest) > 2 && rest[2] == "notx")
		for _, p := range paths {
			fmt.Println("created", p)
		}
		return err
	}
	m, err := migrate.Open(ctx, *dsn, os.DirFS(*dir))
	if err != nil {
		return err
	}
	defer m.Close()
	switch rest[0] {
	case "up":
		applied, err := m.Up(ctx)
		for _, a := range applied {
			fmt.Println("applied", a)
		}
		return err
	case "down":
		reverted, err := m.Down(ctx, 1)
		for _, r := range reverted {
			fmt.Println("reverted", r)
		}
		return err
	case "status":
		st, err := m.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range st {
			mark := " "
			if s.Applied {
				mark = "x"
			}
			fmt.Printf("[%s] %s\n", mark, s.Name)
		}
		return nil
	}
	return fmt.Errorf("unknown migrate command %q", rest[0])
}
