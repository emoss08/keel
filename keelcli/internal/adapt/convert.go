package adapt

import (
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/seed"
)

// migrationKind converts a migration kind.
func migrationKind(k migrate.Kind) migrateport.Kind {
	switch k {
	case migrate.KindRepeatable:
		return migrateport.Repeatable
	case migrate.KindGo:
		return migrateport.Go
	case migrate.KindBaseline:
		return migrateport.Baseline
	default:
		return migrateport.Versioned
	}
}

// migration converts a migration.
func migration(m migrate.Migration) migrateport.Migration {
	return migrateport.Migration{
		Version: m.Version,
		Name:    m.Name,
		Source:  m.Source,
		Kind:    migrationKind(m.Kind),
		Directives: migrateport.Directives{
			NoTx:             m.Directives.NoTx,
			LockTimeout:      m.Directives.LockTimeout,
			StatementTimeout: m.Directives.StatementTimeout,
			NoRetry:          m.Directives.NoRetry,
		},
		Checksum: m.Checksum,
		SQL:      m.SQL,
	}
}

// migrations converts migrations.
func migrations(ms []migrate.Migration) []migrateport.Migration {
	out := make([]migrateport.Migration, len(ms))
	for i, m := range ms {
		out[i] = migration(m)
	}
	return out
}

// migrationState converts a migration state.
func migrationState(s migrate.State) migrateport.State {
	switch s {
	case migrate.StatePending:
		return migrateport.Pending
	case migrate.StateChanged:
		return migrateport.Changed
	case migrate.StateRepeatableDue:
		return migrateport.RepeatableDue
	case migrate.StateMissing:
		return migrateport.Missing
	default:
		return migrateport.Applied
	}
}

// statuses converts migration statuses.
func statuses(ss []migrate.Status) []migrateport.Status {
	out := make([]migrateport.Status, len(ss))
	for i, s := range ss {
		out[i] = migrateport.Status{
			Migration:       migration(s.Migration),
			State:           migrationState(s.State),
			AppliedAt:       s.AppliedAt,
			Duration:        s.Duration,
			AppliedChecksum: s.AppliedChecksum,
		}
	}
	return out
}

// records converts bookkeeping rows.
func records(rs []migrate.Record) []migrateport.Record {
	out := make([]migrateport.Record, len(rs))
	for i, r := range rs {
		out[i] = migrateport.Record{
			Version: r.Version, Name: r.Name, Source: r.Source, Checksum: r.Checksum,
			AppliedAt: r.AppliedAt, Duration: r.Duration,
		}
	}
	return out
}

// plan converts a plan; the port runs Pending and then Repeatables, and calls Marks Baseline.
func plan(p migrate.Plan) migrateport.Plan {
	return migrateport.Plan{
		Migrations: migrations(append(append([]migrate.Migration(nil), p.Pending...), p.Repeatables...)),
		Baseline:   migrations(p.Marks),
		After:      p.After,
	}
}

// blockers converts the backends a run waits on.
func blockers(bs []migrate.Blocker) []migrateport.Blocker {
	if len(bs) == 0 {
		return nil
	}
	out := make([]migrateport.Blocker, len(bs))
	for i, b := range bs {
		out[i] = migrateport.Blocker{PID: b.PID, Application: b.Application, State: b.State, Mode: b.Mode, Age: b.Age, Query: b.Query}
	}
	return out
}

// findings converts lint findings.
func findings(fs []migrate.Finding) []migrateport.Finding {
	out := make([]migrateport.Finding, len(fs))
	for i, f := range fs {
		sev := migrateport.Warning
		if f.Severity == migrate.SeverityError {
			sev = migrateport.Error
		}
		out[i] = migrateport.Finding{Rule: f.Rule, Migration: migration(f.Migration), Line: f.Line, Message: f.Message, Severity: sev}
	}
	return out
}

// seedKind converts a seed kind.
func seedKind(k seed.Kind) seedport.Kind {
	if k == seed.Fixture {
		return seedport.Fixture
	}
	return seedport.Reference
}

// seedDef converts a registered seed, naming the seeds it runs after.
func seedDef(s *seed.Seed) seedport.Seed {
	after := make([]string, 0, len(s.After))
	for _, a := range s.After {
		after = append(after, a.Name)
	}
	return seedport.Seed{Name: s.Name, Kind: seedKind(s.Kind), Description: s.Description, After: after, PerTenant: s.PerTenant}
}

// reasonKind converts the reason a seed runs or does not.
func reasonKind(r seed.Reason) seedport.ReasonKind {
	switch r {
	case seed.InputChanged:
		return seedport.InputChanged
	case seed.VersionChanged:
		return seedport.VersionChanged
	case seed.CodeChanged:
		return seedport.CodeChanged
	case seed.Forced:
		return seedport.Forced
	case seed.UpToDate:
		return seedport.UpToDate
	case seed.FixtureApplied:
		return seedport.FixtureApplied
	case seed.Excluded:
		return seedport.Excluded
	case seed.Refused:
		return seedport.Refused
	default:
		return seedport.New
	}
}

// reason converts a reason with the inputs that changed.
func reason(r seed.Reason, changed []string) seedport.Reason {
	return seedport.Reason{Kind: reasonKind(r), Inputs: changed}
}

// planItems converts a seed plan.
func planItems(p seed.Plan) []seedport.PlanItem {
	out := make([]seedport.PlanItem, len(p.Seeds))
	for i, it := range p.Seeds {
		out[i] = seedport.PlanItem{Seed: seedDef(it.Seed), Reason: reason(it.Reason, it.Changed)}
	}
	return out
}

// seedStatus converts a seed status: a removed seed is orphaned, and a per-tenant seed some tenants
// applied with its current fingerprint is partial.
func seedStatus(s seed.Status) seedport.Status {
	out := seedport.Status{
		Reason: reason(s.Reason, s.Changed), AppliedAt: s.AppliedAt, Duration: s.Duration, Rows: s.Rows, Tenants: s.Tenants,
	}
	if s.Seed == nil {
		out.Seed = seedport.Seed{Name: s.Name, Kind: seedKind(s.Kind)}
		out.State, out.Reason = seedport.Orphaned, seedport.Reason{Kind: seedport.Excluded}
		return out
	}
	out.Seed = seedDef(s.Seed)
	switch {
	case s.State == seed.Applied:
		out.State = seedport.Applied
	case s.Seed.PerTenant && s.Tenants > 0:
		out.State = seedport.Partial
	default:
		out.State = seedport.Due
	}
	return out
}
