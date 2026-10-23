package keelcli

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/internal/ui/console"
)

func (s *session) consoleModel(ctx context.Context) console.Model {
	return console.New(console.Options{
		Name:          s.app.Name,
		Version:       s.app.Version,
		Migrations:    s.app.Migrations,
		Seeds:         s.app.Seeds,
		Facts:         s.app.Facts,
		Extensions:    s.app.Extensions,
		YesProduction: s.g.yesProduction,
		Theme:         s.programTheme(),
		Now:           s.st.now,
		Context:       ctx,
	})
}

func (s *session) console(ctx context.Context) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if _, err := tea.NewProgram(s.consoleModel(ctx), s.programOptions(consoleHeight)...).Run(); err != nil {
		return fmt.Errorf("keelcli: console: %w", err)
	}
	return nil
}
