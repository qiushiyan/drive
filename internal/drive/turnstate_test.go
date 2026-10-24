package drive_test

import (
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// Where a turn stands is Claude Code's own record first: Rex hears only what
// a hook reports, and each case here is one no hook reports in time.

// A message from another session waiting for approval tells Rex the session
// is blocked; the session itself is idle at its input box and takes a step.
func TestAHeldMessageLeavesTheSessionDrivable(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.HoldMessage()
	if rd, _ := r.d.Read("T1", drive.ReadLast, 1); rd.State != "done" {
		t.Fatalf("the session reads as it is, not as Rex was told: %+v", rd)
	}
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{}); err != nil {
		t.Fatalf("a held message does not stop a step: %v", err)
	}
}

// Rex reads blocked until an approved command's PostToolUse; the session is
// busy from the approval on.
func TestAnApprovedCommandThatRunsLongReadsWorking(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("build it")
	c.Edit(func(c *testkit.Claude) { c.SlowTool = true })
	c.Ask("make build")
	if st, _ := r.d.Status(false); st.Rows[0].Input != "prompt" {
		t.Fatalf("the prompt's options are not typed text: %+v", st.Rows[0])
	}
	if rd, _ := r.d.Read("T1", drive.ReadPending, 1); rd.Input != "prompt" {
		t.Fatalf("nor to read: %+v", rd)
	}
	res, err := r.d.Respond(r.view("T1"), "approve")
	if err != nil || res.After != "working" {
		t.Fatalf("an approve is confirmed by the session going to work: %+v %v", res, err)
	}
	if st, _ := r.w.Status(c.Block); st.State != "blocked" {
		t.Fatalf("the fake keeps Rex on the prompt until PostToolUse, as the real one does: %+v", st)
	}
	ended := make(chan *drive.Ending, 1)
	go func() {
		e, _ := r.d.Wait("T1", time.Minute, 10*time.Minute)
		ended <- e
	}()
	select {
	case e := <-ended:
		t.Fatalf("the wait ended while the approved command ran: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
	c.ToolDone()
	c.Done("built")
	if e := <-ended; e == nil || e.State != "done" {
		t.Fatalf("the wait ends with the turn: %+v", e)
	}
}

// Enter typed into a permission prompt answers it, so nothing goes into
// one, --queue or not, whether Rex has heard of the prompt or, in its first
// seconds, only the registry has: turnState reads them on different lines.
func TestNothingIsTypedIntoAPrompt(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  func(c *testkit.Claude, command string)
	}{
		{"Rex told", (*testkit.Claude).Ask},
		{"Rex not told yet", (*testkit.Claude).AskQuietly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "none")
			c := adopted(t, r, "T1")
			c.Busy("deploy the lab host")
			tc.ask(c, "terraform apply")
			typed0, keys0 := c.Seen()
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "also update the changelog"}, drive.SendOptions{Queue: true})
			if !isRefused(err) || !strings.Contains(err.Error(), "permission prompt") {
				t.Fatalf("refused: %v", err)
			}
			if typed, keys := c.Seen(); len(typed) != len(typed0) || len(keys) != len(keys0) {
				t.Fatalf("nothing typed or pressed: %q %q", typed[len(typed0):], keys[len(keys0):])
			}
		})
	}
}

// A prompt queued behind a turn fires its hook when it is queued, so Rex
// reads done from that turn's Stop while the queued turn runs.
func TestAQueuedTurnReadsWorking(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("write the report")
	if sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "then summarise it"}, drive.SendOptions{Queue: true}); err != nil || sent.Outcome != "queued" {
		t.Fatalf("queued: %+v %v", sent, err)
	}
	c.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} }) // the queued turn runs on
	c.Done("the report")
	if st, _ := r.w.Status(c.Block); st.State != "done" {
		t.Fatalf("the fake leaves Rex on the first turn's done, as the real one does: %+v", st)
	}
	if rd, _ := r.d.Read("T1", drive.ReadLast, 1); rd.State != "working" {
		t.Fatalf("the queued turn is a turn under way: %+v", rd)
	}
}

// A typed /compact fires no prompt hook and no Stop.
func TestACompactionReadsWorkingUntilItEnds(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Edit(func(c *testkit.Claude) { c.SlowCompact = true })
	if sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "/compact"}, drive.SendOptions{}); err != nil || sent.Outcome != "arrived" {
		t.Fatalf("/compact: %+v %v", sent, err)
	}
	if rd, _ := r.d.Read("T1", drive.ReadLast, 1); rd.State != "working" {
		t.Fatalf("a compaction under way is work: %+v", rd)
	}
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "implement-spec"}, drive.SendOptions{}); !isRefused(err) {
		t.Fatalf("the next step waits for the compaction: %v", err)
	}
	c.Compacted()
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "implement-spec"}, drive.SendOptions{}); err != nil {
		t.Fatalf("a finished /compact takes its next step: %v", err)
	}
}

