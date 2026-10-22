package main

import (
	"strings"
	"testing"

	"github.com/qiushiyan/drive/internal/batch"
)

// drive log is the driver's memory after a reset: a timer's wake says which
// timer it was, and a respond says where the session went.
func TestTheLogKeepsATimersNoteAndARespondsOutcome(t *testing.T) {
	for _, c := range []struct {
		e    batch.Event
		want []string
	}{
		{batch.Event{Kind: batch.KindWake, HTTP: 204, Try: 1, Note: "veto"}, []string{"veto", "204"}},
		{batch.Event{Kind: batch.KindWake, Try: 6, Note: "veto", Reason: "http 503 unavailable"}, []string{"veto", "http 503"}},
		{batch.Event{Kind: batch.KindRespond, Session: "T2", Action: "approve", Call: "Bash: pnpm install", Outcome: "working"},
			[]string{"approve", "pnpm install", "working"}},
	} {
		got := describe(c.e)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("a %s reads %q, without %q", c.e.Kind, got, w)
			}
		}
	}
}
