package logs

import (
	"path/filepath"
	"testing"
)

func TestRedactAcrossWrites(t *testing.T) {
	file := filepath.Join(t.TempDir(), "log")
	w, err := Create(file, []string{"supersecret", "abc"})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("token=super"))
	w.Write([]byte("secret ok\nshort abc stays\ntail supersecret"))
	w.Close()

	b, next, err := Read(file, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := "token=*** ok\nshort abc stays\ntail ***\n"
	if string(b) != want {
		t.Errorf("got %q, want %q", b, want)
	}
	rest, next2, _ := Read(file, next, 10)
	if len(rest) != 0 || next2 != next {
		t.Errorf("expected EOF at %d, got %q at %d", next, rest, next2)
	}
	part, n, _ := Read(file, 6, 3)
	if string(part) != "***" || n != 9 {
		t.Errorf("offset read = %q %d", part, n)
	}
}
