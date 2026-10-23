package seed

import (
	"context"
	"errors"
	"fmt"

	"github.com/emoss08/keel"
)

// Env is the environment a database serves. A database records its environment once, in keel_meta,
// and Apply refuses to run Fixture seeds against one marked Production.
type Env string

const (
	// Production is a database that serves real users.
	Production Env = "production"
	// Staging is a production-like database for release testing.
	Staging Env = "staging"
	// Development is a developer's database.
	Development Env = "development"
	// Test is a database made for automated tests.
	Test Env = "test"
)

// ErrEnvironment is returned when an environment differs from the one recorded on the database.
var ErrEnvironment = errors.New("seed: environment mismatch")

const envKey = "environment"

// ParseEnv returns the environment named s: production, staging, development or test.
func ParseEnv(s string) (Env, error) {
	switch e := Env(s); e {
	case Production, Staging, Development, Test:
		return e, nil
	}
	return "", fmt.Errorf("seed: unknown environment %q (production, staging, development or test)", s)
}

// MarkEnvironment records env as the database's environment. Recording the environment it already
// has does nothing; recording another one is an ErrEnvironment error unless force is set.
func MarkEnvironment(ctx context.Context, db *keel.DB, env Env, force bool) error {
	if _, err := ParseEnv(string(env)); err != nil {
		return err
	}
	return keel.Write(ctx, db, func(ctx context.Context) error {
		if err := keel.LockKey(ctx, db, lockKey); err != nil {
			return err
		}
		if err := createTables(ctx, db); err != nil {
			return err
		}
		if force {
			_, err := keel.Exec(ctx, db, upsertMeta, envKey, string(env))
			return err
		}
		n, err := keel.Exec(ctx, db, insertMeta, envKey, string(env))
		if err != nil || n == 1 {
			return err
		}
		cur, err := Environment(ctx, db)
		if err != nil {
			return err
		}
		return checkEnv(env, cur)
	})
}

// Environment returns the environment recorded on the database, or "" when none is.
func Environment(ctx context.Context, db *keel.DB) (Env, error) {
	st, err := loadState(ctx, db.Pool())
	if err != nil {
		return "", err
	}
	return st.env, nil
}

// checkEnv refuses a requested environment that differs from the recorded one.
func checkEnv(want, recorded Env) error {
	if want != "" {
		if _, err := ParseEnv(string(want)); err != nil {
			return err
		}
	}
	if want == "" || recorded == "" || want == recorded {
		return nil
	}
	return fmt.Errorf("%w: the database is marked %s, not %s", ErrEnvironment, recorded, want)
}
