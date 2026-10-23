// Command demo is a keelcli database binary over the fake engine, for trying the console and the
// live views without a database.
//
//	go run ./keelcli/cmd/demo            # the console
//	go run ./keelcli/cmd/demo up         # the deploy step's live view
//
// KEEL_DEMO_SPEED scales simulated time (default 1; 0 is instant). KEEL_DEMO lists scenario
// switches, comma separated: drift (a changed and a missing migration), fail-migration, fail-seed,
// production (the database is marked production), no-lock-wait.
package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/emoss08/keel/keelcli"
	"github.com/emoss08/keel/keelcli/fake"
	"github.com/emoss08/keel/keelcli/seedport"
)

func main() {
	opts := fake.Options{Speed: 1}
	if v, err := strconv.ParseFloat(os.Getenv("KEEL_DEMO_SPEED"), 64); err == nil {
		opts.Speed = v
	}
	for _, s := range strings.Split(os.Getenv("KEEL_DEMO"), ",") {
		switch strings.TrimSpace(s) {
		case "drift":
			opts.Drift = true
		case "fail-migration":
			opts.FailMigration = true
		case "fail-seed":
			opts.FailSeed = true
		case "production":
			opts.Environment = seedport.Production
		case "no-lock-wait":
			opts.NoLockWait = true
		}
	}
	engine := fake.New(opts)
	keelcli.Main(keelcli.App{
		Name:        "trenova-devdb",
		Description: "Trenova's development database: migrations, reference seeds and fixtures.",
		Version:     "0.0.0-demo",
		Migrations:  engine.Migrations(),
		Seeds:       engine.Seeds(),
		Facts:       engine.Facts(),
		Extensions:  []string{"pg_trgm", "btree_gin", "pgcrypto"},
	})
}
