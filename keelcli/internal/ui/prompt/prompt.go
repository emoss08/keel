// Package prompt holds keelcli's Huh forms: the confirmation of a destructive action and the
// questions migrate create asks. The commands run them standalone; the console embeds them.
package prompt

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
	"github.com/emoss08/keel/keelcli/migrateport"
)

// ErrAborted is returned when the user leaves a form without answering.
var ErrAborted = errors.New("keelcli: prompt aborted")

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Keys are the form key bindings: Huh's defaults, with esc also leaving the form.
func Keys() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	k.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))
	return k
}

// Confirm is the form that confirms a destructive action; the answer lands in ok.
func Confirm(t theme.Theme, target guard.Target, a guard.Action, ok *bool) *huh.Form {
	affirmative := "Yes, proceed"
	if target.Production() {
		affirmative = "Yes, on production"
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(a.Title).
			Description(guard.Description(target, a)).
			Affirmative(affirmative).
			Negative("Cancel").
			Value(ok),
	)).WithTheme(t.Huh()).WithKeyMap(Keys()).WithShowHelp(true)
}

// ValidateName checks a migration name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("use lower case letters, digits and underscores, starting with a letter")
	}
	return nil
}

// Create is the form that names a new migration and picks its kind.
func Create(t theme.Theme, name *string, kind *migrateport.CreateKind) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Migration name").
			Description("Lower case words joined by underscores, such as add_shipment_holds.").
			Placeholder("add_shipment_holds").
			Validate(ValidateName).
			Value(name),
		huh.NewSelect[migrateport.CreateKind]().
			Title("Kind").
			Options(
				huh.NewOption("Transactional: runs in one transaction with its bookkeeping row", migrateport.Transactional),
				huh.NewOption("notx: statement by statement, for CREATE INDEX CONCURRENTLY and ALTER TYPE … ADD VALUE", migrateport.NoTx),
				huh.NewOption("Repeatable: re-applied whenever it changes, for functions, views and grants", migrateport.RepeatableFile),
			).
			Value(kind),
	)).WithTheme(t.Huh()).WithKeyMap(Keys()).WithShowHelp(true)
}

// Run runs a form on its own, with the program options that set its streams, until it is
// answered, aborted or ctx ends.
func Run(ctx context.Context, f *huh.Form, opts ...tea.ProgramOption) error {
	err := f.WithProgramOptions(opts...).RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrAborted
	}
	if err != nil {
		return fmt.Errorf("keelcli: prompt: %w", err)
	}
	return nil
}
