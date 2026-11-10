package drive_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// holding puts S2 in the batch on a step that took release, its turn going
// as reply says, beside S1, which wants the hold next.
func holding(t *testing.T, wake string, reply func(c *testkit.Claude, prompt string)) (r *rig, holder, waiter *testkit.Claude) {
	t.Helper()
	r = newRig(t, wake)
	holder, waiter = adopted(t, r, "S2"), adopted(t, r, "S1")
	holder.Edit(func(c *testkit.Claude) { c.Reply = reply })
	if _, err := r.d.Send(r.view("S2"), drive.Step{Line: "release, then start the eval"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	return r, holder, waiter
}

// evalRunsOn ends a turn with its eval left running in the background.
func evalRunsOn(c *testkit.Claude, _ string) {
	c.Job("uv run eval --suite search")
	c.Done("Released. The eval runs on in the background.")
}

// On 2026-11-09 a step sent with --hold release also started an eval that
// ran nine hours in the background. Its turn ended and the release was done
// within the hour, but the job kept the hold: nine sends were refused on it,
// and the driver took a second hold name to get past it. A driver hands such
// a hold back, saying why; from then on every reader finds it free, until a
// later send takes it.
func TestAHoldOnlyAJobKeepsCanBeFreed(t *testing.T) {
	r, holder, waiter := holding(t, "webhook", evalRunsOn)
	merge := drive.Step{Line: "merge when ready"}
	_, err := r.d.Send(r.view("S1"), merge, drive.SendOptions{Hold: "release"})
	if !isRefused(err) || !strings.Contains(err.Error(), "a background job it started still runs: uv run eval --suite search") {
		t.Fatalf("the job keeps the hold: %v", err)
	}
	if next := nextOf(err); !strings.Contains(next, "if that job is not what release was taken for, free the hold, saying why: drive --batch b1 free release") {
		t.Fatalf("the refusal names the free: %s", next)
	}
	if err := r.d.Park(r.view("S1"), "waits for release, held by S2", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Free("release", "…"); !isRefused(err) || len(r.events(batch.KindFree)) != 0 {
		t.Fatalf("a free says why, in words: %v", err)
	}
	const why = "S2 released an hour ago; the job it left is its eval, which releases nothing"
	f, err := r.d.Free("release", why)
	if err != nil || f.By != "S2" {
		t.Fatalf("freed: %+v %v", f, err)
	}
	if text := f.Text(); !strings.HasPrefix(text, "freed release: S2 held it since ") || !strings.Contains(text, "uv run eval --suite search") {
		t.Fatalf("it says what it freed, and from what: %q", text)
	}
	if !strings.Contains(f.Next, "drive --batch b1 send "+r.view("S1")+" --hold release 'merge when ready'") {
		t.Fatalf("its next is the send of the session that waits: %q", f.Next)
	}
	frees := r.events(batch.KindFree)
	if len(frees) != 1 || frees[0].Session != "S2" || frees[0].Hold != "release" || frees[0].Reason != why || !batch.Heard(frees[0]) {
		t.Fatalf("the log keeps the free and its reason, as a driver's trace: %+v", frees)
	}
	// Free for every reader: status, watch --due, the page, the log.
	st, err := r.d.Status(false)
	if err != nil || len(st.Holds) != 1 || !strings.HasPrefix(st.Holds[0], "release: free") || !strings.HasSuffix(st.Holds[0], "drive --batch b1 send "+r.view("S1")+" --hold release 'merge when ready'") {
		t.Fatalf("status reads it free, with its waiter's send: %q %v", st.Holds, err)
	}
	for _, row := range st.Rows {
		if row.Label == "S1" && !strings.Contains(row.Watch, "release is free now") {
			t.Fatalf("the parked waiter's line says so: %q", row.Watch)
		}
	}
	// The waiter stays parked with no watch of its own: its turn ended long
	// before, and the news is the hold.
	armed, later, err := r.d.Due()
	if err != nil || len(armed) != 0 || r.watches.Alive(watchOf(waiter)) {
		t.Fatalf("--due arms no watch for a parked waiter: %+v %v", armed, err)
	}
	if len(later) != 1 || later[0].Free != "release" || !strings.Contains(later[0].LaterText(), "drive --batch b1 send "+r.view("S1")+" --hold release 'merge when ready'") {
		t.Fatalf("--due says the hold is free and what to send: %+v", later)
	}
	if ov, err := r.d.Overview(true); err != nil || len(ov.Holds) != 1 || !strings.HasPrefix(ov.Holds[0], "release: free") {
		t.Fatalf("the page reads it free: %+v %v", ov, err)
	}
	if _, evs, err := r.d.Log(time.Time{}); err != nil || !strings.Contains(drive.LogText(r.b, evs, r.w.Clock.Now()), "release: "+why) {
		t.Fatalf("the log prints the free with its reason: %v", err)
	}
	// Freed twice, it says nothing holds it, and records nothing more.
	if f, err := r.d.Free("release", why); err != nil || f.By != "" || !strings.HasPrefix(f.Text(), "release is free: nothing holds it") || len(r.events(batch.KindFree)) != 1 {
		t.Fatalf("free already: %+v %v", f, err)
	}
	// The waiter's send takes it, the holder's eval still running, and it is held again.
	waiter.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	if _, err := r.d.Send(r.view("S1"), merge, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatalf("the waiter takes the hold: %v", err)
	}
	if _, err := r.d.Send(r.view("S2"), drive.Step{Line: "and the changelog"}, drive.SendOptions{Hold: "release"}); !isRefused(err) || !strings.Contains(err.Error(), "release is held by S1") {
		t.Fatalf("taken again, it is held: %v", err)
	}
	if st, _ := r.d.Status(false); len(st.Holds) != 1 || !strings.HasPrefix(st.Holds[0], "release: held by S1") {
		t.Fatalf("status reads the new holder: %q", st.Holds)
	}
	// The eval it left keeps no hold its holder takes afterwards.
	waiter.Done("merged and released")
	holder.Edit(func(c *testkit.Claude) { c.Reply = nil })
	if _, err := r.d.Send(r.view("S2"), drive.Step{Line: "and the changelog"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Send(r.view("S1"), drive.Step{Line: "release the docs"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatalf("a job older than the taking keeps nothing: %v", err)
	}
}

// Each session that waits for a freed hold is named with what it was
// refused, a send or an open, and the first to go takes the hold.
func TestAFreeNamesEverySessionThatWaits(t *testing.T) {
	r, _, _ := holding(t, "webhook", evalRunsOn)
	if _, err := r.d.Send(r.view("S1"), drive.Step{Line: "merge when ready"}, drive.SendOptions{Hold: "release"}); !isRefused(err) {
		t.Fatalf("held: %v", err)
	}
	if _, err := r.d.Open("notes", drive.Step{Line: "write the release notes"}, drive.OpenOptions{Dir: r.w.Root, Hold: "release"}); !isRefused(err) {
		t.Fatalf("held: %v", err)
	}
	f, err := r.d.Free("release", "the job S2 left is its eval")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"drive --batch b1 send " + r.view("S1") + " --hold release 'merge when ready'",
		"drive --batch b1 open notes --dir " + r.w.Root + " --hold release 'write the release notes'",
	} {
		if !strings.Contains(f.Next, want) {
			t.Errorf("%q is missing from: %s", want, f.Next)
		}
	}
	// The open takes it, and the send that waited is refused on the open's session.
	o, err := r.d.Open("notes", drive.Step{Line: "write the release notes"}, drive.OpenOptions{Dir: r.w.Root, Hold: "release"})
	if err != nil || o.Sent == nil {
		t.Fatalf("the open takes the freed hold: %+v %v", o, err)
	}
}

// A hold its holder's turn still needs stays held: free is refused while the
// turn runs, a later step included, while it waits on a prompt, while its
// send is under way, and while it cannot be read, each time saying why and
// what to run instead.
func TestAHoldItsHoldersTurnNeedsIsNotFreed(t *testing.T) {
	const why = "the job it left is its eval"
	runsOn := func(*testkit.Claude, string) {}
	for _, tc := range []struct {
		name, wake string
		reply      func(c *testkit.Claude, prompt string)
		then       func(r *rig, holder *testkit.Claude) // what happens before the free
		reason     string
		next       []string
	}{
		{"its turn still runs", "webhook", runsOn, nil, "whose turn is still running",
			[]string{"S2's wake comes when its turn ends", "drive --batch b1 free release '" + why + "'"}},
		{"its turn still runs, for a driver that waits itself", "none", runsOn, nil, "whose turn is still running",
			[]string{"drive --batch b1 wait S2", "drive --batch b1 free release '" + why + "'"}},
		{"it works on a later step", "webhook", evalRunsOn, func(r *rig, holder *testkit.Claude) {
			holder.Edit(func(c *testkit.Claude) { c.Reply = runsOn })
			if _, err := r.d.Send(r.view("S2"), drive.Step{Line: "and tag it"}, drive.SendOptions{}); err != nil {
				r.t.Fatal(err)
			}
		}, "who works on a later step", []string{"S2's wake comes when its turn ends"}},
		{"it waits on a prompt", "webhook", func(c *testkit.Claude, _ string) { c.Ask("git push --tags") }, nil, "who waits on a prompt",
			[]string{"drive --batch b1 read S2 --pending", " approve", " deny"}},
		{"it cannot be read", "webhook", evalRunsOn, func(r *rig, _ *testkit.Claude) { r.w.StatusFails = 1 }, "whose session cannot be read",
			[]string{"try it again: drive --batch b1 free release '" + why + "'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, holder, _ := holding(t, tc.wake, tc.reply)
			if tc.then != nil {
				tc.then(r, holder)
			}
			f, err := r.d.Free("release", why)
			if !isRefused(err) || f != nil || !strings.Contains(err.Error(), "release stays held by S2 since ") || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("refused, saying why: %+v %v", f, err)
			}
			next := nextOf(err)
			for _, want := range tc.next {
				if !strings.Contains(next, want) {
					t.Errorf("%q is missing from: %s", want, next)
				}
			}
			if tc.wake == "webhook" && strings.Contains(next, "drive --batch b1 wait") {
				t.Errorf("a chat driver cannot hold a wait open: %s", next)
			}
			for _, m := range printedView.FindAllStringSubmatch(next, -1) {
				if now := r.view("S2"); m[1] != now {
					t.Errorf("printed view %s, but S2 stands at %s: %s", m[1], now, next)
				}
			}
			if len(r.events(batch.KindFree)) != 0 {
				t.Fatal("a refusal frees nothing")
			}
			// Still held, and the refusal made its holder no waiter.
			if _, err := r.d.Send(r.view("S1"), drive.Step{Line: "merge when ready"}, drive.SendOptions{Hold: "release"}); !isRefused(err) || !strings.Contains(err.Error(), "release is held by S2") {
				t.Fatalf("the hold stands: %v", err)
			}
			if st, _ := r.d.Status(false); len(st.Holds) != 1 || strings.Contains(st.Holds[0], "waiting for it: S2") {
				t.Fatalf("its holder waits for nothing: %q", st.Holds)
			}
		})
	}
	t.Run("its send is under way", func(t *testing.T) {
		r := newRig(t, "webhook")
		holder := adopted(t, r, "S2")
		gate := make(chan struct{})
		holder.Edit(func(c *testkit.Claude) { c.TypeGate = gate })
		sent := make(chan error, 1)
		v := r.view("S2")
		go func() {
			_, err := r.d.Send(v, drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"})
			sent <- err
		}()
		admitted(r, "S2", "release")
		_, err := r.d.Free("release", why)
		close(gate)
		if serr := <-sent; serr != nil {
			t.Fatal(serr)
		}
		if !isRefused(err) || !strings.Contains(err.Error(), "whose send is still under way") || !strings.Contains(nextOf(err), "drive --batch b1 read S2") {
			t.Fatalf("refused while the send that takes it is typed: %v", err)
		}
	})
}

// A hold nothing holds is free already, which is no error: its holder's
// turn ended with no job left, or no step ever named it.
func TestFreeingAHoldNothingHoldsSaysSo(t *testing.T) {
	r, _, _ := holding(t, "webhook", nil) // the turn that took release ends at once
	const why = "its turn ended"
	f, err := r.d.Free("release", why)
	if err != nil || f.By != "" || f.Text() != "release is free: nothing holds it\n" {
		t.Fatalf("free already: %+v %v", f, err)
	}
	f, err = r.d.Free("relase", why)
	if err != nil || !strings.Contains(f.Text(), "no step in this batch has named a hold relase") || !strings.Contains(f.Text(), "release") {
		t.Fatalf("a name no step has named says so, and names the batch's holds: %+v %v", f, err)
	}
	if len(r.events(batch.KindFree)) != 0 || len(r.events(batch.KindRefused)) != 0 {
		t.Fatal("nothing is recorded")
	}
}

// A session parked behind a hold was armed a watch on every watchdog run once
// the hold was free, and each watch posted "its turn ended": four posts in
// twenty minutes for one view while a run heard each and left it parked,
// parking it again with a time included, none of them naming the hold. A
// hold that comes free is news of its own, told once by the watchdog to
// whoever waits for it, an open that was refused included, with the send or
// open each was refused; again only while no run has heard, and anew once
// the hold has been taken or refused again.
func TestAFreeHoldIsToldOnceToWhoeverWaits(t *testing.T) {
	r, holder, waiter := holding(t, "webhook", evalRunsOn)
	l := hook(t, r)
	merge := drive.Step{Line: "merge when ready"}
	if _, err := r.d.Send(r.view("S1"), merge, drive.SendOptions{Hold: "release"}); !isRefused(err) {
		t.Fatalf("refused on the hold: %v", err)
	}
	if err := r.d.Park(r.view("S1"), "waits for release, held by S2", time.Time{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.w.Root, "work")
	if _, err := r.d.Open("J1", drive.Step{Line: "judge the release"}, drive.OpenOptions{Dir: dir, Hold: "release"}); !isRefused(err) {
		t.Fatalf("an open refused on the hold waits too, with no session yet: %v", err)
	}
	if _, err := r.d.Free("release", "S2 released; the job it left is its eval"); err != nil {
		t.Fatal(err)
	}
	// due runs the watchdog once the run before it is over, and says what it posted.
	due := func() string {
		t.Helper()
		r.w.Clock.Sleep(11 * time.Minute)
		reports, err := r.d.DueAll()
		if err != nil || len(reports) != 1 || reports[0].Err != nil {
			t.Fatalf("the watchdog ran: %+v %v", reports, err)
		}
		if r.watches.Alive(watchOf(waiter)) {
			t.Fatal("a parked waiter is armed no watch")
		}
		return reports[0].Posted
	}
	posted := due()
	if l.count() != 1 || !strings.Contains(posted, "send "+r.view("S1")+" --hold release 'merge when ready'") || !strings.Contains(posted, "open J1 --dir "+dir+" --hold release 'judge the release'") {
		t.Fatalf("told once, each waiter with what it was refused: %d %q", l.count(), posted)
	}
	if posted = due(); l.count() != 2 || !strings.Contains(posted, "--hold release 'merge when ready'") {
		t.Fatalf("no run answered, so it is told again: %d %q", l.count(), posted)
	}
	r.d.Seen() // a run hears it, and leaves both waiting
	if posted = due(); posted != "" || l.count() != 2 {
		t.Fatalf("heard, it is told no more: %d %q", l.count(), posted)
	}
	// Parking the waiter again, with a time, is its driver's answer: no news.
	if err := r.d.Park(r.view("S1"), "the merge can wait until tomorrow", r.w.Clock.Now().Add(20*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if posted = due(); posted != "" || l.count() != 2 {
		t.Fatalf("parked again, it is told no more: %d %q", l.count(), posted)
	}
	if st, _ := r.d.Status(false); len(st.Holds) != 1 || !strings.HasPrefix(st.Holds[0], "release: free") || !strings.Contains(st.Holds[0], "open J1 ") {
		t.Fatalf("status still says it is free, and for whom: %q", st.Holds)
	}
	// The holder takes it again, the waiter is refused again, and it comes
	// free again: that is news.
	holder.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	if _, err := r.d.Send(r.view("S2"), drive.Step{Line: "and the changelog"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Send(r.view("S1"), merge, drive.SendOptions{Hold: "release"}); !isRefused(err) {
		t.Fatalf("held again: %v", err)
	}
	if posted = due(); posted != "" || l.count() != 2 {
		t.Fatalf("held, there is nothing to tell: %d %q", l.count(), posted)
	}
	holder.Done("the changelog is in")
	if posted = due(); l.count() != 3 || !strings.Contains(posted, "--hold release 'merge when ready'") {
		t.Fatalf("free again after it was taken, it is told again: %d %q", l.count(), posted)
	}
}
