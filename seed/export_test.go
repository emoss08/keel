package seed

import "github.com/emoss08/keel"

// NewRunForTest returns a Run of s outside an Apply, for testing the readers.
func NewRunForTest(db *keel.DB, s *Seed) (*Run, error) {
	gr, err := prepare([]*Seed{s})
	if err != nil {
		return nil, err
	}
	return &Run{DB: db, Seed: s, prep: gr.bySeed[s], values: &values{m: map[string]any{}}}, nil
}