// Claude Code that crashed never told Rex it ended; a wait on it ends.
func TestACrashedSessionIsGone(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("a long build")
	c.Crash()
	ended := make(chan *drive.Ending, 1)
	go func() {
		e, _ := r.d.Wait("T1", time.Minute, 10*time.Minute)
		ended <- e
	}()
	select {
	case e := <-ended:
		if e == nil || e.State != "gone" {
			t.Fatalf("the wait reports the session gone: %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never ended: Rex still reads working")
	}
}

// A turn that ended with a background job running is done, the job named;
// a hold that job keeps says so, and the refusal of a waiter gives the step
// that has the holder stop it, where an interrupt cannot.
func TestABackgroundJobIsShownAndStoppedBySession(t *testing.T) {
	r := newRig(t, "none")
	a, b := adopted(t, r, "T1"), adopted(t, r, "T2")
	a.Edit(func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) { c.Job("find ~ -name twin.toml"); c.Done("merged") }
	})
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "close it out"}, drive.SendOptions{Hold: "main"}); err != nil {
		t.Fatal(err)
	}
	a.HoldMessage() // Rex is told something else; the session's own record reads shell
	rd, _ := r.d.Read("T1", drive.ReadLast, 1)
	if rd.State != "done" || len(rd.Jobs) != 1 || rd.Jobs[0].Command != "find ~ -name twin.toml" {
		t.Fatalf("done, its job named: %+v", rd)
	}
	_, err := r.d.Send(r.view("T2"), drive.Step{Line: "close it out"}, drive.SendOptions{Hold: "main"})
	if !isRefused(err) || !strings.Contains(err.Error(), "find ~ -name twin.toml") || !strings.Contains(err.Error(), "drive send "+r.view("T1")) {
		t.Fatalf("the hold names the job that keeps it, and the step that stops it: %v", err)
	}
	if _, err := r.d.Respond(r.view("T1"), "interrupt"); !isRefused(err) || !strings.Contains(err.Error(), "drive send "+r.view("T1")) {
		t.Fatalf("an interrupt stops no job, and says what does: %v", err)
	}
	_ = b
}

// A hold its holder keeps through a later step says so: a follow-up to a
// closeout is still the closeout.
func TestAHoldKeptForALaterStepSaysSo(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T1")
	adopted(t, r, "T2")
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "close it out"}, drive.SendOptions{Hold: "main"}); err != nil {
		t.Fatal(err)
	}
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "skip the push"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Send(r.view("T2"), drive.Step{Line: "close it out"}, drive.SendOptions{Hold: "main"}); !isRefused(err) || !strings.Contains(err.Error(), "later step") {
		t.Fatalf("the hold stands while its holder works on, and says why: %v", err)
	}
}

// A compaction answers with its output, not a reply; read shows it.
func TestACompactionsOutputIsItsAnswer(t *testing.T) {
	r := newRig(t, "none")
	adopted(t, r, "T1")
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "/compact"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rd, _ := r.d.Read("T1", drive.ReadLast, 1); !strings.Contains(rd.Body, "/compact finished") || !strings.Contains(rd.Body, "Compacted") {
		t.Fatalf("read shows the compaction's output: %q", rd.Body)
	}
}

// A session closed after its last step is still read from its transcript,
// as a report run after a reset needs its recap; it has no view to act on.
func TestAClosedSessionIsReadFromItsTranscript(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	done(c, "recap", "Recap: built --shout.")
	if err := r.d.Close(r.view("T1")); err != nil {
		t.Fatal(err)
	}
	rd, err := r.d.Read("T1", drive.ReadLast, 1)
	if err != nil || rd.State != "closed" || !strings.Contains(rd.Body, "built --shout") || rd.View.Session != "" {
		t.Fatalf("its recap, read closed: %+v %v", rd, err)
	}
	if rd, err := r.d.Read("T1", drive.ReadUser, 1); err != nil || rd.Body != "recap" {
		t.Fatalf("its last prompt: %+v %v", rd, err)
	}
	if _, err := r.d.Read("T1", drive.ReadScreen, 1); !isRefused(err) || !strings.Contains(err.Error(), "drive read T1") {
		t.Fatalf("no screen, and what to read instead: %v", err)
	}
	// One whose process ended without a close reads the same way, as gone.
	c2 := adopted(t, r, "T2")
	c2.Crash()
	if rd, err := r.d.Read("T2", drive.ReadLast, 1); err != nil || rd.State != "gone" || rd.View.Session != "" {
		t.Fatalf("a crashed session, read gone: %+v %v", rd, err)
	}
}

// A session parked to wait for a hold says so while it waits, and that the
// hold is free once it is: its park reason alone would still name the holder.
func TestAParkedWaiterSaysWhenItsHoldIsFree(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T1")
	adopted(t, r, "T2")
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "close it out"}, drive.SendOptions{Hold: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Send(r.view("T2"), drive.Step{Line: "close it out"}, drive.SendOptions{Hold: "main"}); !isRefused(err) {
		t.Fatalf("held: %v", err)
	}
	if err := r.d.Park(r.view("T2"), "waits for main, held by T1"); err != nil {
		t.Fatal(err)
	}
	a.Done("merged")
	st, _ := r.d.Status(false)
	for _, row := range st.Rows {
		if row.Label == "T2" && !strings.Contains(row.Watch, "main is free now") {
			t.Fatalf("its line says the hold is free: %q", row.Watch)
		}
	}
}
