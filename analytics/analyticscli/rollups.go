package analyticscli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/keelcli"
)

// rollups dispatches the rollups commands.
func (e *env) rollups(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return keelcli.Usagef("analytics rollups: name a command: migration, refresh or pending")
	}
	switch args[0] {
	case "migration":
		return e.rollupMigration(args[1:])
	case "refresh":
		return e.rollupRefresh(ctx, args[1:])
	case "pending":
		return e.rollupPending(ctx, args[1:])
	}
	return keelcli.Usagef("analytics rollups: unknown command %q: want migration, refresh or pending", args[0])
}

// rollupMigration writes or checks the rollups' migrations.
func (e *env) rollupMigration(args []string) error {
	dir, check, err := e.outputFlags("rollups migration", args, true)
	if err != nil {
		return err
	}
	if dir == "" {
		return keelcli.Usagef("analytics rollups migration: -out is required")
	}
	if check {
		if err := rollup.CheckMigrations(dir, e.cfg.Rollups...); err != nil {
			return problems(err)
		}
		return e.wrote(dir, true)
	}
	paths, err := rollup.WriteMigrations(dir, e.cfg.Rollups...)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		_, err := fmt.Fprintf(e.out, "every rollup's migration in %s is up to date\n", dir)
		return err
	}
	for _, p := range paths {
		if err := e.wrote(p, false); err != nil {
			return err
		}
	}
	return nil
}

// rollupRefresh refreshes the selected rollups, marking every tenant first with -rebuild.
func (e *env) rollupRefresh(ctx context.Context, args []string) error {
	fs := e.flags("analytics rollups refresh")
	names := fs.String("rollup", "", "comma-separated rollups to refresh; default all")
	batch := fs.Int("batch", e.cfg.Refresh.BatchSize, "dirty rows claimed per transaction")
	maxBatches := fs.Int("max-batches", e.cfg.Refresh.MaxBatches, "transactions to run at most; 0 runs until done")
	rebuild := fs.Bool("rebuild", false, "mark every tenant for a whole rebuild first")
	if err := e.parse(fs, args); err != nil {
		return err
	}
	selected, err := e.pick(*names)
	if err != nil {
		return err
	}
	db, err := e.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if *rebuild {
		if _, err := rollup.MarkAll(ctx, db, selected...); err != nil {
			return err
		}
	}
	o := e.cfg.Refresh
	o.BatchSize, o.MaxBatches = *batch, *maxBatches
	rep, err := rollup.RefreshWith(ctx, db, o, selected...)
	if err != nil {
		return err
	}
	if e.json() {
		return json.NewEncoder(e.out).Encode(rep)
	}
	_, err = fmt.Fprintf(e.out, "refreshed %d slices and %d whole tenants, %d rows, in %d transactions (%s); %d slices left to other workers\n",
		rep.Slices, rep.Tenants, rep.Rows, rep.Batches, rep.Duration.Round(time.Millisecond), rep.Skipped)
	return err
}

// rollupPending prints the dirty rows waiting for each rollup.
func (e *env) rollupPending(ctx context.Context, args []string) error {
	fs := e.flags("analytics rollups pending")
	names := fs.String("rollup", "", "comma-separated rollups; default all")
	if err := e.parse(fs, args); err != nil {
		return err
	}
	selected, err := e.pick(*names)
	if err != nil {
		return err
	}
	db, err := e.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	pending, err := rollup.Pending(ctx, db, selected...)
	if err != nil {
		return err
	}
	if e.json() {
		return json.NewEncoder(e.out).Encode(pending)
	}
	for _, r := range selected {
		if _, err := fmt.Fprintf(e.out, "%s\t%d\n", r.Name(), pending[r.Name()]); err != nil {
			return err
		}
	}
	return nil
}

// pick returns the rollups named in a comma-separated list, or every rollup for an empty one.
func (e *env) pick(list string) ([]*rollup.Rollup, error) {
	if len(e.cfg.Rollups) == 0 {
		return nil, keelcli.Usagef("analytics rollups: the application declares no rollups")
	}
	if strings.TrimSpace(list) == "" {
		return e.cfg.Rollups, nil
	}
	var out []*rollup.Rollup
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		i := slices.IndexFunc(e.cfg.Rollups, func(r *rollup.Rollup) bool { return r.Name() == name })
		if i < 0 {
			return nil, keelcli.Usagef("analytics rollups: unknown rollup %q", name)
		}
		out = append(out, e.cfg.Rollups[i])
	}
	return out, nil
}

// open opens the database through Config.Open.
func (e *env) open(ctx context.Context) (*keel.DB, error) {
	if e.cfg.Open == nil {
		return nil, errors.New("analytics rollups: Config.Open is not set")
	}
	db, err := e.cfg.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("analytics rollups: open the database: %w", err)
	}
	return db, nil
}
