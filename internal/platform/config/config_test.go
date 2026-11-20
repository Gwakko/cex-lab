package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRules(t *testing.T) {
	src := `
# full-line comment

PLAIN=value
SPACES =  padded value
export EXPORTED=yes
EMPTY=
INLINE=abc # trailing comment
HASH_NO_SPACE=abc#def
SINGLE='lit #not a comment \n'
DOUBLE="tab\there \"quoted\" back\\slash"
NEWLINE="a\nb"
QUOTED_THEN_COMMENT="x" # comment
URL=http://user@127.0.0.1:8545/path?q=1
EQUALS=a=b=c
`
	got, err := Parse(strings.NewReader(src), ".env")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PLAIN":               "value",
		"SPACES":              "padded value",
		"EXPORTED":            "yes",
		"EMPTY":               "",
		"INLINE":              "abc",
		"HASH_NO_SPACE":       "abc#def",
		"SINGLE":              `lit #not a comment \n`,
		"DOUBLE":              "tab\there \"quoted\" back\\slash",
		"NEWLINE":             "a\nb",
		"QUOTED_THEN_COMMENT": "x",
		"URL":                 "http://user@127.0.0.1:8545/path?q=1",
		"EQUALS":              "a=b=c",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys: %v", len(got), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestMalformedLinesNameFileAndLine(t *testing.T) {
	cases := map[string]string{
		"A=1\nno equals sign\n":   ".env.local:2: expected KEY=value",
		"1BAD=x":                  ".env.local:1: invalid key",
		"A=\"open":                ".env.local:1: A: unterminated double quote",
		"A='open":                 ".env.local:1: A: unterminated single quote",
		"A=\"x\" trailing":        ".env.local:1: A: unexpected text after closing quote",
		"A=it's":                  ".env.local:1: A: quotes inside an unquoted value",
		"\n\n\nA=\"bad \\q esc\"": ".env.local:4: A: unknown escape",
	}
	for src, want := range cases {
		_, err := Parse(strings.NewReader(src), ".env.local")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", src, err, want)
		}
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrecedenceEnvThenLocalThenDotenv(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env", "A=from-env-file\nB=from-env-file\nC=from-env-file\n")
	write(t, dir, ".env.local", "B=from-local\nC=from-local\n")
	s, err := Load(dir, []string{"C=from-process", "UNRELATED=1"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][2]string{
		"A": {"from-env-file", ".env"},
		"B": {"from-local", ".env.local"},
		"C": {"from-process", OriginEnv},
	} {
		v, ok := s.Lookup(key)
		if !ok || v != want[0] || s.Origin(key) != want[1] {
			t.Errorf("%s = %q from %q, want %q from %q", key, v, s.Origin(key), want[0], want[1])
		}
	}
	if _, ok := s.Lookup("MISSING"); ok || s.Origin("MISSING") != "" {
		t.Fatal("unset key reported as set")
	}
	if got := s.Keys("A"); len(got) != 1 || got[0] != "A" {
		t.Fatalf("Keys = %v", got)
	}
}

func TestMissingFilesAreFine(t *testing.T) {
	s, err := Load(t.TempDir(), []string{"X=1"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Lookup("X"); v != "1" {
		t.Fatal("environment not loaded")
	}
}

func TestMalformedFileFailsLoad(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env", "OK=1\n")
	write(t, dir, ".env.local", "OK=2\nbroken line\n")
	if _, err := Load(dir, nil); err == nil || !strings.Contains(err.Error(), ".env.local:2") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadDoesNotTouchTheProcessEnvironment(t *testing.T) {
	const key = "CEXLAB_CONFIG_TEST_SHOULD_NOT_LEAK"
	if _, set := os.LookupEnv(key); set {
		t.Skipf("%s already set in the environment", key)
	}
	dir := t.TempDir()
	write(t, dir, ".env", key+"=1\n")
	before := len(os.Environ())
	if _, err := Load(dir, os.Environ()); err != nil {
		t.Fatal(err)
	}
	if _, set := os.LookupEnv(key); set || len(os.Environ()) != before {
		t.Fatal("Load modified the process environment")
	}
}
