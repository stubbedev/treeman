// Package ident provides validated identifier quoting for SQL engines.
//
// Identifiers (database/schema/table names) cannot be parameterized in
// SQL, so callers must interpolate them. This package centralizes that
// interpolation behind validate-then-quote helpers so a missed
// validation call cannot create an injection point.
package ident

import "fmt"

// validate rejects anything outside [A-Za-z0-9_]. Both MySQL and
// Postgres support a wider set inside quoted identifiers, but treeman
// only ever generates names from its own template engine — the strict
// charset is enough and keeps the trust surface small.
func validate(s string) error {
	if s == "" {
		return fmt.Errorf("empty identifier")
	}
	for _, c := range s {
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
		if !ok {
			return fmt.Errorf("invalid identifier: %q", s)
		}
	}
	return nil
}

// ValidateMySQL reports whether s is safe to interpolate into a
// backtick-quoted MySQL identifier.
func ValidateMySQL(s string) error { return validate(s) }

// ValidatePostgres reports whether s is safe to interpolate into a
// double-quoted Postgres identifier.
func ValidatePostgres(s string) error { return validate(s) }

// QuoteMySQL returns s wrapped in backticks, or an error if s is not
// a valid identifier.
func QuoteMySQL(s string) (string, error) {
	if err := validate(s); err != nil {
		return "", err
	}
	return "`" + s + "`", nil
}

// QuotePostgres returns s wrapped in double quotes, or an error if s
// is not a valid identifier.
func QuotePostgres(s string) (string, error) {
	if err := validate(s); err != nil {
		return "", err
	}
	return `"` + s + `"`, nil
}
