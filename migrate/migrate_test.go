package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
)

// basic is a source of a table, a notx index migration and a column.
func basic() fstest.MapFS {
	return files(
		"20260101000000_items.sql", "CREATE TABLE items (id int PRIMARY KEY, name text);",
		"20260102000000_index.sql", "--keel:notx\nCREATE INDEX CONCURRENTLY idx_items_name ON items (name);\n--keel:split\nCREATE INDEX CONCURRENTLY idx_items_id_name ON items (id, name);\n",
		"20260103000000_column.sql", "ALTER TABLE items ADD COLUMN price numeric;",
	)
}

func TestUpStatusHistory(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	m := open(t, dsn, one(basic()), Options{})

	res, err := m.Up(ctx, UpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(migrationIDs(res.Applied)) != "[20260101000000_items 20260102000000_index 20260103000000_column]" {
		t.Fatalf("applied %v", migrationIDs(res.Applied))
	}
	again, err := m.Up(ctx, UpOptions{})
	if err != nil || len(again.Applied) != 0 || !again.Plan.Empty() {
		t.Fatalf("second up: %+v %v", again, err)
	}
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if s.State != StateApplied || s.AppliedAt.IsZero() || s.AppliedChecksum != s.Checksum {
			t.Fatalf("status %+v", s)
		}
	}
	c := connect(t, dsn)
	if n := scalar[int](t, c, "SELECT count(*) FROM pg_indexes WHERE tablename = 'items'"); n != 3 {
		t.Fatalf("%d indexes", n)
	}
	hist, err := m.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || hist[1].Source != "core" || hist[1].Kind != KindVersioned || hist[0].ID() != "20260101000000_items" {
		t.Fatalf("history %+v", hist)
	}
}

