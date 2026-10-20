// Package dsn holds what package pg and testkit share about connection
// strings.
package dsn

import (
	"regexp"
	"strings"
)

var urlForm = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// IsURL reports whether s, less surrounding whitespace, is in URL form,
// scheme://..., whatever the scheme, rather than keyword/value form, whose
// values may hold "://" themselves.
func IsURL(s string) bool { return urlForm.MatchString(strings.TrimSpace(s)) }
