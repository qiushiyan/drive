package main

import (
	"slices"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit/clikittest"
)

// Every help page, pinned: an agent learns drive from them. `make golden`
// rewrites them after an intended change, and the diff is reviewed.
func TestHelp(t *testing.T) {
	clikittest.Golden(t, app, "testdata/help")
}

// A watch or timer an older drive started re-execs this binary by the old
// form once an install replaces it, and a wake it posts may still say
// `drive watch --sleep`: each runs here as the same operation.
func TestOlderDrivesCommandLinesStillRun(t *testing.T) {
	for _, c := range []struct{ old, now []string }{
		{[]string{"--batch", "b1", "wait", "T1", "--notify"}, []string{"--batch", "b1", "notify", "T1"}},
		{[]string{"--batch", "b1", "wait", "--sleep", "300", "--note", "veto", "--notify"}, []string{"--batch", "b1", "ring", "300", "--", "veto"}},
		{[]string{"watch", "--sleep", "900", "-T1", "retry"}, []string{"timer", "900", "--", "-T1", "retry"}},
		{[]string{"wait", "T1"}, []string{"wait", "T1"}},
		{[]string{"watch", "T1"}, []string{"watch", "T1"}},
	} {
		got := upgrade(c.old)
		if !slices.Equal(got, c.now) {
			t.Errorf("%q reads as %q, want %q", c.old, got, c.now)
			continue
		}
		k, err := kong.New(&CLI{}, app.Options...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.Parse(got); err != nil {
			t.Errorf("%q does not parse: %v", got, err)
		}
	}
}