func TestStatusWithoutTable(t *testing.T) {
	ctx := context.Background()
	m := open(t, database(t), one(basic()), Options{})
	st, err := m.Status(ctx)
	if err != nil || len(st) != 3 || st[0].State != StatePending {
		t.Fatalf("%+v %v", st, err)
	}
	if ok, _ := m.tableExists(ctx); ok {
		t.Fatal("Status created the table")
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	m := open(t, dsn, one(files(
		"20260101000000_ok.sql", "CREATE TABLE a (id int);",
		"20260102000000_bad.sql", "CREATE TABLE b (id int); SELECT 1/0;",
	)), Options{})
	res, err := m.Up(ctx, UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "20260102000000_bad") {
		t.Fatalf("error %v", err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("applied %v", res.Applied)
	}
	if scalar[bool](t, connect(t, dsn), "SELECT to_regclass('b') IS NOT NULL") {
		t.Fatal("a failed migration left its table behind")
	}
}

func TestConcurrentUpAppliesOnce(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	var wg sync.WaitGroup
	counts := make([]int, 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Go(func() {
			m, err := Open(ctx, dsn, one(basic()), Options{})
			if err != nil {
				errs[i] = err
				return
			}
			defer m.Close()
			res, err := m.Up(ctx, UpOptions{})
			counts[i], errs[i] = len(res.Applied), err
		})
	}
	wg.Wait()
	total := 0
	for i := range 4 {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		total += counts[i]
	}
	if total != 3 {
		t.Fatalf("applied %d migrations across runners, want 3", total)
	}
}

func TestTimeoutDirectives(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	m := open(t, dsn, one(files(
		"20260101000000_settings.sql", "CREATE TABLE settings (name text, value text);\nINSERT INTO settings VALUES ('default', current_setting('lock_timeout'));",
		"20260102000000_custom.sql", "--keel:lock_timeout 1500ms\n--keel:statement_timeout 30s\nINSERT INTO settings VALUES ('custom', current_setting('lock_timeout') || ' ' || current_setting('statement_timeout'));",
		"20260103000000_off.sql", "--keel:lock_timeout 0\nINSERT INTO settings VALUES ('off', current_setting('lock_timeout'));",
		"20260104000000_notx.sql", "--keel:notx\n--keel:lock_timeout 3s\nINSERT INTO settings VALUES ('notx', current_setting('lock_timeout'));",
	)), Options{})
	if _, err := m.Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	c := connect(t, dsn)
	rows, err := c.Query(ctx, "SELECT name || '=' || value FROM settings ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[custom=1500ms 30s default=5s notx=3s off=0]" {
		t.Fatalf("settings %v", got)
	}
	var lt string
	if err := m.Conn().QueryRow(ctx, "SHOW lock_timeout").Scan(&lt); err != nil || lt != "0" {
		t.Fatalf("session lock_timeout after notx = %q %v", lt, err)
	}
	bad := open(t, database(t), one(files("20260101000000_slow.sql", "--keel:statement_timeout 50ms\nSELECT pg_sleep(1);")), Options{})
	if _, err := bad.Up(ctx, UpOptions{}); err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("statement_timeout: %v", err)
	}
}

func TestRepeatables(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	src := files(
		"20260101000000_items.sql", "CREATE TABLE items (id int, name text); CREATE TABLE runs (name text);",
		"R_b_view.sql", "CREATE OR REPLACE VIEW item_names AS SELECT name FROM items; INSERT INTO runs VALUES ('b');",
		"R_a_func.sql", "CREATE OR REPLACE FUNCTION item_count() RETURNS bigint LANGUAGE sql AS $$ SELECT count(*) FROM items $$; INSERT INTO runs VALUES ('a');",
	)
	m := open(t, dsn, one(src), Options{})
	res, err := m.Up(ctx, UpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(migrationIDs(res.Applied)) != "[20260101000000_items R_a_func R_b_view]" {
		t.Fatalf("applied %v", migrationIDs(res.Applied))
	}
	res, _ = m.Up(ctx, UpOptions{})
	if len(res.Applied) != 0 {
		t.Fatalf("unchanged repeatables re-ran: %v", migrationIDs(res.Applied))
	}
	src["R_b_view.sql"] = &fstest.MapFile{Data: []byte("CREATE OR REPLACE VIEW item_names AS SELECT name, id FROM items; INSERT INTO runs VALUES ('b');")}
	m2 := open(t, dsn, one(src), Options{})
	st, _ := m2.Status(ctx)
	if st[2].State != StateRepeatableDue || st[1].State != StateApplied || st[2].AppliedAt.IsZero() {
		t.Fatalf("status %+v", st)
	}
	res, err = m2.Up(ctx, UpOptions{})
	if err != nil || fmt.Sprint(migrationIDs(res.Applied)) != "[R_b_view]" {
		t.Fatalf("applied %v %v", migrationIDs(res.Applied), err)
	}
	c := connect(t, dsn)
	if got := scalar[string](t, c, "SELECT string_agg(name, '' ORDER BY ctid) FROM runs"); got != "abb" {
		t.Fatalf("runs %s", got)
	}
	if n := scalar[int](t, c, "SELECT count(*) FROM keel_migrations WHERE version = '' AND kind = 'repeatable'"); n != 2 {
		t.Fatalf("%d repeatable rows", n)
	}
}

func TestGoMigrations(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	var inTx, outTx bool
	set := one(files(
		"20260101000000_items.sql", "CREATE TABLE items (id int, name text);",
		"20260103000000_upper.sql", "UPDATE items SET name = upper(name);",
	),
		Func{Version: "20260102000000", Name: "seed", Up: func(ctx context.Context, tx pgx.Tx) error {
			inTx = true
			_, err := tx.Exec(ctx, "INSERT INTO items VALUES (1, 'a'), (2, 'b')")
			return err
		}},
		Func{Version: "20260104000000", Name: "index", UpNoTx: func(ctx context.Context, conn *pgx.Conn) error {
			var lt string
			if err := conn.QueryRow(ctx, "SHOW lock_timeout").Scan(&lt); err != nil {
				return err
			}
			outTx = lt == "5s"
			_, err := conn.Exec(ctx, "CREATE INDEX CONCURRENTLY items_name ON items (name)")
			return err
		}},
	)
	rec := newRecorder()
	m := open(t, dsn, set, Options{Observer: rec.observe})
	res, err := m.Up(ctx, UpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(migrationIDs(res.Applied)) != "[20260101000000_items 20260102000000_seed 20260103000000_upper 20260104000000_index]" {
		t.Fatalf("applied %v", migrationIDs(res.Applied))
	}
	if !inTx || !outTx {
		t.Fatalf("ran in tx %v, notx with lock timeout %v", inTx, outTx)
	}
	c := connect(t, dsn)
	if got := scalar[string](t, c, "SELECT string_agg(name, ',' ORDER BY id) FROM items"); got != "A,B" {
		t.Fatalf("items %s", got)
	}
	if k := scalar[string](t, c, "SELECT kind FROM keel_migrations WHERE version = '20260102000000'"); k != "go" {
		t.Fatalf("kind %s", k)
	}
	failing := one(nil, Func{Version: "20260101000000", Name: "fail", Up: func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "CREATE TABLE never (id int)"); err != nil {
			return err
		}
		return errors.New("boom")
	}})
	dsn2 := database(t)
	if _, err := open(t, dsn2, failing, Options{}).Up(ctx, UpOptions{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error %v", err)
	}
	if scalar[bool](t, connect(t, dsn2), "SELECT to_regclass('never') IS NOT NULL") {
		t.Fatal("failed Go migration committed")
	}
}

func TestAfter(t *testing.T) {
	ctx := context.Background()
	after := []string{"CREATE TABLE IF NOT EXISTS after_runs (n serial, at_txid bigint)", "INSERT INTO after_runs (at_txid) VALUES (txid_current())"}
	t.Run("in the last transaction", func(t *testing.T) {
		dsn := database(t)
		m := open(t, dsn, one(files(
			"20260101000000_a.sql", "CREATE TABLE a (id int);",
			"20260102000000_b.sql", "CREATE TABLE b (id int, txid bigint DEFAULT txid_current()); INSERT INTO b (id) VALUES (1);",
		)), Options{After: after})
		res, err := m.Up(ctx, UpOptions{})
		if err != nil || len(res.Plan.After) != 2 {
			t.Fatalf("%+v %v", res.Plan, err)
		}
		c := connect(t, dsn)
		if !scalar[bool](t, c, "SELECT (SELECT txid FROM b) = (SELECT at_txid FROM after_runs)") {
			t.Fatal("After did not run in the last migration's transaction")
		}
		res, err = m.Up(ctx, UpOptions{})
		if err != nil || res.Plan.After != nil {
			t.Fatalf("After planned for an empty run: %+v %v", res.Plan, err)
		}
		if n := scalar[int](t, c, "SELECT count(*) FROM after_runs"); n != 1 {
			t.Fatalf("After ran %d times", n)
		}
	})
	t.Run("after a notx migration", func(t *testing.T) {
		dsn := database(t)
		rec := newRecorder()
		m := open(t, dsn, one(files(
			"20260101000000_a.sql", "CREATE TABLE a (id int);",
			"20260102000000_b.sql", "--keel:notx\nCREATE INDEX CONCURRENTLY a_id ON a (id);",
		)), Options{After: after, Observer: rec.observe})
		if _, err := m.Up(ctx, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		k := rec.kinds()
		if fmt.Sprint(k[len(k)-4:]) != fmt.Sprint([]EventKind{MigrationEnd, After, After, RunEnd}) {
			t.Fatalf("events %v", k)
		}
		if n := scalar[int](t, connect(t, dsn), "SELECT count(*) FROM after_runs"); n != 1 {
			t.Fatalf("After ran %d times", n)
		}
	})
	t.Run("after a failure", func(t *testing.T) {
		dsn := database(t)
		m := open(t, dsn, one(files(
			"20260101000000_a.sql", "CREATE TABLE a (id int);",
			"20260102000000_b.sql", "SELECT 1/0;",
		)), Options{After: after})
		if _, err := m.Up(ctx, UpOptions{}); err == nil {
			t.Fatal("no error")
		}
		if n := scalar[int](t, connect(t, dsn), "SELECT count(*) FROM after_runs"); n != 1 {
			t.Fatalf("After ran %d times after a partial run", n)
		}
	})
}

func TestOutOfOrder(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	if _, err := open(t, dsn, one(files("20260102000000_b.sql", "CREATE TABLE b (id int);")), Options{}).Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	late := one(files("20260101000000_a.sql", "CREATE TABLE a (id int);", "20260102000000_b.sql", "CREATE TABLE b (id int);"))
	m := open(t, dsn, late, Options{})
	if _, err := m.Up(ctx, UpOptions{}); !errors.Is(err, ErrOutOfOrder) || !strings.Contains(err.Error(), "20260101000000_a") {
		t.Fatalf("got %v, want ErrOutOfOrder", err)
	}
	if _, err := m.Plan(ctx, UpOptions{}); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("plan: %v", err)
	}
	st, _ := m.Status(ctx)
	if !st[0].OutOfOrder || st[0].State != StatePending {
		t.Fatalf("status %+v", st[0])
	}
	rep, err := m.Check(ctx)
	if err != nil || rep.OK() || len(rep.OutOfOrder) != 1 {
		t.Fatalf("check %+v %v", rep, err)
	}
	res, err := open(t, dsn, late, Options{AllowOutOfOrder: true}).Up(ctx, UpOptions{})
	if err != nil || len(res.Applied) != 1 {
		t.Fatalf("%v %v", res.Applied, err)
	}
}

func TestChangedAndMissing(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	src := basic()
	if _, err := open(t, dsn, one(src), Options{}).Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	src["20260101000000_items.sql"] = &fstest.MapFile{Data: []byte("-- edited\nCREATE TABLE items (id int PRIMARY KEY, name text);")}
	delete(src, "20260103000000_column.sql")
	m := open(t, dsn, one(src), Options{})
	if _, err := m.Up(ctx, UpOptions{}); !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), "20260101000000_items") {
		t.Fatalf("got %v, want ErrChanged", err)
	}
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	states := []string{}
	for _, s := range st {
		states = append(states, s.ID()+"="+s.State.String())
	}
	if fmt.Sprint(states) != "[20260101000000_items=changed 20260102000000_index=applied 20260103000000_column=missing]" {
		t.Fatalf("states %v", states)
	}
	if st[2].AppliedAt.IsZero() || st[2].Source != "core" {
		t.Fatalf("missing %+v", st[2])
	}
	rep, _ := m.Check(ctx)
	if rep.OK() || len(rep.Changed) != 1 || len(rep.Missing) != 1 {
		t.Fatalf("check %+v", rep)
	}
	rep2, err := m.Repair(ctx, "20260101000000")
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Old == rep2.New || rep2.New != checksum(string(src["20260101000000_items.sql"].Data)) {
		t.Fatalf("repair %+v", rep2)
	}
	if again, _ := m.Repair(ctx, "20260101000000"); again.Old != again.New {
		t.Fatalf("second repair %+v", again)
	}
	if _, err := m.Up(ctx, UpOptions{}); err != nil {
		t.Fatalf("after repair: %v", err)
	}
	if _, err := m.Repair(ctx, "20991231000000"); err == nil {
		t.Fatal("repair of an unknown version")
	}
}

func TestPlanDryRunTo(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	src := basic()
	src["R_view.sql"] = &fstest.MapFile{Data: []byte("CREATE OR REPLACE VIEW v AS SELECT 1 AS one;")}
	m := open(t, dsn, one(src), Options{After: []string{"SELECT 1"}})
	p, err := m.Plan(ctx, UpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Pending) != 3 || len(p.Repeatables) != 1 || len(p.After) != 1 || p.Total() != 4 {
		t.Fatalf("plan %+v", p)
	}
	res, err := m.Up(ctx, UpOptions{DryRun: true})
	if err != nil || len(res.Applied) != 0 || res.Plan.Total() != 4 {
		t.Fatalf("dry run %+v %v", res, err)
	}
	if ok, _ := m.tableExists(ctx); ok {
		t.Fatal("dry run created the table")
	}
	res, err = m.Up(ctx, UpOptions{To: "20260102000000"})
	if err != nil || fmt.Sprint(migrationIDs(res.Applied)) != "[20260101000000_items 20260102000000_index]" {
		t.Fatalf("to: %v %v", migrationIDs(res.Applied), err)
	}
	if _, err := m.Up(ctx, UpOptions{To: "20200101000000"}); err == nil {
		t.Fatal("unknown To accepted")
	}
	res, err = m.Up(ctx, UpOptions{})
	if err != nil || fmt.Sprint(migrationIDs(res.Applied)) != "[20260103000000_column R_view]" {
		t.Fatalf("rest: %v %v", migrationIDs(res.Applied), err)
	}
}

func TestEventSequence(t *testing.T) {
	ctx := context.Background()
	rec := newRecorder()
	src := basic()
	src["R_view.sql"] = &fstest.MapFile{Data: []byte("CREATE OR REPLACE VIEW v AS SELECT 1 AS one;")}
	m := open(t, database(t), one(src), Options{Observer: rec.observe, After: []string{"SELECT 1"}})
	if _, err := m.Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []EventKind{LockAcquired, MigrationStart, MigrationEnd, MigrationStart, Statement, Statement,
		MigrationEnd, MigrationStart, MigrationEnd, MigrationStart, After, MigrationEnd, RunEnd}
	if fmt.Sprint(rec.kinds()) != fmt.Sprint(want) {
		t.Fatalf("events %v", rec.kinds())
	}
	ev := rec.all()
	if ev[1].Index != 0 || ev[1].Total != 4 || ev[1].Migration.ID() != "20260101000000_items" {
		t.Fatalf("start %+v", ev[1])
	}
	if ev[5].Statement != 2 || ev[5].Statements != 2 || ev[5].Index != 1 || !strings.Contains(ev[5].SQL, "idx_items_id_name") {
		t.Fatalf("statement %+v", ev[5])
	}
	if ev[9].Migration.Kind != KindRepeatable || ev[9].Index != 3 || ev[10].SQL != "SELECT 1" || ev[10].Migration != nil {
		t.Fatalf("repeatable %+v after %+v", ev[9], ev[10])
	}
	if end := ev[len(ev)-1]; end.Duration <= 0 || end.Err != nil || end.Total != 4 || end.Migration != nil {
		t.Fatalf("run end %+v", end)
	}
	if ev[2].Duration <= 0 || ev[2].Err != nil {
		t.Fatalf("migration end %+v", ev[2])
	}
	for _, k := range []EventKind{LockWait, LockAcquired, MigrationStart, Statement, Retry, MigrationEnd, BaselineMarked, After, RunEnd} {
		if strings.HasPrefix(k.String(), "EventKind") {
			t.Errorf("kind %d has no name", k)
		}
	}
}

func TestLockWaitEvents(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	holderCfg, _ := pgx.ParseConfig(dsn)
	holderCfg.RuntimeParams["application_name"] = "keel-test-holder"
	holder, err := pgx.ConnectConfig(ctx, holderCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	pid := int32(holder.PgConn().PID())
	rec := newRecorder()
	m := open(t, dsn, one(basic()), Options{Observer: rec.observe})
	done := make(chan error, 1)
	go func() {
		_, err := m.Up(ctx, UpOptions{})
		done <- err
	}()
	waits := 0
	deadline := time.After(10 * time.Second)
	for waits < 2 {
		select {
		case e := <-rec.notify:
			if e.Kind != LockWait {
				continue
			}
			waits++
			if len(e.Blockers) != 1 || e.Blockers[0].PID != pid || e.Blockers[0].Application != "keel-test-holder" || e.Blockers[0].Mode != "ExclusiveLock" {
				t.Fatalf("blockers %+v", e.Blockers)
			}
			if waits == 2 && e.Duration < 900*time.Millisecond {
				t.Fatalf("second LockWait after %v", e.Duration)
			}
		case <-deadline:
			t.Fatal("no LockWait events")
		}
	}
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, e := range rec.all() {
		if e.Kind == LockAcquired {
			if e.Duration < time.Second {
				t.Fatalf("lock acquired after %v", e.Duration)
			}
			return
		}
	}
	t.Fatal("no LockAcquired")
}

// lockedTable applies a table and returns a connection holding ACCESS EXCLUSIVE on it in an open
// transaction.
func lockedTable(t *testing.T, dsn string) (*pgx.Conn, pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	if _, err := open(t, dsn, one(files("20260101000000_items.sql", "CREATE TABLE items (id int);")), Options{}).Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(dsn)
	cfg.RuntimeParams["application_name"] = "keel-test-report"
	holder, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE items IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	return holder, tx
}

// lockedSet adds a column to the table lockedTable locks, with extra directives.
func lockedSet(extra string) Set {
	return one(files(
		"20260101000000_items.sql", "CREATE TABLE items (id int);",
		"20260102000000_column.sql", extra+"ALTER TABLE items ADD COLUMN name text;",
	))
}

func TestLockTimeoutRetry(t *testing.T) {
	ctx := context.Background()
	for _, notx := range []bool{false, true} {
		t.Run(fmt.Sprintf("notx=%v", notx), func(t *testing.T) {
			dsn := database(t)
			holder, tx := lockedTable(t, dsn)
			pid := int32(holder.PgConn().PID())
			released := make(chan struct{})
			var once sync.Once
			rec := newRecorder()
			observe := func(e Event) {
				rec.observe(e)
				if e.Kind == Retry && e.Attempt == 3 {
					once.Do(func() { close(released) })
				}
			}
			committed := make(chan struct{})
			go func() {
				defer close(committed)
				<-released
				_ = tx.Commit(context.Background())
			}()
			defer func() {
				once.Do(func() { close(released) })
				<-committed
			}()
			extra := ""
			if notx {
				extra = "--keel:notx\n"
			}
			m := open(t, dsn, lockedSet(extra), Options{LockTimeout: 200 * time.Millisecond, RetryBackoff: 20 * time.Millisecond, Observer: observe})
			res, err := m.Up(ctx, UpOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Applied) != 1 {
				t.Fatalf("applied %v", migrationIDs(res.Applied))
			}
			var retries []Event
			for _, e := range rec.all() {
				if e.Kind == Retry {
					retries = append(retries, e)
				}
			}
			if len(retries) < 2 || retries[0].Attempt != 2 || retries[0].Migration.ID() != "20260102000000_column" {
				t.Fatalf("retries %+v", retries)
			}
			b := retries[0].Blockers
			if len(b) != 1 || b[0].PID != pid || b[0].Application != "keel-test-report" || b[0].Mode != "AccessExclusiveLock" ||
				b[0].State != "idle in transaction" || !strings.Contains(b[0].Query, "LOCK TABLE") || b[0].Age <= 0 {
				t.Fatalf("blockers %+v", b)
			}
			if notx && (retries[0].Statement != 1 || retries[0].Statements != 1) {
				t.Fatalf("notx retry %+v", retries[0])
			}
			if !isLockTimeout(retries[0].Err) {
				t.Fatalf("retry error %v", retries[0].Err)
			}
		})
	}
}

func TestLockTimeoutExhausted(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	lockedTable(t, dsn)
	rec := newRecorder()
	m := open(t, dsn, lockedSet(""), Options{LockTimeout: 100 * time.Millisecond, Retries: 2, RetryBackoff: 10 * time.Millisecond, Observer: rec.observe})
	_, err := m.Up(ctx, UpOptions{})
	if !isLockTimeout(err) || !strings.Contains(err.Error(), "after 3 attempts") || !strings.Contains(err.Error(), "keel-test-report") {
		t.Fatalf("error %v", err)
	}
	retries := 0
	for _, e := range rec.all() {
		if e.Kind == Retry {
			retries++
		}
	}
	if retries != 2 {
		t.Fatalf("%d retries", retries)
	}

	rec2 := newRecorder()
	m2 := open(t, dsn, lockedSet("--keel:no_retry\n--keel:lock_timeout 50ms\n"), Options{Observer: rec2.observe})
	if _, err := m2.Up(ctx, UpOptions{}); !isLockTimeout(err) {
		t.Fatalf("no_retry error %v", err)
	}
	for _, e := range rec2.all() {
		if e.Kind == Retry {
			t.Fatal("no_retry retried")
		}
	}
}

func TestBaseline(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	c := connect(t, dsn)
	if _, err := c.Exec(ctx, "CREATE TABLE items (id int PRIMARY KEY, name text)"); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	m := open(t, dsn, one(basic()), Options{Observer: rec.observe})
	marked, err := m.Baseline(ctx, "20260101000000")
	if err != nil || fmt.Sprint(migrationIDs(marked)) != "[20260101000000_items]" {
		t.Fatalf("%v %v", migrationIDs(marked), err)
	}
	if k := rec.kinds(); fmt.Sprint(k) != fmt.Sprint([]EventKind{LockAcquired, BaselineMarked}) {
		t.Fatalf("events %v", k)
	}
	res, err := m.Up(ctx, UpOptions{})
	if err != nil || len(res.Applied) != 2 {
		t.Fatalf("%v %v", migrationIDs(res.Applied), err)
	}
	if again, err := m.Baseline(ctx, "20260103000000"); err != nil || len(again) != 0 {
		t.Fatalf("%v %v", again, err)
	}
	if _, err := m.Baseline(ctx, "20200101000000"); err == nil {
		t.Fatal("unknown version accepted")
	}
}

func TestBaselineFile(t *testing.T) {
	ctx := context.Background()
	original := files(
		"20260101000000_a.sql", "CREATE TABLE a (id int);",
		"20260102000000_b.sql", "CREATE TABLE b (id int);",
	)
	squashed := files(
		"20260102000000_baseline.sql", "--keel:baseline\nCREATE TABLE a (id int);\nCREATE TABLE b (id int);",
		"20260103000000_c.sql", "CREATE TABLE c (id int);",
	)

	fresh := database(t)
	res, err := open(t, fresh, one(squashed), Options{}).Up(ctx, UpOptions{})
	if err != nil || fmt.Sprint(migrationIDs(res.Applied)) != "[20260102000000_baseline 20260103000000_c]" || len(res.Marked) != 0 {
		t.Fatalf("fresh: %v %v", res, err)
	}
	if err := Require(ctx, connect(t, fresh), "20260101000000"); err != nil {
		t.Fatalf("require a squashed version: %v", err)
	}

	old := database(t)
	if _, err := open(t, old, one(original), Options{}).Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	m := open(t, old, one(squashed), Options{Observer: rec.observe})
	p, err := m.Plan(ctx, UpOptions{})
	if err != nil || len(p.Marks) != 1 || len(p.Pending) != 1 {
		t.Fatalf("plan %+v %v", p, err)
	}
	res, err = m.Up(ctx, UpOptions{})
	if err != nil || fmt.Sprint(migrationIDs(res.Marked)) != "[20260102000000_baseline]" || fmt.Sprint(migrationIDs(res.Applied)) != "[20260103000000_c]" {
		t.Fatalf("old: %+v %v", res, err)
	}
	if k := rec.kinds(); k[1] != BaselineMarked || rec.all()[1].Total != 2 {
		t.Fatalf("events %v", k)
	}
	st, _ := m.Status(ctx)
	for _, s := range st {
		if s.State != StateApplied {
			t.Fatalf("status %+v", st)
		}
	}
	if len(st) != 2 {
		t.Fatalf("status %+v", st)
	}

	behind := database(t)
	if _, err := open(t, behind, one(files("20260101000000_a.sql", "CREATE TABLE a (id int);")), Options{}).Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	b := open(t, behind, one(files("20260102000000_baseline.sql", "--keel:baseline\nCREATE TABLE a (id int);\nCREATE TABLE b (id int);")), Options{})
	if _, err := b.Up(ctx, UpOptions{}); !errors.Is(err, ErrBaselineGap) {
		t.Fatalf("got %v, want ErrBaselineGap", err)
	}
}

func TestSchemaOption(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	m := open(t, dsn, one(basic()), Options{Schema: "ops"})
	if _, err := m.Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	c := connect(t, dsn)
	if n := scalar[int](t, c, "SELECT count(*) FROM ops.keel_migrations"); n != 3 {
		t.Fatalf("%d rows", n)
	}
	if scalar[bool](t, c, "SELECT to_regclass('public.keel_migrations') IS NOT NULL") {
		t.Fatal("public.keel_migrations created")
	}
}

func TestCheckAgainst(t *testing.T) {
	base := one(files("20260101000000_a.sql", "x", "20260103000000_c.sql", "z", "20260104000000_d.sql", "w"))
	head := one(files("20260101000000_a.sql", "x2", "20260102000000_b.sql", "y", "20260103000000_c.sql", "z", "20260105000000_e.sql", "v"))
	rep, err := CheckAgainst(head, base)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || ids(rep.OutOfOrder) != "20260102000000_b (core)" || ids(rep.Changed) != "20260101000000_a (core)" ||
		ids(rep.Missing) != "20260104000000_d (core)" || ids(rep.Pending) != "20260105000000_e (core)" {
		t.Fatalf("report %+v", rep)
	}
	if rep, _ := CheckAgainst(base, base); !rep.OK() {
		t.Fatalf("self check %+v", rep)
	}
}

func TestOptionsDefaults(t *testing.T) {
	var o Options
	if o.lockTimeout(Directives{}) != DefaultLockTimeout || o.retries(Directives{}) != DefaultRetries {
		t.Fatal("defaults")
	}
	if (Options{LockTimeout: -1}).lockTimeout(Directives{}) != 0 || o.lockTimeout(Directives{LockTimeout: -1}) != 0 {
		t.Fatal("disabled lock timeout")
	}
	if (Options{Retries: -1}).retries(Directives{}) != 0 || o.retries(Directives{NoRetry: true}) != 0 {
		t.Fatal("disabled retries")
	}
	if b := (Options{RetryBackoff: time.Second}).backoff(10); b < maxBackoff || b > maxBackoff*5/4 {
		t.Fatalf("backoff %v", b)
	}
	if millis(1500*time.Microsecond) != "1ms" || millis(0) != "0" {
		t.Fatal("millis")
	}
}

func TestAfterFuncRunsOnceWithTheAppliedMigrations(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	var calls int
	var applied []string
	m := open(t, dsn, one(basic()), Options{AfterFunc: func(_ context.Context, ms []Migration) error {
		calls++
		applied = migrationIDs(ms)
		return nil
	}})

	if _, err := m.Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || fmt.Sprint(applied) != "[20260101000000_items 20260102000000_index 20260103000000_column]" {
		t.Fatalf("calls %d, applied %v", calls, applied)
	}
	if _, err := m.Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("a run that applied nothing called AfterFunc: %d calls", calls)
	}
}

func TestAfterFuncErrorJoinsTheRun(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	boom := errors.New("boom")
	m := open(t, dsn, one(basic()), Options{AfterFunc: func(context.Context, []Migration) error { return boom }})

	res, err := m.Up(ctx, UpOptions{})
	if !errors.Is(err, boom) || len(res.Applied) != 3 {
		t.Fatalf("err %v, applied %d", err, len(res.Applied))
	}
}
