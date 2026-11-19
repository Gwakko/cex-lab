package journal

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type rec struct {
	N int
	S string
}

func TestAppendAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []rec{{1, "a"}, {2, "b"}, {3, "c"}}
	for _, r := range want {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	j.Close()

	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	got, err := LoadAll[rec](j)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTornTailIsTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	j, _ := Open(path)
	if err := j.Append(rec{1, "ok"}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"N":2,"S":"tor`) // crash mid-write
	f.Close()

	j, _ = Open(path)
	defer j.Close()
	got, err := LoadAll[rec](j)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want only the complete record", got)
	}
	if err := j.Append(rec{2, "again"}); err != nil {
		t.Fatal(err)
	}
	got, _ = LoadAll[rec](j)
	if !reflect.DeepEqual(got, []rec{{1, "ok"}, {2, "again"}}) {
		t.Fatalf("after re-append: %v", got)
	}
}

func TestCorruptRecordFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	os.WriteFile(path, []byte("{\"N\":1}\nnot json\n"), 0o600)
	j, _ := Open(path)
	defer j.Close()
	if _, err := LoadAll[rec](j); err == nil {
		t.Fatal("expected error for corrupt record")
	}
}
