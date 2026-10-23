package schema

import (
	"fmt"
	"strings"
)

// FieldTag is a parsed `db` tag of a struct field.
type FieldTag struct {
	Name     string
	Flags    ColFlag
	IDPrefix string
	TSVector bool

	Rel        RelKind
	RelKey     string
	JoinTable  string
	JoinLocal  string
	JoinRemote string
}

// IsRelation reports whether the tag declares a relation.
func (t FieldTag) IsRelation() bool { return t.Rel != 0 }

// TableTag is the parsed `db` tag of the keel.Table marker.
type TableTag struct {
	Name string
}

// ParseTableTag parses the tag on a keel.Table marker field.
func ParseTableTag(tag string) (TableTag, error) {
	parts := strings.Split(tag, ",")
	name := strings.TrimSpace(parts[0])
	if err := checkIdent(name); err != nil {
		return TableTag{}, fmt.Errorf("table name: %w", err)
	}
	for _, opt := range parts[1:] {
		return TableTag{}, fmt.Errorf("unknown table option %q", strings.TrimSpace(opt))
	}
	return TableTag{Name: name}, nil
}

// ParseFieldTag parses the `db` tag of a struct field. A tag of "-" or "" means the field is not
// stored; ok is false for it.
func ParseFieldTag(tag string) (ft FieldTag, ok bool, err error) {
	if tag == "" || tag == "-" {
		return FieldTag{}, false, nil
	}
	parts := strings.Split(tag, ",")
	head := strings.TrimSpace(parts[0])
	if rel, key, isRel := parseRelation(head); isRel {
		if len(parts) > 1 {
			return FieldTag{}, false, fmt.Errorf("relation tag %q takes no options", tag)
		}
		ft.Rel = rel
		if rel == ManyToMany {
			segs := strings.Split(key, ".")
			if len(segs) != 3 {
				return FieldTag{}, false, fmt.Errorf(
					"m2m tag %q must be m2m:<join_table>.<local_fk>.<remote_fk>", tag,
				)
			}
			for _, s := range segs {
				if err := checkIdent(s); err != nil {
					return FieldTag{}, false, fmt.Errorf("m2m tag %q: %w", tag, err)
				}
			}
			ft.JoinTable, ft.JoinLocal, ft.JoinRemote = segs[0], segs[1], segs[2]
			return ft, true, nil
		}
		if err := checkIdent(key); err != nil {
			return FieldTag{}, false, fmt.Errorf("relation tag %q: %w", tag, err)
		}
		ft.RelKey = key
		return ft, true, nil
	}

	if err := checkIdent(head); err != nil {
		return FieldTag{}, false, fmt.Errorf("column tag %q: %w", tag, err)
	}
	ft.Name = head
	for _, raw := range parts[1:] {
		opt := strings.TrimSpace(raw)
		switch {
		case opt == "pk":
			ft.Flags |= FlagPK
		case opt == "tenant":
			ft.Flags |= FlagTenant
		case opt == "version":
			ft.Flags |= FlagVersion
		case opt == "autocreate":
			ft.Flags |= FlagAutoCreate
		case opt == "autoupdate":
			ft.Flags |= FlagAutoUpdate
		case opt == "readonly":
			ft.Flags |= FlagReadOnly
		case opt == "noselect":
			ft.Flags |= FlagNoSelect
		case opt == "jsonb":
			ft.Flags |= FlagJSONB
		case opt == "tsvector":
			ft.TSVector = true
		case strings.HasPrefix(opt, "autoid:"):
			ft.Flags |= FlagAutoID
			ft.IDPrefix = strings.TrimPrefix(opt, "autoid:")
			if ft.IDPrefix == "" {
				return FieldTag{}, false, fmt.Errorf("column tag %q: autoid needs a prefix", tag)
			}
		default:
			return FieldTag{}, false, fmt.Errorf("column tag %q: unknown option %q", tag, opt)
		}
	}
	if ft.Flags.Has(FlagReadOnly) && ft.Flags&(FlagPK|FlagTenant|FlagVersion|FlagAutoCreate|
		FlagAutoUpdate|FlagAutoID) != 0 {
		return FieldTag{}, false, fmt.Errorf("column tag %q: readonly excludes write options", tag)
	}
	return ft, true, nil
}

func parseRelation(head string) (RelKind, string, bool) {
	name, key, found := strings.Cut(head, ":")
	if !found {
		return 0, "", false
	}
	switch name {
	case "belongs":
		return BelongsTo, key, true
	case "one":
		return HasOne, key, true
	case "many":
		return HasMany, key, true
	case "m2m":
		return ManyToMany, key, true
	}
	return 0, "", false
}

func checkIdent(s string) error {
	if s == "" {
		return fmt.Errorf("empty identifier")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return fmt.Errorf("identifier %q must match [a-z0-9_]+", s)
		}
	}
	if s[0] >= '0' && s[0] <= '9' {
		return fmt.Errorf("identifier %q must not start with a digit", s)
	}
	return nil
}
