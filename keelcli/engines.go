package keelcli

import (
	"errors"
	"strings"

	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/internal/adapt"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/seed"
)

// AdminEnv names the environment variable that overrides the admin DSN, which keelcli otherwise
// derives from the application's DSN by connecting to the postgres database.
const AdminEnv = "KEEL_ADMIN_DSN"

// engines are the ports a session runs on; envs records the environment mark.
type engines struct {
	migrations migrateport.Engine
	seeds      seedport.Engine
	facts      factsport.Facts
	envs       seedport.Environments
}

// given reports whether s was set, rather than left the zero Set.
func given(s migrate.Set) bool { return s.Err() != nil || s.Fingerprint() != "" }

// hasMigrations reports whether the App names its migrations.
func (s *session) hasMigrations() bool {
	a := s.app
	return a.Engines.Migrations != nil || given(a.Migrations) || len(a.Dirs) > 0
}

// hasSeeds reports whether the App has seeds, without building the engines.
func (s *session) hasSeeds() bool { return s.app.Engines.Seeds != nil || s.app.Seeds != nil }

// hasFacts reports whether the App can read database facts, without building the engines.
func (s *session) hasFacts() bool { return s.app.Engines.Facts != nil || s.app.Config != nil }

// database is the session's database, opened on first use.
func (s *session) database() *adapt.Database {
	if s.db == nil {
		s.db = adapt.NewDatabase(adapt.DatabaseOptions{
			Name: s.app.Name, Config: s.app.Config, Parallel: s.app.Parallel, Admin: s.adminDSN,
		})
	}
	return s.db
}

// adminDSN is the admin DSN given by -admin or KEEL_ADMIN_DSN, or "".
func (s *session) adminDSN() string {
	if s.admin != "" {
		return s.admin
	}
	for _, kv := range s.st.environ {
		if v, ok := strings.CutPrefix(kv, AdminEnv+"="); ok {
			return v
		}
	}
	return ""
}

// engines builds the engines the App does not replace, once.
func (s *session) engines() *engines {
	if s.eng != nil {
		return s.eng
	}
	a := s.app
	e := &engines{migrations: a.Engines.Migrations, seeds: a.Engines.Seeds, facts: a.Engines.Facts}
	if e.migrations == nil {
		e.migrations = adapt.NewMigrations(s.migrationsConfig())
	}
	if e.seeds == nil && a.Seeds != nil {
		e.seeds = &adapt.Seeds{Environments: adapt.Environments{DB: s.database()}, Registry: a.Seeds, Options: s.seedOptions()}
	}
	if e.facts == nil && a.Config != nil {
		e.facts = adapt.Facts{Environments: adapt.Environments{DB: s.database()}}
	}
	switch {
	case e.seeds != nil:
		e.envs = e.seeds
	case a.Config != nil:
		e.envs = adapt.Environments{DB: s.database()}
	}
	s.eng = e
	return e
}

// seedOptions are the seed options every run uses.
func (s *session) seedOptions() seed.Options {
	return seed.Options{Tenants: s.app.Tenants, System: s.app.System, Parallel: s.app.Parallel}
}

// migrationsConfig configures the migration adapter from the App.
func (s *session) migrationsConfig() adapt.MigrationsConfig {
	a := s.app
	set, dirs := a.Migrations, a.Dirs
	read := func() migrate.Set { return set }
	if !given(set) {
		read = func() migrate.Set { return adapt.DirSet(dirs) }
	}
	return adapt.MigrationsConfig{
		Name: a.Name, DB: s.database(), Set: read, Options: a.Migrate, Dirs: dirs, Stamp: a.Stamp, Dump: a.Dump,
		Lint: a.Lint, Seeds: a.Seeds, SeedOptions: s.seedOptions(), Now: s.st.now,
	}
}

// migrations is the migration engine.
func (s *session) migrations() migrateport.Engine { return s.engines().migrations }

// seeds is the seed engine, nil without seeds.
func (s *session) seeds() seedport.Engine { return s.engines().seeds }

// facts reads database facts, nil when the App cannot.
func (s *session) facts() factsport.Facts { return s.engines().facts }

// environments records the environment mark, nil when the App cannot.
func (s *session) environments() seedport.Environments { return s.engines().envs }

// setup runs the App's Setup once and checks the App names its migrations.
func (s *session) setup() error {
	if s.ready {
		return nil
	}
	s.ready = true
	if s.app.Setup != nil {
		if err := s.app.Setup(s.app); err != nil {
			return err
		}
	}
	if !s.hasMigrations() {
		return errors.New("keelcli: App.Migrations is required: set it, or Dirs, or Engines.Migrations")
	}
	return nil
}

// close closes the database the session opened.
func (s *session) close() {
	if s.db != nil {
		s.db.Close()
	}
}
