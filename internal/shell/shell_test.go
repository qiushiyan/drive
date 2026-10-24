package shell

import (
	"os/exec"
	"strings"
	"testing"
)

// A quoted word reaches a program as the word it was, whatever it holds.
func TestQuoteRoundTripsThroughAShell(t *testing.T) {
	words := []string{"T1@3fa91c07", "it's done", "$HOME", "a b", "", "--queue", "~/dev", "x;y", "colon:ok", "新"}
	out, err := exec.Command("sh", "-c", "printf '%s\\n' "+Join(words...)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); strings.Join(got, "|") != strings.Join(words, "|") {
		t.Fatalf("%q came back as %q", words, got)
	}
	if Quote("T1@3fa91c07") != "T1@3fa91c07" || Quote("a b") != "'a b'" {
		t.Fatalf("plain words stay bare, others are quoted: %s %s", Quote("T1@3fa91c07"), Quote("a b"))
	}
}
