package keel

import (
	"strings"
	"unicode"

	"github.com/emoss08/keel/schema"
)

// SearchMode is how a search string becomes a tsquery.
type SearchMode uint8

const (
	// WebSearch reads the string as a web search engine does: words are ANDed, "quoted words"
	// match as a phrase, or separates alternatives and -word excludes a word
	// (websearch_to_tsquery). It is the default.
	WebSearch SearchMode = iota + 1
	// Plain ANDs the string's words, ignoring punctuation and operators (plainto_tsquery).
	Plain
	// Phrase matches the string's words next to each other, in order (phraseto_tsquery).
	Phrase
	// Prefix matches each word of the string as the start of a word, so a word typed in part
	// matches: "dal ree" is to_tsquery('dal':* & 'ree':*).
	Prefix
	// AnyWord matches rows holding any of the string's words: to_tsquery('dallas' | 'reefer').
	AnyWord
	// AnyPrefix matches rows holding a word that starts with any of the string's words.
	AnyPrefix
)

// SearchOption shapes a full-text search: the configurations and modes its query is built with,
// and the normalization of a rank.
type SearchOption func(*search)

// SearchConfig builds the query with each of the text search configurations named and ORs them:
// SearchConfig("simple", "english") finds a word whether the column's vector stemmed it or not.
// Names are configuration identifiers ([a-z_][a-z0-9_]*, optionally schema-qualified); Postgres
// refuses one it does not have when the statement runs. Without SearchConfig the column's
// tsvector=<config> tag option applies, else the database's default_text_search_config.
func SearchConfig(configs ...string) SearchOption {
	return func(s *search) { s.configs = append(s.configs[:0:0], configs...) }
}

// SearchModes builds the query with each of modes and ORs them: SearchModes(keel.WebSearch,
// keel.Prefix) finds whole words and the words being typed. Without it the mode is WebSearch.
func SearchModes(modes ...SearchMode) SearchOption {
	return func(s *search) { s.modes = append(s.modes[:0:0], modes...) }
}

// RankNormalization sets the normalization of Rank and RankCD, the bit mask ts_rank takes as its
// third argument (1 divides the rank by 1 + the logarithm of the document length, 32 scales it to
// rank/(rank+1), …). Match ignores it.
func RankNormalization(mask int) SearchOption {
	return func(s *search) { s.norm, s.hasNorm = int64(mask), true }
}

// search is a full-text query: the text, the configurations ("" is the database default) and
// modes it is built with, the word lists of the word modes, and the error of an invalid option.
type search struct {
	text    string
	configs []string
	modes   []SearchMode
	norm    int64
	hasNorm bool
	words   map[SearchMode]string
	err     error
}

// newSearch builds the search of text over column c (nil when the searched expression is not a
// column) with opts.
func newSearch(c *schema.Column, text string, opts []SearchOption) *search {
	s := &search{text: text}
	for _, o := range opts {
		o(s)
	}
	if len(s.configs) == 0 {
		cfg := ""
		if c != nil {
			cfg = c.TSConfig
		}
		s.configs = []string{cfg}
	}
	if len(s.modes) == 0 {
		s.modes = []SearchMode{WebSearch}
	}
	for _, cfg := range s.configs {
		if cfg != "" && !validConfig(cfg) {
			s.err = errf("text search configuration %q must match [a-z_][a-z0-9_]* or schema.name", cfg)
		}
	}
	for _, m := range s.modes {
		if m < WebSearch || m > AnyPrefix {
			s.err = errf("unknown search mode %d", m)
		}
		if m >= Prefix && s.words[m] == "" {
			if s.words == nil {
				s.words = make(map[SearchMode]string, 1)
			}
			s.words[m] = wordQuery(text, m)
		}
	}
	return s
}

// validConfig reports whether name is a text search configuration identifier, optionally
// qualified by one schema.
func validConfig(name string) bool {
	schemaName, cfg, qualified := strings.Cut(name, ".")
	if qualified {
		return validAlias(schemaName) && validAlias(cfg)
	}
	return validAlias(name)
}

// wordQuery writes the tsquery text of a word mode: each word of text quoted as a lexeme, with :*
// for the prefix modes, joined by & for Prefix and | for the any modes. It is empty when text has
// no words.
func wordQuery(text string, m SearchMode) string {
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var b strings.Builder
	for i, w := range words {
		if i > 0 {
			if m == Prefix {
				b.WriteString(" & ")
			} else {
				b.WriteString(" | ")
			}
		}
		b.WriteByte('\'')
		b.WriteString(w)
		b.WriteByte('\'')
		if m != AnyWord {
			b.WriteString(":*")
		}
	}
	return b.String()
}

