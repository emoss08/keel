package fake

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

const reconcile = "SELECT trenova_rls.reconcile()"

const baselineSQL = `--keel:baseline
-- Squashed from 1,375 migrations by keel migrate squash.
CREATE SCHEMA IF NOT EXISTS trenova_rls;

CREATE TABLE organizations (
    id text PRIMARY KEY,
    name text NOT NULL,
    scac text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE shipments (
    id text PRIMARY KEY,
    organization_id text NOT NULL REFERENCES organizations (id),
    status text NOT NULL DEFAULT 'New',
    pro_number text NOT NULL,
    weight numeric(12, 2),
    created_at timestamptz NOT NULL DEFAULT now()
);
`

const shipmentHoldsSQL = `CREATE TABLE shipment_holds (
    id text PRIMARY KEY,
    organization_id text NOT NULL,
    shipment_id text NOT NULL REFERENCES shipments (id) ON DELETE CASCADE,
    reason text NOT NULL,
    released_at timestamptz
);

CREATE INDEX shipment_holds_shipment_idx ON shipment_holds (shipment_id);
`

const driverQualificationsSQL = `CREATE TABLE driver_qualifications (
    id text PRIMARY KEY,
    organization_id text NOT NULL,
    worker_id text NOT NULL,
    kind text NOT NULL,
    expires_on date NOT NULL
);
`

const ediPartnerIndexSQL = `--keel:notx
--keel:lock_timeout 2s
CREATE INDEX CONCURRENTLY edi_partner_idx ON edi_partners (organization_id, scac);
--keel:split
CREATE INDEX CONCURRENTLY edi_partner_docs_idx ON edi_documents (partner_id, received_at DESC);
`

const rateTablesSQL = `CREATE TABLE rate_tables (
    id text PRIMARY KEY,
    organization_id text NOT NULL,
    name text NOT NULL,
    effective daterange NOT NULL
);

ALTER TABLE shipments ADD COLUMN rate_table_id text REFERENCES rate_tables (id);
ALTER TABLE shipments ADD COLUMN currency text NOT NULL;
CREATE INDEX shipments_rate_table_idx ON shipments (rate_table_id);
`

const rlsHelpersSQL = `-- Re-applied whenever this file changes.
CREATE OR REPLACE FUNCTION trenova_rls.reconcile() RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    t record;
BEGIN
    FOR t IN SELECT table_name FROM information_schema.columns
             WHERE column_name = 'organization_id' AND table_schema = 'public'
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t.table_name);
    END LOOP;
END;
$$;
`

type script struct {
	duration   time.Duration
	statements []time.Duration
	retries    int
	fails      bool
}

type migration struct {
	m               migrateport.Migration
	state           migrateport.State
	appliedAt       time.Time
	duration        time.Duration
	appliedChecksum string
	script          script
}

type seedScript struct {
	duration time.Duration
	rows     int64
	failAt   int
}

type seed struct {
	s           seedport.Seed
	reason      seedport.Reason
	applied     bool
	appliedAt   time.Time
	duration    time.Duration
	rows        int64
	tenants     int
	tenantsDone int
	script      seedScript
}

func checksum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func applied(m migrateport.Migration, at time.Time, d time.Duration) *migration {
	m.Checksum = checksum(m.SQL)
	return &migration{
		m:               m,
		state:           migrateport.Applied,
		appliedAt:       at,
		duration:        d,
		appliedChecksum: m.Checksum,
	}
}

func pending(m migrateport.Migration, s script) *migration {
	m.Checksum = checksum(m.SQL + m.Name)
	return &migration{m: m, state: migrateport.Pending, script: s}
}

func demoMigrations(now time.Time, opts Options) []*migration {
	day := now.Truncate(24 * time.Hour)
	ms := []*migration{
		applied(migrateport.Migration{
			Version: "20261023143000", Name: "baseline", Source: "core",
			Kind: migrateport.Baseline, SQL: baselineSQL,
		}, day.Add(-72*time.Hour+14*time.Hour+31*time.Minute), 1800*time.Millisecond),
		applied(migrateport.Migration{
			Version: "20261024091500", Name: "shipment_holds", Source: "core", SQL: shipmentHoldsSQL,
		}, day.Add(-48*time.Hour+9*time.Hour+16*time.Minute), 42*time.Millisecond),
		applied(migrateport.Migration{
			Version: "20261024150000", Name: "driver_qualifications", Source: "cloud",
			SQL: driverQualificationsSQL,
		}, day.Add(-48*time.Hour+15*time.Hour+2*time.Minute), 118*time.Millisecond),
		pending(migrateport.Migration{
			Version: "20261025110000", Name: "edi_partner_index", Source: "core", SQL: ediPartnerIndexSQL,
			Directives: migrateport.Directives{NoTx: true, LockTimeout: 2 * time.Second},
		}, script{statements: []time.Duration{2300 * time.Millisecond, 1700 * time.Millisecond}}),
		pending(migrateport.Migration{
			Version: "20261025120000", Name: "rate_tables", Source: "core", SQL: rateTablesSQL,
		}, script{duration: 640 * time.Millisecond, retries: 2}),
		pending(migrateport.Migration{
			Version: "20261026080000", Name: "backfill_rate_currency", Source: "core", Kind: migrateport.Go,
		}, script{duration: 2600 * time.Millisecond, fails: opts.FailMigration}),
		pending(migrateport.Migration{
			Name: "tenant_rls_helpers", Source: "core", Kind: migrateport.Repeatable, SQL: rlsHelpersSQL,
		}, script{duration: 90 * time.Millisecond}),
	}
	ms[len(ms)-1].state = migrateport.RepeatableDue
	ms[len(ms)-1].appliedAt = day.Add(-48*time.Hour + 9*time.Hour + 16*time.Minute)
	ms[len(ms)-1].appliedChecksum = checksum("previous")
	if opts.NoLockWait {
		ms[4].script.retries = 0
	}
	if opts.Drift {
		ms[2].state = migrateport.Changed
		ms[2].m.Checksum = checksum(driverQualificationsSQL + "-- reviewed\n")
		missing := applied(migrateport.Migration{
			Version: "20261020100000", Name: "legacy_fuel_codes", Source: "core",
		}, day.Add(-96*time.Hour), 12*time.Millisecond)
		missing.state = migrateport.Missing
		ms = append([]*migration{missing}, ms...)
	}
	return ms
}

