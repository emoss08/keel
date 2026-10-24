// Package guard decides whether a destructive action may run against a database: it reads the
// database and its environment mark and refuses a production-marked database unless the user
// said so. The commands and the console share it, so both refuse the same things.
package guard

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/seedport"
)

// ErrRefused marks a refusal that a flag would lift: keelcli exits with its usage code.
var ErrRefused = errors.New("keelcli: refused")

// ErrDeclined marks a confirmation the user answered no.
var ErrDeclined = errors.New("keelcli: not confirmed")

// Target is the database an action would change.
type Target struct {
	Database    string
	Host        string
	Port        int
	DSN         string
	Environment seedport.Environment
}

// Name is the database and where it lives.
func (t Target) Name() string {
	switch {
	case t.Database == "":
		return "the database"
	case t.Host == "":
		return t.Database
	case t.Port == 0:
		return t.Database + " on " + t.Host
	default:
		return fmt.Sprintf("%s on %s:%d", t.Database, t.Host, t.Port)
	}
}

// Mark is the environment mark as a word.
func (t Target) Mark() string {
	if t.Environment == "" {
		return "unmarked"
	}
	return string(t.Environment)
}

// Production reports whether the database is marked production.
func (t Target) Production() bool { return t.Environment == seedport.Production }

// Sources are where the target is read from; either may be nil.
type Sources struct {
	Facts factsport.Facts
	Seeds seedport.Environments
}

// Read reads the target from the facts port, falling back to the seed engine's environment.
func Read(ctx context.Context, src Sources) (Target, error) {
	var t Target
	if src.Facts != nil {
		c, err := src.Facts.Connection(ctx)
		if err != nil {
			return t, wrap("connect", err)
		}
		t = Target{Database: c.Database, Host: c.Host, Port: c.Port, DSN: render.DSN(c.DSN)}
		env, err := src.Facts.Environment(ctx)
		if err != nil {
			return t, wrap("read environment", err)
		}
		t.Environment = env
		return t, nil
	}
	if src.Seeds != nil {
		env, err := src.Seeds.Environment(ctx)
		if err != nil {
			return t, wrap("read environment", err)
		}
		t.Environment = env
	}
	return t, nil
}

// wrap names what failed, unless the error already says it comes from keelcli.
func wrap(what string, err error) error {
	if strings.HasPrefix(err.Error(), "keelcli:") {
		return err
	}
	return fmt.Errorf("keelcli: %s: %w", what, err)
}

// Action is a destructive action. Verb completes "refusing to …"; Never refuses production
// whatever the flags say.
type Action struct {
	Verb   string
	Title  string
	Detail []string
	Never  bool
}

// Check refuses the action against a production database unless yesProduction is set, and always
// when the action is Never.
func Check(t Target, a Action, yesProduction bool) error {
	if !t.Production() {
		return nil
	}
	if a.Never {
		return refusal(fmt.Sprintf("keelcli: refusing to %s: %s is marked production, where it never runs", a.Verb, t.Name()))
	}
	if !yesProduction {
		return refusal(fmt.Sprintf("keelcli: refusing to %s: %s is marked production; pass -yes-production to proceed", a.Verb, t.Name()))
	}
	return nil
}

type refusal string

// Error returns the message.
func (r refusal) Error() string { return string(r) }

// Is matches ErrRefused.
func (r refusal) Is(target error) bool { return target == ErrRefused }

// Description is the confirmation text: the database, its mark and the action's detail.
func Description(t Target, a Action) string {
	lines := []string{"Database: " + t.Name(), "Environment: " + t.Mark()}
	if t.Production() {
		lines[1] += "  (PRODUCTION)"
	}
	lines = append(lines, a.Detail...)
	return strings.Join(lines, "\n")
}
