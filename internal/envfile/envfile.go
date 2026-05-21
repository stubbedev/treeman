// Package envfile reads shell-style env files (`.env`,
// `.env.testing`, etc.). Mirrors the subset of the dotenv spec used
// by Laravel, Symfony, Node, Python, Ruby, sqlx-cli.
//
// Supports KEY=VALUE / `export KEY=VALUE`, single/double quoted
// values, inline `# comment` on unquoted values, and `${OTHER}`
// expansion against earlier keys + the process env. Does NOT support
// multi-line values or command substitution.
package envfile

import (
	"bufio"
	"os"
	"strings"
)

// EnvFile is the parsed key/value map plus the on-disk source for
// provenance.
type EnvFile struct {
	Vars   map[string]string
	Source string // absolute path of the file this was parsed from; "" for inline parse
}

// Get returns the value for key, or empty + false if absent.
func (e EnvFile) Get(key string) (string, bool) {
	v, ok := e.Vars[key]
	return v, ok
}

// Merge layers `other` on top, but only inserts keys not already
// present. Used by ReadLayered when iterating .env → .env.testing
// → etc., since later files override earlier ones (caller handles
// ordering by calling Merge in the reverse order, or by direct
// insertion).
func (e *EnvFile) Merge(other EnvFile) {
	for k, v := range other.Vars {
		if _, exists := e.Vars[k]; !exists {
			e.Vars[k] = v
		}
	}
}

// Parse a string into an EnvFile (no Source).
func Parse(input string) EnvFile {
	out := EnvFile{Vars: map[string]string{}}
	sc := bufio.NewScanner(strings.NewReader(input))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimLeft(sc.Text(), " \t")
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if !validKey(key) {
			continue
		}
		raw := strings.TrimSpace(line[eq+1:])
		value := stripQuotesAndComment(raw)
		expanded := expand(value, out.Vars)
		out.Vars[key] = expanded
	}
	return out
}

// Read parses a file from disk. Returns empty + nil error when the
// file doesn't exist (callers iterate over conventional paths and
// most of them are absent).
func Read(path string) (EnvFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return EnvFile{Vars: map[string]string{}}, nil
		}
		return EnvFile{Vars: map[string]string{}}, err
	}
	e := Parse(string(b))
	e.Source = path
	return e, nil
}

// ReadOptional is the no-error variant of Read — non-existent or
// unreadable files yield an empty EnvFile. Use when iterating
// conventional dotenv paths where most are expected to be missing.
func ReadOptional(path string) EnvFile {
	e, _ := Read(path)
	return e
}

// ReadLayered walks the conventional env files for a repo, later
// files overriding earlier (Laravel's `.env` → `.env.testing`
// order). The returned EnvFile's Source field tracks the last file
// that actually existed — used by `resolve` for provenance labels.
func ReadLayered(paths []string) EnvFile {
	out := EnvFile{Vars: map[string]string{}}
	for _, p := range paths {
		one := ReadOptional(p)
		if one.Source != "" {
			out.Source = one.Source
		}
		for k, v := range one.Vars {
			out.Vars[k] = v
		}
	}
	return out
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	first := key[0]
	if !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z') || first == '_') {
		return false
	}
	for i := 1; i < len(key); i++ {
		c := key[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func stripQuotesAndComment(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	// Strip inline comments on unquoted values: `VAL # note`.
	if idx := strings.Index(s, " #"); idx >= 0 {
		return strings.TrimRight(s[:idx], " \t")
	}
	return s
}

// expand resolves `${VAR}` against the file's own prior keys, then
// the process env. No recursion.
func expand(s string, locals map[string]string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end >= 0 {
				key := s[i+2 : i+2+end]
				if v, ok := locals[key]; ok {
					b.WriteString(v)
				} else if v := os.Getenv(key); v != "" {
					b.WriteString(v)
				} else {
					b.WriteString(s[i : i+2+end+1])
				}
				i += 2 + end + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
