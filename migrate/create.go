package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CreateOptions configure Create.
type CreateOptions struct {
	// NoTx starts the file with --keel:notx.
	NoTx bool
	// Repeatable creates R_<name>.sql instead of a versioned file.
	Repeatable bool
	// Now is the time the version is taken from; default the current time.
	Now time.Time
}

// Create writes an empty migration named name in dir and returns its path. The version is the
// UTC time, moved forward a second at a time past any version already in dir.
func Create(dir, name string, o CreateOptions) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.NewReplacer(" ", "_", "-", "_").Replace(name)
	if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
		return "", fmt.Errorf("migrate: create: %q must hold only letters, digits and underscores", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("migrate: create: %w", err)
	}
	var body string
	if o.NoTx {
		body = "--keel:notx\n"
	}
	if o.Repeatable {
		return write(filepath.Join(dir, "R_"+name+".sql"), body)
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	used, err := versions(dir)
	if err != nil {
		return "", err
	}
	t := now.UTC().Truncate(time.Second)
	for used[t.Format(versionLayout)] {
		t = t.Add(time.Second)
	}
	return write(filepath.Join(dir, t.Format(versionLayout)+"_"+name+".sql"), body)
}

// versions returns the versions of the migration files in dir.
func versions(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: create: %w", err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if v, _, ok := strings.Cut(e.Name(), "_"); ok {
			out[v] = true
		}
	}
	return out, nil
}

// write creates path with body, failing when it exists.
func write(path, body string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("migrate: create: %s exists", path)
	}
	if err != nil {
		return "", fmt.Errorf("migrate: create: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("migrate: create: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("migrate: create: %w", err)
	}
	return path, nil
}