func demoSeeds(now time.Time, opts Options) []*seed {
	at := now.Add(-26 * time.Hour)
	ref := func(name, desc string, after []string, perTenant bool) seedport.Seed {
		return seedport.Seed{Name: name, Kind: seedport.Reference, Description: desc, After: after, PerTenant: perTenant}
	}
	fix := func(name, desc string, after []string, perTenant bool) seedport.Seed {
		return seedport.Seed{Name: name, Kind: seedport.Fixture, Description: desc, After: after, PerTenant: perTenant}
	}
	upToDate := seedport.Reason{Kind: seedport.UpToDate}
	ss := []*seed{
		{s: ref("us_states", "US states and territories", nil, false), reason: upToDate, applied: true,
			rows: 56, script: seedScript{duration: 80 * time.Millisecond, rows: 56}},
		{s: ref("hazmat_references", "DOT hazardous materials table", nil, false),
			reason:  seedport.Reason{Kind: seedport.InputChanged, Inputs: []string{"data/hazmat.yaml"}},
			applied: true, rows: 3104, script: seedScript{duration: 1400 * time.Millisecond, rows: 3121}},
		{s: ref("ifta_jurisdictions", "IFTA member jurisdictions", []string{"us_states"}, false),
			reason: upToDate, applied: true, rows: 68,
			script: seedScript{duration: 60 * time.Millisecond, rows: 68}},
		{s: ref("organization_roles", "Built-in roles", nil, false),
			reason: seedport.Reason{Kind: seedport.New}, script: seedScript{duration: 310 * time.Millisecond, rows: 24}},
		{s: ref("organization_role_permissions", "Permission sync for every role", []string{"organization_roles"}, true),
			reason:  seedport.Reason{Kind: seedport.CodeChanged},
			applied: true, rows: 211000, tenants: opts.Tenants,
			script: seedScript{duration: 7 * time.Second, rows: 24, failAt: failAt(opts)}},
		{s: ref("document_template_starters", "Starter document templates", []string{"organization_roles"}, false),
			reason:  seedport.Reason{Kind: seedport.VersionChanged},
			applied: true, rows: 12, script: seedScript{duration: 450 * time.Millisecond, rows: 12}},
		{s: ref("system_agent_definitions", "System agent definitions", nil, false), reason: upToDate,
			applied: true, rows: 9, script: seedScript{duration: 40 * time.Millisecond, rows: 9}},
		{s: fix("test_organizations", "Development organizations", []string{"organization_roles"}, false),
			reason: seedport.Reason{Kind: seedport.New}, script: seedScript{duration: 520 * time.Millisecond, rows: 3}},
		{s: fix("workers", "Drivers and dispatchers", []string{"test_organizations"}, false),
			reason: seedport.Reason{Kind: seedport.New}, script: seedScript{duration: 900 * time.Millisecond, rows: 240}},
		{s: fix("shipments", "Shipments with stops and moves",
			[]string{"workers", "ifta_jurisdictions"}, false),
			reason: seedport.Reason{Kind: seedport.New}, script: seedScript{duration: 2200 * time.Millisecond, rows: 12000}},
	}
	for _, s := range ss {
		if s.applied {
			s.appliedAt = at
			s.duration = s.script.duration
			if s.s.PerTenant {
				s.tenantsDone = s.tenants
			}
		}
	}
	return ss
}

func failAt(opts Options) int {
	if !opts.FailSeed {
		return 0
	}
	return opts.Tenants*47/100 + 1
}

func demoOrphans(now time.Time) []seedport.Status {
	return []seedport.Status{{
		Seed:      seedport.Seed{Name: "legacy_rate_codes", Kind: seedport.Reference},
		State:     seedport.Orphaned,
		Reason:    seedport.Reason{Kind: seedport.Excluded},
		AppliedAt: now.Add(-21 * 24 * time.Hour),
		Duration:  210 * time.Millisecond,
		Rows:      412,
	}}
}

func demoTemplates(now time.Time) []migrateport.Template {
	return []migrateport.Template{
		{Name: "keel_tpl_9f2c41d0", Fingerprint: "9f2c41d0", Current: true, Size: 48 << 20, CreatedAt: now.Add(-2 * time.Hour)},
		{Name: "keel_tpl_51ab07e3", Fingerprint: "51ab07e3", Size: 46 << 20, CreatedAt: now.Add(-3 * 24 * time.Hour)},
		{Name: "keel_tpl_c7d2e9aa", Fingerprint: "c7d2e9aa", Size: 45 << 20, CreatedAt: now.Add(-19 * 24 * time.Hour)},
	}
}
