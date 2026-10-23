package seed

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

// prepared is a validated seed with its fingerprint.
type prepared struct {
	seed        *Seed
	fingerprint string
	inputs      map[string]string
	files       map[string]fs.FS
	code        string
}

// graph is a validated registry: its seeds in dependency order with their fingerprints.
type graph struct {
	seeds  []*prepared
	bySeed map[*Seed]*prepared
	byName map[string]*prepared
}

// prepare validates seeds and computes their fingerprints, reporting every problem at once.
func prepare(seeds []*Seed) (*graph, error) {
	member := make(map[*Seed]bool, len(seeds))
	for _, s := range seeds {
		member[s] = true
	}
	errs := checkNames(seeds)
	for _, s := range seeds {
		errs = append(errs, checkSeed(s, member)...)
	}
	errs = append(errs, cycles(seeds, member)...)
	g := &graph{bySeed: map[*Seed]*prepared{}, byName: map[string]*prepared{}}
	for _, s := range order(seeds) {
		p, perrs := fingerprint(s)
		errs = append(errs, perrs...)
		g.seeds = append(g.seeds, p)
		g.bySeed[s] = p
		g.byName[s.Name] = p
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return g, nil
}

// label names a seed in an error, even one without a name.
func label(s *Seed) string {
	if s.Name == "" {
		return "a seed without a name"
	}
	return fmt.Sprintf("seed %q", s.Name)
}

func checkNames(seeds []*Seed) []error {
	var errs []error
	seen := map[string]bool{}
	for _, s := range seeds {
		switch {
		case s.Name == "":
			errs = append(errs, errors.New("seed: a seed has no name"))
		case seen[s.Name]:
			errs = append(errs, fmt.Errorf("seed: %s is registered more than once", label(s)))
		}
		seen[s.Name] = true
	}
	return errs
}

func checkSeed(s *Seed, member map[*Seed]bool) []error {
	var errs []error
	if s.Kind != Reference && s.Kind != Fixture {
		errs = append(errs, fmt.Errorf("seed: %s has no Kind (seed.Reference or seed.Fixture)", label(s)))
	}
	if s.Run == nil {
		errs = append(errs, fmt.Errorf("seed: %s has no Run function", label(s)))
	}
	if s.Timeout < 0 {
		errs = append(errs, fmt.Errorf("seed: %s has a negative Timeout", label(s)))
	}
	for _, a := range s.After {
		switch {
		case a == nil:
			errs = append(errs, fmt.Errorf("seed: %s runs after a nil seed", label(s)))
		case a == s:
			errs = append(errs, fmt.Errorf("seed: %s runs after itself", label(s)))
		case !member[a]:
			errs = append(errs, fmt.Errorf("seed: %s runs after %s, which is not in the registry", label(s),
				label(a)))
		case s.Kind == Reference && a.Kind == Fixture:
			errs = append(errs, fmt.Errorf("seed: reference %s runs after fixture %s; reference data "+
				"cannot depend on development data", label(s), label(a)))
		}
	}
	return errs
}

// cycles reports each cycle among seeds once, starting from its first registered seed.
func cycles(seeds []*Seed, member map[*Seed]bool) []error {
	pos := make(map[*Seed]int, len(seeds))
	for i, s := range seeds {
		pos[s] = i
	}
	const (
		visiting = 1
		done     = 2
	)
	color := map[*Seed]int{}
	seen := map[string]bool{}
	var errs []error
	var stack []*Seed
	var visit func(s *Seed)
	visit = func(s *Seed) {
		color[s] = visiting
		stack = append(stack, s)
		for _, a := range s.After {
			if a == nil || a == s || !member[a] {
				continue
			}
			switch color[a] {
			case visiting:
				if msg := cycleMessage(stack, a, pos); !seen[msg] {
					seen[msg] = true
					errs = append(errs, errors.New(msg))
				}
			case 0:
				visit(a)
			}
		}
		stack = stack[:len(stack)-1]
		color[s] = done
	}
	for _, s := range seeds {
		if color[s] == 0 {
			visit(s)
		}
	}
	return errs
}

// cycleMessage describes the cycle that closes at a, rotated to start at its first registered seed,
// in run order.
func cycleMessage(stack []*Seed, a *Seed, pos map[*Seed]int) string {
	start := slices.Index(stack, a)
	loop := slices.Clone(stack[start:])
	slices.Reverse(loop)
	first := 0
	for i, s := range loop {
		if pos[s] < pos[loop[first]] {
			first = i
		}
	}
	loop = append(loop[first:], loop[:first]...)
	names := make([]string, 0, len(loop)+1)
	for _, s := range loop {
		names = append(names, s.Name)
	}
	names = append(names, loop[0].Name)
	return "seed: After cycle: " + strings.Join(names, " → ")
}

// order sorts seeds so each comes after the seeds it names in After, keeping registration order
// otherwise. Seeds in a cycle, which can never be placed, come last.
func order(seeds []*Seed) []*Seed {
	member := make(map[*Seed]bool, len(seeds))
	for _, s := range seeds {
		member[s] = true
	}
	placed := make(map[*Seed]bool, len(seeds))
	out := make([]*Seed, 0, len(seeds))
	for progress := true; progress; {
		progress = false
		for _, s := range seeds {
			if !placed[s] && ready(s, member, placed) {
				placed[s] = true
				out = append(out, s)
				progress = true
				break
			}
		}
	}
	for _, s := range seeds {
		if !placed[s] {
			out = append(out, s)
		}
	}
	return out
}

func ready(s *Seed, member, placed map[*Seed]bool) bool {
	for _, a := range s.After {
		if a != nil && a != s && member[a] && !placed[a] {
			return false
		}
	}
	return true
}

// fingerprint resolves a seed's inputs and computes its fingerprint: sha256 of its name, version,
// each input's path and content, and its Fingerprint function's result.
func fingerprint(s *Seed) (*prepared, []error) {
	p := &prepared{seed: s, inputs: map[string]string{}, files: map[string]fs.FS{}}
	var errs []error
	for _, in := range s.Inputs {
		errs = append(errs, p.addInput(in)...)
	}
	if s.Fingerprint != nil {
		code, err := callFingerprint(s)
		if err != nil {
			errs = append(errs, err)
		}
		p.code = hashHex([]byte(code))
	}
	h := sha256.New()
	writeField(h, "keel/seed/1")
	writeField(h, s.Name)
	writeField(h, s.Version)
	paths := slices.Sorted(maps.Keys(p.inputs))
	writeField(h, fmt.Sprint(len(paths)))
	for _, path := range paths {
		writeField(h, path)
		writeField(h, p.inputs[path])
	}
	writeField(h, p.code)
	p.fingerprint = hex.EncodeToString(h.Sum(nil))
	return p, errs
}

func callFingerprint(s *Seed) (code string, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("seed: %s: Fingerprint panicked: %v", label(s), v)
		}
	}()
	return s.Fingerprint(), nil
}

