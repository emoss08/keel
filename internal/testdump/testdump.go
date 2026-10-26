// Package testdump gives the squash tests the pg_dump command of the test database's server.
package testdump

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Command returns the pg_dump command, with {db} for the database: KEEL_TEST_PG_DUMP split on
// spaces when it is set (CI points it at its service container), else pg_dump in the keelpg
// container. It probes the command with --version first and skips the test when it cannot run,
// or fails it when KEEL_TEST_REQUIRE is set.
func Command(t testing.TB) []string {
	t.Helper()
	cmd := strings.Fields(os.Getenv("KEEL_TEST_PG_DUMP"))
	if len(cmd) == 0 {
		cmd = []string{"docker", "exec", "keelpg", "pg_dump", "-U", "keel", "-d", "{db}"}
	}
	i := slices.IndexFunc(cmd, func(arg string) bool { return filepath.Base(arg) == "pg_dump" })
	if i < 0 {
		t.Fatalf("KEEL_TEST_PG_DUMP %q runs no pg_dump", strings.Join(cmd, " "))
	}
	probe := append(slices.Clone(cmd[:i+1]), "--version")
	if err := exec.Command(probe[0], probe[1:]...).Run(); err != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatalf("%s: %v", strings.Join(probe, " "), err)
		}
		t.Skipf("pg_dump unavailable (%s): %v", strings.Join(probe, " "), err)
	}
	return cmd
}
