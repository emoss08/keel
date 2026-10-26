package seed

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/goccy/go-yaml"
)

// YAML decodes the seed's input file at path into a T. Mapping keys match `yaml` tags, or field
// names lowercased; a key that appears twice is an error.
func YAML[T any](r *Run, path string) (T, error) {
	var v T
	data, err := r.ReadFile(path)
	if err != nil {
		return v, err
	}
	if err := yaml.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("seed: %s: %s: %w", r.Seed.Name, path, err)
	}
	return v, nil
}

// JSON decodes the seed's input file at path into a T with the database's JSON codec
// (keel.Config.JSON).
func JSON[T any](r *Run, path string) (T, error) {
	var v T
	data, err := r.ReadFile(path)
	if err != nil {
		return v, err
	}
	if err := r.DB.JSON().Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("seed: %s: %s: %w", r.Seed.Name, path, err)
	}
	return v, nil
}

// CSV decodes every record of the seed's input file at path into a T, a struct; see CSVEach for how
// columns map to fields. Use CSVEach for files too large to hold at once.
func CSV[T any](r *Run, path string) ([]T, error) {
	var out []T
	err := CSVEach(r, path, func(v T) error {
		out = append(out, v)
		return nil
	})
	return out, err
}

// CSVEach streams the records of the seed's input file at path, decoding each into a T and calling
// fn with it, one record in memory at a time. An error from fn stops the stream and is returned.
//
// The first record is the header. Each column maps to the exported field of T tagged `csv:"name"`,
// or whose name in snake case is the column's (ProNumber reads pro_number); fields of embedded
// structs count, and `csv:"-"` skips a field. A column with no field is an error, so a misspelled
// header cannot drop data; a field with no column keeps its zero value. Fields may be strings,
// booleans, integers, floats, time.Duration ("90s"), time.Time (RFC 3339 or 2006-01-02),
// encoding.TextUnmarshaler implementations, or pointers to any of them. An empty cell leaves the
// field zero, and a pointer nil.
//
// To load a large file, batch the records into Copy:
//
//	batch := make([]*model.Hazmat, 0, 5000)
//	flush := func() error {
//	    n, err := modeldb.Hazmats.Copy(ctx, r.DB, batch)
//	    r.Count(n)
//	    batch = batch[:0]
//	    return err
//	}
//	err := seed.CSVEach(r, "data/hazmat.csv", func(h model.Hazmat) error {
//	    batch = append(batch, &h)
//	    if len(batch) == cap(batch) {
//	        return flush()
//	    }
//	    return nil
//	})
//	if err == nil && len(batch) > 0 {
//	    err = flush()
//	}
func CSVEach[T any](r *Run, path string, fn func(T) error) error {
	f, err := r.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cr := csv.NewReader(f)
	cr.ReuseRecord = true
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("seed: %s: %s: %w", r.Seed.Name, path, err)
	}
	plan, err := planCSV(reflect.TypeFor[T](), header)
	if err != nil {
		return fmt.Errorf("seed: %s: %s: %w", r.Seed.Name, path, err)
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("seed: %s: %s: %w", r.Seed.Name, path, err)
		}
		var v T
		if err := plan.decode(reflect.ValueOf(&v).Elem(), rec); err != nil {
			line, _ := cr.FieldPos(0)
			return fmt.Errorf("seed: %s: %s line %d: %w", r.Seed.Name, path, line, err)
		}
		if err := fn(v); err != nil {
			return err
		}
	}
}