// tsqueryFuncs are the functions that turn a search string into a tsquery, by mode.
var tsqueryFuncs = map[SearchMode]string{
	WebSearch: "websearch_to_tsquery(", Plain: "plainto_tsquery(", Phrase: "phraseto_tsquery(",
	Prefix: "to_tsquery(", AnyWord: "to_tsquery(", AnyPrefix: "to_tsquery(",
}

// writeQuery writes the search's tsquery: one term per configuration and mode, ORed. Word modes
// whose string has no words are left out, and a query left with no term is NULL, which matches
// nothing.
func (s *search) writeQuery(r *render) {
	if s.err != nil {
		r.fail(s.err)
		return
	}
	terms := 0
	for _, m := range s.modes {
		if m < Prefix || s.words[m] != "" {
			terms += len(s.configs)
		}
	}
	if terms == 0 {
		r.s("NULL::tsquery")
		return
	}
	if terms > 1 {
		r.b('(')
	}
	first := true
	for _, cfg := range s.configs {
		for _, m := range s.modes {
			text := s.text
			if m >= Prefix {
				if text = s.words[m]; text == "" {
					continue
				}
			}
			if !first {
				r.s(" || ")
			}
			first = false
			r.s(tsqueryFuncs[m])
			if cfg != "" {
				r.b('\'')
				r.s(cfg)
				r.s("'::regconfig, ")
			}
			r.argTyped(text, "tsquery")
			r.b(')')
		}
	}
	if terms > 1 {
		r.b(')')
	}
}

// match writes e @@ query.
func (s *search) match(e Expr) Pred {
	return predExpr{e: func(r *render) {
		e.writeSQL(r)
		r.s(" @@ ")
		s.writeQuery(r)
	}}
}

// rank is fn(e, query[, normalization]): ts_rank or ts_rank_cd.
func (s *search) rank(fn string, e Expr) Agg {
	return Agg{fn: fn, custom: func(r *render) {
		r.s(fn)
		r.b('(')
		e.writeSQL(r)
		r.s(", ")
		s.writeQuery(r)
		if s.hasNorm {
			r.s(", ")
			r.argTyped(int32(s.norm), "rank normalization")
		}
		r.b(')')
	}}
}

// columnOf returns the schema column of a column reference, or nil.
func columnOf(c Col) *schema.Column {
	if c.Ref == nil || c.Ref.Table == nil {
		return nil
	}
	return &c.Ref.Table.Columns[c.Index]
}

// Match is col @@ query: the rows whose vector matches the search string, built by
// websearch_to_tsquery with the column's configuration unless opts say otherwise. The string is a
// bound parameter, so any user input is safe.
//
//	s.SearchVector.Match(q)                                              // websearch_to_tsquery
//	s.SearchVector.Match(q, keel.SearchConfig("simple", "english"))       // either configuration
//	s.SearchVector.Match(q, keel.SearchModes(keel.WebSearch, keel.Prefix)) // words or prefixes
func (c TSVectorCol) Match(query string, opts ...SearchOption) Pred {
	return newSearch(columnOf(c.Col), query, opts).match(c.Col)
}

// Rank is ts_rank(col, query): how well the row's vector matches the search string, a real
// number for Order and for a computed field. opts build the query as for Match.
func (c TSVectorCol) Rank(query string, opts ...SearchOption) Agg {
	return newSearch(columnOf(c.Col), query, opts).rank("ts_rank", c.Col)
}

// RankCD is ts_rank_cd(col, query): the cover density rank, which also weighs how close the
// matching words are to each other.
func (c TSVectorCol) RankCD(query string, opts ...SearchOption) Agg {
	return newSearch(columnOf(c.Col), query, opts).rank("ts_rank_cd", c.Col)
}

// dynSearch returns the search of query over d, with the configuration of d's column when d is
// one.
func (d DynExpr) dynSearch(query string, opts []SearchOption) *search {
	var c *schema.Column
	if d.n != nil && d.n.col != nil {
		c = columnOf(*d.n.col)
	}
	return newSearch(c, query, opts)
}

// Match is d @@ query, as TSVectorCol.Match: d is a tsvector column or expression.
func (d DynExpr) Match(query string, opts ...SearchOption) DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	s := d.dynSearch(query, opts)
	if s.err != nil {
		return dynFail(s.err)
	}
	return newDyn(s.match(d), d.Label())
}

// Rank is ts_rank(d, query), as TSVectorCol.Rank.
func (d DynExpr) Rank(query string, opts ...SearchOption) DynExpr {
	return d.dynRank("ts_rank", query, opts)
}

// RankCD is ts_rank_cd(d, query), as TSVectorCol.RankCD.
func (d DynExpr) RankCD(query string, opts ...SearchOption) DynExpr {
	return d.dynRank("ts_rank_cd", query, opts)
}

func (d DynExpr) dynRank(fn, query string, opts []SearchOption) DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	s := d.dynSearch(query, opts)
	if s.err != nil {
		return dynFail(s.err)
	}
	return newDyn(s.rank(fn, d), "rank")
}
