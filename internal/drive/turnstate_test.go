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

// A permission prompt opens seconds before Rex hears of it; a queued line
// typed then would answer it.
func TestAPromptRexHasNotHeardOfTakesNoStep(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("deploy the lab host")
	c.AskQuietly("terraform apply")
	_, err := r.d.Send(r.view("T1"), drive.Step{Line: "also update the changelog"}, drive.SendOptions{Queue: true})
	if !isRefused(err) || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("a send to a session on a prompt is refused, queued or not: %v", err)
	}
	if typed, _ := c.Seen(); len(without(typed, "Read ")) != 1 {
		t.Fatalf("nothing is typed into the prompt: %q", typed)
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
