package formula

import (
	"sort"
	"strings"
)

const maxSuggestions = 3

// Distance returns the Damerau-Levenshtein distance between a and b: the fewest insertions,
// deletions, substitutions and transpositions of adjacent characters that turn one into the other.
func Distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	inf := len(ra) + len(rb)
	d := make([][]int, len(ra)+2)
	for i := range d {
		d[i] = make([]int, len(rb)+2)
	}
	d[0][0] = inf
	for i := 0; i <= len(ra); i++ {
		d[i+1][0], d[i+1][1] = inf, i
	}
	for j := 0; j <= len(rb); j++ {
		d[0][j+1], d[1][j+1] = inf, j
	}
	last := map[rune]int{}
	for i := 1; i <= len(ra); i++ {
		match := 0
		for j := 1; j <= len(rb); j++ {
			i1, j1 := last[rb[j-1]], match
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost, match = 0, j
			}
			d[i+1][j+1] = min(
				d[i][j]+cost,
				d[i+1][j]+1,
				d[i][j+1]+1,
				d[i1][j1]+(i-i1-1)+1+(j-j1-1),
			)
		}
		last[ra[i-1]] = i
	}
	return d[len(ra)+1][len(rb)+1]
}

// Suggest returns the candidates closest to name, up to three, when they are close enough to be
// what was meant. A candidate equal to name ignoring case always qualifies.
func Suggest(name string, candidates []string) []string {
	type scored struct {
		name string
		dist int
	}
	limit := 1
	if n := len([]rune(name)); n > 2 {
		limit = max(2, n/3)
	}
	lower := strings.ToLower(name)
	var found []scored
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] || c == name {
			continue
		}
		seen[c] = true
		d := Distance(lower, strings.ToLower(c))
		if d <= limit {
			found = append(found, scored{c, d})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].dist != found[j].dist {
			return found[i].dist < found[j].dist
		}
		return found[i].name < found[j].name
	})
	var out []string
	for i := 0; i < len(found) && i < maxSuggestions && found[i].dist == found[0].dist; i++ {
		out = append(out, found[i].name)
	}
	return out
}
