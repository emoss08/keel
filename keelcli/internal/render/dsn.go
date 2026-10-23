package render

import (
	"net/url"
	"strings"
)

var secretParams = []string{"password", "sslpassword", "sslkey"}

const redacted = "keelcli-redacted"

// DSN redacts a connection string for display: the password and secret parameters of a URL, or
// of a key/value string, become "***".
func DSN(dsn string) string {
	if !strings.Contains(dsn, "://") {
		fields := strings.Fields(dsn)
		for i, f := range fields {
			if k, _, ok := strings.Cut(f, "="); ok && isSecret(k) {
				fields[i] = k + "=***"
			}
		}
		return strings.Join(fields, " ")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable DSN)"
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), redacted)
	}
	q := u.Query()
	for _, p := range secretParams {
		if q.Has(p) {
			q.Set(p, redacted)
		}
	}
	u.RawQuery = q.Encode()
	return strings.ReplaceAll(u.String(), redacted, "***")
}

func isSecret(k string) bool {
	for _, p := range secretParams {
		if k == p {
			return true
		}
	}
	return false
}
