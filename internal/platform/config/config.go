// Package config loads configuration from layered sources without touching
// the process environment: the real environment first, then .env.local, then
// .env. It returns values and where each came from; callers build typed
// settings from Lookup. It never sets environment variables.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Files are read in this order; later files override earlier ones, and the
// process environment overrides both.
var Files = []string{".env", ".env.local"}

// OriginEnv marks a value that came from the process environment.
const OriginEnv = "environment"

var keyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Source is the merged result.
type Source struct {
	values map[string]string
	origin map[string]string
}

// Load reads dir/.env and dir/.env.local (missing files are fine) and layers
// environ (as returned by os.Environ) on top. Pass os.Environ() in
// production and a fixed slice in tests.
func Load(dir string, environ []string) (*Source, error) {
	s := &Source{values: map[string]string{}, origin: map[string]string{}}
	for _, name := range Files {
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		vals, err := Parse(f, name)
		f.Close()
		if err != nil {
			return nil, err
		}
		s.merge(vals, name)
	}
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	s.merge(env, OriginEnv)
	return s, nil
}

func (s *Source) merge(vals map[string]string, origin string) {
	for k, v := range vals {
		s.values[k] = v
		s.origin[k] = origin
	}
}

// Lookup returns a value and whether it was set by any source.
func (s *Source) Lookup(key string) (string, bool) {
	v, ok := s.values[key]
	return v, ok
}

// Origin says which source a key came from ("environment", ".env.local",
// ".env"), or "" if unset.
func (s *Source) Origin(key string) string { return s.origin[key] }

// Keys returns the set keys with the given prefix, sorted. Use it to log
// which keys were loaded and from where, never their values.
func (s *Source) Keys(prefix string) []string {
	var out []string
	for k := range s.values {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// Parse reads dotenv syntax: KEY=value lines, blank lines, # comments, an
// optional "export " prefix, 'single' (literal) or "double" (with \n \t \" \\
// escapes) quoted values, and inline # comments after unquoted values or
// after a closing quote. Any other line is an error naming name and the line.
func Parse(r io.Reader, name string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %v", name, n, err)
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func parseLine(line string) (string, string, error) {
	line = strings.TrimPrefix(line, "export ")
	key, rest, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", errors.New(`expected KEY=value`)
	}
	key = strings.TrimSpace(key)
	if !keyRE.MatchString(key) {
		return "", "", fmt.Errorf("invalid key %q", key)
	}
	val, err := parseValue(strings.TrimSpace(rest))
	if err != nil {
		return "", "", fmt.Errorf("%s: %v", key, err)
	}
	return key, val, nil
}

func parseValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", errors.New("unterminated single quote")
		}
		return v[1 : end+1], afterQuote(v[end+2:])
	case '"':
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			c := v[i]
			switch {
			case c == '"':
				return b.String(), afterQuote(v[i+1:])
			case c == '\\' && i+1 < len(v):
				i++
				switch v[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case '"', '\\':
					b.WriteByte(v[i])
				default:
					return "", fmt.Errorf(`unknown escape \%c`, v[i])
				}
			default:
				b.WriteByte(c)
			}
		}
		return "", errors.New("unterminated double quote")
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	if strings.ContainsAny(v, `"'`) {
		return "", errors.New("quotes inside an unquoted value; quote the whole value")
	}
	return v, nil
}

func afterQuote(rest string) error {
	rest = strings.TrimSpace(rest)
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return fmt.Errorf("unexpected text after closing quote: %q", rest)
	}
	return nil
}
