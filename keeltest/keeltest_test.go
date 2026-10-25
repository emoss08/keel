package keeltest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emoss08/keel/keeltest"
)

func TestSnapshotWritesThenCompares(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sql := `SELECT s."id" FROM "shipments" s WHERE s."organization_id" = $1 AND s."created_at" > $2`
	args := []any{"org_1", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}

	t.Setenv(keeltest.UpdateEnv, "1")
	keeltest.Snapshot(t, sql, args)
	got, err := os.ReadFile(filepath.Join("testdata", "keel", "TestSnapshotWritesThenCompares.sql"))
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT s.id\nFROM shipments s\nWHERE s.organization_id = $1 AND s.created_at > $2\n" +
		"-- $1 = 'org_1'\n-- $2 = '2026-01-02 03:04:05Z'\n"
	if string(got) != want {
		t.Fatalf("snapshot:\n%s\nwant:\n%s", got, want)
	}

	t.Setenv(keeltest.UpdateEnv, "")
	keeltest.Snapshot(t, sql, args)
	keeltest.SnapshotNamed(t, "TestSnapshotWritesThenCompares", sql, args)
}