// addInput resolves one declared input, hashing each file it names.
func (p *prepared) addInput(in Input) []error {
	if in.FS == nil {
		return []error{fmt.Errorf("seed: %s: input %q has no file system", label(p.seed), in.Path)}
	}
	paths := []string{in.Path}
	if strings.ContainsAny(in.Path, `*?[\`) {
		matches, err := fs.Glob(in.FS, in.Path)
		if err != nil {
			return []error{fmt.Errorf("seed: %s: input pattern %q: %w", label(p.seed), in.Path, err)}
		}
		if len(matches) == 0 {
			return []error{fmt.Errorf("seed: %s: input pattern %q matches no file", label(p.seed), in.Path)}
		}
		paths = matches
	}
	var errs []error
	for _, path := range paths {
		if _, dup := p.files[path]; dup {
			errs = append(errs, fmt.Errorf("seed: %s: input %q is declared twice", label(p.seed), path))
			continue
		}
		data, err := fs.ReadFile(in.FS, path)
		if err != nil {
			errs = append(errs, fmt.Errorf("seed: %s: read input: %w", label(p.seed), err))
			continue
		}
		p.files[path] = in.FS
		p.inputs[path] = hashHex(data)
	}
	return errs
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeField writes s length-prefixed, so no two field lists hash alike.
func writeField(h hash.Hash, s string) {
	var n [binary.MaxVarintLen64]byte
	h.Write(n[:binary.PutUvarint(n[:], uint64(len(s)))])
	h.Write([]byte(s))
}
