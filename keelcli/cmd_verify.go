package keelcli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/keelcli/internal/render"
)

// verifyDoc is verify's JSON document: the number of tables checked and every drift.
type verifyDoc struct {
	Tables int          `json:"tables"`
	Drifts []keel.Drift `json:"drifts"`
}

func setupVerify(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("verify", args); err != nil {
			return err
		}
		tables := s.app.Tables()
		db, err := s.database().DB(ctx)
		if err != nil {
			return err
		}
		var drifts []keel.Drift
		if err := s.busy(ctx, "Verifying "+render.Plural(len(tables), "table", "tables"), func(ctx context.Context) (err error) {
			drifts, err = keel.Verify(ctx, db, tables)
			return err
		}); err != nil {
			return err
		}
		doc := verifyDoc{Tables: len(tables), Drifts: drifts}
		if doc.Drifts == nil {
			doc.Drifts = []keel.Drift{}
		}
		if err := s.emit(doc, func(p render.Painter, _ int) string { return verifyText(p, len(tables), drifts) }); err != nil {
			return err
		}
		if len(drifts) > 0 {
			return problemsf("keelcli: verify found %s", render.Plural(len(drifts), "drift", "drifts"))
		}
		return nil
	}
}

// verifyText writes verify's plain output: one block per drift, its location and kind, what
// differs and the fix.
func verifyText(p render.Painter, tables int, drifts []keel.Drift) string {
	if len(drifts) == 0 {
		return fmt.Sprintf("%s %s checked.\n", p.Paint(render.OK, "No drift:"),
			render.Plural(tables, "table", "tables"))
	}
	var b strings.Builder
	for _, d := range drifts {
		where := d.Table
		if d.Column != "" {
			where += "." + d.Column
		}
		if d.Field != "" {
			where += " (" + d.Field + ")"
		}
		fmt.Fprintf(&b, "%s %s\n  %s\n  %s %s\n", p.Paint(render.Fail, string(d.Kind)), p.Paint(render.Strong, where),
			d.Detail, p.Paint(render.Muted, "fix:"), d.Fix)
	}
	fmt.Fprintf(&b, "%s in %s.\n", p.Paint(render.Fail, render.Plural(len(drifts), "drift", "drifts")),
		render.Plural(tables, "table", "tables"))
	return b.String()
}
