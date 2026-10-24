package formula

import (
	"fmt"
	"strings"
)

// Reference renders the operator table and the function registry as Markdown, for the guide.
func Reference() string {
	var b strings.Builder
	b.WriteString("| Operator | Precedence | Meaning |\n|---|---|---|\n")
	for _, o := range operators {
		form := "a " + o.Symbol + " b"
		switch o.Form {
		case "prefix":
			form = o.Symbol + "a"
			if o.Symbol == "not" {
				form = "not a"
			}
		case "postfix":
			form = "a " + o.Symbol
		}
		fmt.Fprintf(&b, "| `%s` | %d | %s |\n", escapeCell(form), o.Precedence, o.Doc)
	}
	b.WriteString("\n| Function | Returns | Kind | Meaning | Example |\n|---|---|---|---|---|\n")
	for _, f := range registry {
		kind := "row"
		if f.Aggregate {
			kind = "aggregate"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | `%s` |\n", escapeCell(f.Signature()), f.Returns(), kind, f.Doc, escapeCell(f.Example))
	}
	return b.String()
}

func escapeCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
