package gen

import (
	"fmt"
	"strings"
	"unicode"
)

// defaultAcronyms are the words written in capitals when Options.Acronyms is empty.
var defaultAcronyms = []string{"ID", "URL", "API", "JSON", "UUID"}

// labeler derives display labels from Go names.
type labeler struct {
	acronyms map[string]bool
}

// newLabeler returns a labeler that writes the given words in capitals, or the default acronyms
// when there are none.
func newLabeler(acronyms []string) (*labeler, error) {
	if len(acronyms) == 0 {
		acronyms = defaultAcronyms
	}
	l := &labeler{acronyms: map[string]bool{}}
	for _, a := range acronyms {
		a = strings.ToUpper(strings.TrimSpace(a))
		if a == "" || strings.IndexFunc(a, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) >= 0 {
			return nil, fmt.Errorf("acronym %q must be letters and digits", a)
		}
		l.acronyms[a] = true
	}
	return l, nil
}

// label turns a Go name into words: split at case changes, digits and underscores, acronyms in
// capitals, other words capitalised (ProNumber → "PRO Number" with PRO an acronym, CustomerID →
// "Customer ID", Address2 → "Address 2").
func (l *labeler) label(name string) string {
	words := splitWords(name)
	for i, w := range words {
		words[i] = l.word(w)
	}
	return strings.Join(words, " ")
}

// word writes one word in capitals when it is an acronym or already in capitals, else with its
// first letter in upper case.
func (l *labeler) word(w string) string {
	if up := strings.ToUpper(w); l.acronyms[up] || (len(w) > 1 && up == w) {
		return up
	}
	r := []rune(w)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// pluralLabel puts the last word of a label in the plural ("Shipment" → "Shipments", "Bill Of
// Lading" → "Bill Of Ladings").
func pluralLabel(label string) string {
	i := strings.LastIndexByte(label, ' ')
	return label[:i+1] + plural(label[i+1:])
}

// splitWords splits a Go name at underscores, at a lower-case letter followed by an upper-case
// letter, between letters and digits, and before the last capital of a run of capitals followed
// by a lower-case letter (HTTPServer → HTTP, Server), unless that letter is a plural s.
func splitWords(name string) []string {
	r := []rune(name)
	var words []string
	start := 0
	flush := func(end int) {
		if end > start {
			words = append(words, string(r[start:end]))
		}
		start = end
	}
	for i := 0; i < len(r); i++ {
		if r[i] == '_' {
			flush(i)
			start = i + 1
			continue
		}
		if i > start && boundary(r, i) {
			flush(i)
		}
	}
	flush(len(r))
	return words
}

// boundary reports whether a word starts at r[i].
func boundary(r []rune, i int) bool {
	prev, cur := r[i-1], r[i]
	switch {
	case unicode.IsDigit(prev) != unicode.IsDigit(cur):
		return true
	case unicode.IsUpper(cur) && unicode.IsLower(prev):
		return true
	case unicode.IsUpper(cur) && unicode.IsUpper(prev) && i+1 < len(r) && unicode.IsLower(r[i+1]):
		return !pluralS(r, i+1)
	}
	return false
}

// pluralS reports whether r[i] is an 's' that ends a word, making a run of capitals plural
// (CustomerIDs → Customer, IDs).
func pluralS(r []rune, i int) bool {
	return r[i] == 's' && (i+1 == len(r) || !unicode.IsLower(r[i+1]))
}
