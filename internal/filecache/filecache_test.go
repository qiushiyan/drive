package filecache

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestAReadingStandsOnlyWhileItsFileDoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	c := Cache[[]string]{Clone: slices.Clone[[]string]}
	read := func() ([]string, error) {
		reads++
		data, err := os.ReadFile(path)
		return []string{string(data)}, err
	}
	want := func(what, v string, n int) {
		t.Helper()
		got, err := c.Read(path, read)
		if err != nil || len(got) != 1 || got[0] != v || reads != n {
			t.Fatalf("%s: %q %v after %d reads, want %q after %d", what, got, err, reads, v, n)
		}
		got[0] = "a reader's own change"
	}

	if _, err := c.Read(path, read); !os.IsNotExist(err) || reads != 1 {
		t.Fatalf("a file that is not there is the reader's to report: %v", err)
	}
	write("one")
	want("a first look reads it", "one", 2)
	want("a look at a file that stands reads nothing, and no reader's change reaches the next", "one", 2)

	stamp, _ := os.Stat(path)
	write("three, longer")
	want("an append is a new size", "three, longer", 3)

	if err := os.Chtimes(path, time.Time{}, stamp.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	want("a touch is a new time", "three, longer", 4)

	other := path + ".new"
	if err := os.WriteFile(other, []byte("three, LONGER"), 0o644); err != nil {
		t.Fatal(err)
	}
	now, _ := os.Stat(path)
	if err := os.Chtimes(other, now.ModTime(), now.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	want("a file renamed into its place is another file, at the same size and time", "three, LONGER", 5)

	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)
	if _, err := c.Read(path, read); err == nil || reads != 6 {
		t.Fatalf("a file made unreadable is read again, and fails as it would: %v after %d reads", err, reads)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	want("readable again it stands as it was read, and a failed read left nothing behind", "three, LONGER", 6)
}

func TestAReadingOfAFileWrittenUnderTheReadIsNotKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	reads := 0
	c := Cache[string]{Clone: func(s string) string { return s }}
	read := func() (string, error) {
		reads++
		data, err := os.ReadFile(path)
		if reads == 1 { // a writer appends as the read ends
			if err := os.WriteFile(path, []byte("one, two"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return string(data), err
	}
	if got, _ := c.Read(path, read); got != "one" {
		t.Fatalf("the read's own answer: %q", got)
	}
	if got, _ := c.Read(path, read); got != "one, two" || reads != 2 {
		t.Fatalf("what was read before the write is not what the file reads as now: %q after %d reads", got, reads)
	}
}
