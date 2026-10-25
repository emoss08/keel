package keelcli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/keelcli/internal/plans"
	"github.com/emoss08/keel/keelcli/internal/render"
)

// plansDoc is plans' JSON document: the shapes explained, the findings and the shapes EXPLAIN
// could not plan, and every shape's report.
type plansDoc struct {
	Shapes    int            `json:"shapes"`
	Findings  int            `json:"findings"`
	Unplanned int            `json:"unplanned"`
	Reports   []plans.Report `json:"reports"`
}

func setupPlans(fs *flag.FlagSet) runner {
	path := fs.String("shapes", "", "JSON lines `file` of statement shapes, as keel.RecordShapesFile writes it; - reads stdin")
	seqRows := fs.Float64("seq-scan-rows", 10000, "flag sequential scans of relations with more than `rows` rows (pg_class.reltuples); -1 flags every one")
	maxCost := fs.Float64("max-cost", 0, "flag plans whose total cost exceeds `cost`; 0 checks no cost")
	sortLimit := fs.Bool("sort-limit", false, "flag a Sort under a Limit: a page sorted without an index")
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("plans", args); err != nil {
			return err
		}
		if *path == "" {
			return Usagef("keelcli: plans needs -shapes: the file keel.RecordShapesFile wrote")
		}
		shapes, err := s.readShapes(*path)
		if err != nil {
			return err
		}
		rules := plans.Rules{SeqScanRows: *seqRows, MaxCost: *maxCost, SortLimit: *sortLimit}
		var reports []plans.Report
		if len(shapes) > 0 {
			conn, err := s.database().Connect(ctx)
			if err != nil {
				return err
			}
			defer conn.Close(context.WithoutCancel(ctx))
			if err := s.busy(ctx, "Planning "+render.Plural(len(shapes), "shape", "shapes"), func(ctx context.Context) (err error) {
				reports, err = plans.Run(ctx, conn, shapes, rules)
				return err
			}); err != nil {
				return err
			}
		}
		doc := plansDoc{Shapes: len(shapes), Reports: reports}
		if doc.Reports == nil {
			doc.Reports = []plans.Report{}
		}
		for _, r := range reports {
			doc.Findings += len(r.Findings)
			if !r.Planned() {
				doc.Unplanned++
			}
		}
		if err := s.emit(doc, func(p render.Painter, _ int) string { return plansText(p, doc) }); err != nil {
			return err
		}
		if doc.Findings > 0 {
			return problemsf("keelcli: plans found %s", render.Plural(doc.Findings, "finding", "findings"))
		}
		return nil
	}
}

// readShapes reads the shapes file at path, or stdin for -.
func (s *session) readShapes(path string) ([]keel.Shape, error) {
	var r io.Reader = s.st.in
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("keelcli: plans: %w", err)
		}
		defer f.Close()
		r = f
	}
	shapes, err := keel.ReadShapes(r)
	if err != nil {
		return nil, fmt.Errorf("keelcli: plans: %w", err)
	}
	return shapes, nil
}

// plansText writes plans' plain output: one block per shape with findings or without a plan, its
// operation and table, the statement and each finding or the error, then a summary line.
func plansText(p render.Painter, doc plansDoc) string {
	if doc.Shapes == 0 {
		return p.Paint(render.Muted, "No shapes to plan.") + "\n"
	}
	var b strings.Builder
	flagged := 0
	for _, r := range doc.Reports {
		switch {
		case !r.Planned():
			fmt.Fprintf(&b, "%s %s\n  %s\n  %s\n", p.Paint(render.Warn, "unplanned"), p.Paint(render.Strong, shapeName(r.Shape)),
				render.OneLine(r.SQL), r.Error)
		case len(r.Findings) > 0:
			flagged++
			fmt.Fprintf(&b, "%s %s %s\n  %s\n", p.Paint(render.Fail, "findings"), p.Paint(render.Strong, shapeName(r.Shape)),
				p.Paint(render.Muted, fmt.Sprintf("(cost %.2f, %s rows)", r.Cost, render.Count(int64(r.Rows)))), render.OneLine(r.SQL))
			for _, f := range r.Findings {
				fmt.Fprintf(&b, "  %s %s\n", p.Paint(render.Fail, string(f.Kind)), f.Detail)
			}
		}
	}
	planned := render.Plural(doc.Shapes-doc.Unplanned, "shape", "shapes") + " planned"
	if doc.Findings == 0 {
		fmt.Fprintf(&b, "%s %s", p.Paint(render.OK, "No findings:"), planned)
	} else {
		fmt.Fprintf(&b, "%s in %d of %s", p.Paint(render.Fail, render.Plural(doc.Findings, "finding", "findings")), flagged, planned)
	}
	if doc.Unplanned > 0 {
		fmt.Fprintf(&b, "; %s", p.Paint(render.Warn, render.Plural(doc.Unplanned, "shape", "shapes")+" could not be planned"))
	}
	b.WriteString(".\n")
	return b.String()
}

// shapeName names a shape by its operation and table.
func shapeName(s keel.Shape) string {
	return strings.TrimSpace(s.Op + " " + s.Table)
}
