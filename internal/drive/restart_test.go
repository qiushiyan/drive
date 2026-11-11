package drive_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/testkit"
)

// inRexSession makes the rig's batch one whose sessions open in the Rex
// session "work", as every batch the owner starts names one.
func inRexSession(t *testing.T, r *rig) {
	t.Helper()
	b, _, err := r.d.Store.New(batch.Meta{Name: "b2", Started: r.w.Clock.Now(), Wake: r.b.Meta.Wake, RexSession: "work"})
	if err != nil {
		t.Fatal(err)
	}
	r.b, r.d.BatchName = b, "b2"
}

// reopenOf is the open a gone session's next: names, as its options.
func reopenOf(t *testing.T, next, label string) drive.OpenOptions {
	t.Helper()
	_, cmd, ok := strings.Cut(next, "drive --batch b2 open "+label+" ")
	if !ok {
		t.Fatalf("no open of %s in %q", label, next)
	}
	var opt drive.OpenOptions
	f := strings.Fields(cmd)
	for i := 0; i+1 < len(f); i += 2 {
		switch f[i] {
		case "--dir":
			opt.Dir = f[i+1]
		case "--resume":
			opt.Resume = f[i+1]
		case "--account":
			opt.Account = f[i+1]
		default:
			t.Fatalf("the open takes nothing else: %q", next)
		}
	}
	return opt
}

// A Rex restart ends every block and the batch's Rex session with them.
// Each session is gone, and drive names the one command that brings it
// back under its label, its conversation resumed: on 10-29 the gone wakes
// named none, and the bots parked asking how. The open starts the Rex
// session again, once; a parked label stays parked, with no wake to reach
// its driver, and keeps what was sent to it; one cut short mid-turn wakes
// as interrupted, not as a turn that ended.
func TestARestartedSessionComesBackUnderItsLabel(t *testing.T) {
	r := newRig(t, "webhook")
	inRexSession(t, r)
	busy := adopted(t, r, "T1")
	busy.Busy("build it")
	parked := adopted(t, r, "T2")
	if err := r.d.Park(r.view("T2"), "waits for the owner's call on D2", r.later()); err != nil {
		t.Fatal(err)
	}
	r.w.Restart()

	e, err := r.d.Wait("T1", time.Minute, 10*time.Minute)
	if err != nil || e.State != "gone" || !strings.Contains(e.Next, "--resume "+busy.SID+" --account acct@example.com") {
		t.Fatalf("the gone wake names the open that brings it back: %+v %v", e, err)
	}
	st, err := r.d.Status(false)
	if err != nil || len(st.Rows) != 2 || st.Rows[1].Next == "" || !strings.Contains(st.Text(r.w.Clock.Now()), "T2 is gone: its block no longer exists; if it was closed to end the session, put that on record: drive --batch b2 close T2; if not, bring it back") {
		t.Fatalf("status reads with the batch's Rex session gone, and names each open: %+v %v", st, err)
	}
	if rd, err := r.d.Read("T2", drive.ReadLast, 10); err != nil || rd.State != "gone" || rd.Next != st.Rows[1].Next {
		t.Fatalf("read names it too: %+v %v", rd, err)
	}

	for _, label := range []string{"T1", "T2"} {
		next := e.Next
		if label == "T2" {
			next = st.Rows[1].Next
		}
		o, err := r.d.Open(label, drive.Step{}, reopenOf(t, next, label))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if parked := o.Parked != ""; parked != (label == "T2") || !strings.Contains(o.Text(), "T2 stays parked") == parked {
			t.Fatalf("%s: only the parked label stays parked, and says so: %+v\n%s", label, o, o.Text())
		}
	}
	if n := r.w.RexSessions("work"); n != 1 {
		t.Fatalf("the Rex session starts again once: %d", n)
	}
	st, _ = r.d.Status(false)
	// Its park comes back whole, the time it is to be looked at again with it.
	if w := st.Rows[1].Watch; !strings.HasPrefix(w, "parked until ") || !strings.HasSuffix(w, ": waits for the owner's call on D2") || !st.Rows[0].Armed || st.Rows[1].Armed {
		t.Fatalf("the parked label gets no wake, the other one does: %+v", st.Rows)
	}
	if st.Rows[1].LastSent != "start" || st.Rows[1].SID != parked.SID {
		t.Fatalf("its own conversation carries on, with what was sent to it: %+v", st.Rows[1])
	}
	if e, err := r.d.Wait("T1", time.Minute, 10*time.Minute); err != nil || e.State != "interrupted" || strings.Contains(e.Reply, "No response requested") {
		t.Fatalf("a turn the restart cut short reads as interrupted: %+v %v", e, err)
	}
}

// A server that saves its sessions gives each block back as a plain shell, with
// its id and label. rex-resume normally resumes the conversation there; when it
// did not, the open a gone session names brings it back in that block, where
// the owner's tab is, instead of leaving the shell beside a new one.
func TestARestoredBlockResumesInPlace(t *testing.T) {
	r := newRig(t, "webhook")
	inRexSession(t, r)
	busy := adopted(t, r, "T1")
	busy.Busy("build it")
	back := adopted(t, r, "T2")
	front := adopted(t, r, "T3")
	r.w.Restore()
	front.Front = "nvim"

	// Its shell resumed it: nothing is gone, and no open is needed.
	r.w.BackInBlock(back.Block)
	if rd, err := r.d.Read("T2", drive.ReadLast, 10); err != nil || rd.State == "gone" {
		t.Fatalf("a conversation its shell resumed reads as running: %+v %v", rd, err)
	}
	if _, err := r.d.Open("T2", drive.Step{}, drive.OpenOptions{Dir: back.Cwd, Resume: back.SID}); err == nil {
		t.Fatal("an open of a session already back is refused")
	}

	e, err := r.d.Wait("T1", time.Minute, 10*time.Minute)
	if err != nil || e.State != "gone" {
		t.Fatalf("no conversation in the block yet: %+v %v", e, err)
	}
	o, err := r.d.Open("T1", drive.Step{}, reopenOf(t, e.Next, "T1"))
	if err != nil {
		t.Fatal(err)
	}
	if o.Block != busy.Block {
		t.Fatalf("it comes back in its own block, %s: %s", busy.Block, o.Block)
	}
	typed := busy.Typed[len(busy.Typed)-1]
	if !strings.HasPrefix(typed, "cd ") || !strings.Contains(typed, "--resume "+busy.SID) || strings.Contains(typed, "exec zsh") {
		t.Fatalf("the shell gets the launch as a line: %q", typed)
	}
	if n := r.w.RexSessions("work"); n != 1 {
		t.Fatalf("no Rex session starts: %d", n)
	}
	if e, err := r.d.Wait("T1", time.Minute, 10*time.Minute); err != nil || e.State != "interrupted" {
		t.Fatalf("a turn the restart cut short reads as interrupted: %+v %v", e, err)
	}

	// Another program in front: nothing is typed into it.
	before := len(front.Typed)
	if _, err := r.d.Open("T3", drive.Step{}, drive.OpenOptions{Dir: front.Cwd, Resume: front.SID}); err == nil {
		t.Fatal("Claude Code does not start behind another program")
	}
	if len(front.Typed) != before {
		t.Fatalf("nothing is typed into another program: %q", front.Typed)
	}
}

// While Rex's server is down nothing in it can be read, and drive starts
// none: a server a watch started on 10-29 gave every pane the watch's
// NO_COLOR=1 and TERM=dumb. A watch waits the restart out and wakes on
// what it finds once a server runs; a command says the server is down.
func TestADownServerIsWaitedOutNotTakenForGone(t *testing.T) {
	r := newRig(t, "none")
	adopted(t, r, "T1")
	r.w.Down = 20
	if e, err := r.d.Wait("T1", time.Minute, 10*time.Minute); err != nil || e.State != "done" {
		t.Fatalf("the wait outlasts the server's downtime: %+v %v", e, err)
	}
	r.w.Down = 1
	_, err := r.d.Status(false)
	if !isRefused(err) || !errors.Is(err, rex.ErrNoServer) || !strings.Contains(err.Error(), "Rex's server is not running") {
		t.Fatalf("status says the server is down: %v", err)
	}
	r.w.Down = 3
	if _, err := r.d.Read("T1", drive.ReadLast, 10); !isRefused(err) || !errors.Is(err, rex.ErrNoServer) {
		t.Fatalf("so does read: %v", err)
	}
}

// Before a planned restart every live batch's wakes stop, whichever driver
// drives it, so none wakes its driver mid-restart; on 10-29 the restore did
// it by hand, pidfile by pidfile. Each batch's status says so, and its
// watch --due arms them again, the one command stop-all names per batch.
func TestStopAllStopsEveryLiveBatchsWakesUntilDue(t *testing.T) {
	r := newRig(t, "webhook")
	newDriverBatch(t, r, "2026-10-28", "session-driver")
	adopted(t, r, "S1")
	newDriverBatch(t, r, "itell-2026-10-28", "itell")
	adopted(t, r, "P1")
	if err := r.d.Timer(600, "veto"); err != nil {
		t.Fatal(err)
	}
	stopped, err := r.d.StopAll()
	labels := map[string]string{}
	for _, s := range stopped {
		labels[s.Batch.Meta.Name] = strings.Join(s.Labels, ",")
	}
	if err != nil || len(stopped) != 3 || labels["2026-10-28"] != "S1" || labels["itell-2026-10-28"] != "P1" || labels["b1"] != "" {
		t.Fatalf("every driver's wakes stop, the rig's own batch's among them: %v %v", labels, err)
	}
	out := drive.StoppedText(stopped)
	for _, want := range []string{"batch b1, driver of no name: no wake was armed", "batch 2026-10-28, driver session-driver: stopped the wakes of S1",
		"drive --batch itell-2026-10-28 watch --due", "timers run on"} {
		if !strings.Contains(out, want) {
			t.Errorf("stop-all says %q:\n%s", want, out)
		}
	}
	for name := range r.watches.Running {
		if !strings.Contains(name, "-timer-") {
			t.Errorf("a session's watch still runs: %s", name)
		}
	}
	st, _ := r.d.Status(false)
	if !strings.HasPrefix(st.Rows[0].Watch, "stopped ") || st.Rows[0].Due != "its wake was stopped" {
		t.Fatalf("status says the wake was stopped, and is due: %+v", st.Rows[0].Wake)
	}
	if armed, _, err := r.d.Due(); err != nil || len(armed) != 1 || armed[0].Label != "P1" {
		t.Fatalf("watch --due arms it again: %+v %v", armed, err)
	}
}

// An account at its usage limit takes no step from any session until the
// limit lifts, days away for a weekly one, so the failure keeps the account
// on record, and the open that brings the session back once it is gone
// resumes it on another: on 2026-11-03 S8's gone wake named the account
// that had just hit its weekly limit.
func TestAnAccountAtItsLimitIsNotOfferedAgain(t *testing.T) {
	r := newRig(t, "webhook")
	accounts(r, "acct@example.com", "other@example.com")
	c := adopted(t, r, "T1")
	c.Reply = func(c *testkit.Claude, _ string) { c.Limit(r.w.Clock.Now().Add(45 * time.Hour)) }
	_, err := r.d.Send(r.view("T1"), drive.Step{Line: "build it"}, drive.SendOptions{})
	if f, ok := errors.AsType[*drive.Failed](err); !ok || !strings.Contains(f.What, "weekly limit") {
		t.Fatalf("the step met the limit: %v", err)
	}
	if evs := r.events(batch.KindFailure); len(evs) != 1 || evs[0].Account != "acct@example.com" {
		t.Fatalf("the failure keeps the account: %+v", evs)
	}
	if err := r.w.Close(c.Block); err != nil {
		t.Fatal(err)
	}
	rd, err := r.d.Read("T1", drive.ReadLast, 10)
	if err != nil || rd.State != "gone" || !strings.Contains(rd.Next, "--resume "+c.SID+" --account other@example.com") {
		t.Fatalf("the open that brings it back is on another account: %+v %v", rd, err)
	}
}

// A usage limit lifts only at its reset, days away for a weekly one, so a
// step that meets it goes on another account, and the next: that says so
// runs as written: Stop on the limit's menu, which writes nothing, the close
// on the same view, and the session resumed there with the step. On
// 2026-11-03 drive offered a 15-minute retry timer, and the owner had to
// say "exit these sessions and continue with x --resume".
func TestAStepThatMeetsAUsageLimitGoesOnAnotherAccount(t *testing.T) {
	r := newRig(t, "webhook")
	accounts(r, "acct@example.com", "other@example.com")
	c := adopted(t, r, "T1")
	resets := r.w.Clock.Now().Add(45 * time.Hour)
	c.Reply = func(c *testkit.Claude, _ string) { c.Limit(resets) }
	_, err := r.d.Send(r.view("T1"), drive.Step{Line: "build it"}, drive.SendOptions{})
	f, ok := errors.AsType[*drive.Failed](err)
	if !ok || f.Login {
		t.Fatalf("a usage limit is a failure of the step, not a rejected login: %v", err)
	}
	v := r.view("T1")
	want := "drive --batch b1 respond " + v + " approve && drive --batch b1 close " + v + " && drive --batch b1 open T1 --dir " + c.Cwd +
		" --resume " + c.SID + " --account other@example.com 'build it'"
	if !strings.Contains(f.Next, "takes no step until "+resets.Format("Mon 2 Jan 15:04")) || !strings.Contains(f.Next, want) {
		t.Fatalf("the next relaunches it with its step, saying until when its account is out:\n%s\nwant %s", f.Next, want)
	}
	rd, err := r.d.Read("T1", drive.ReadLast, 10)
	if err != nil || !strings.Contains(rd.Next, want) {
		t.Fatalf("read says the same of the session on its limit's menu: %+v %v", rd, err)
	}
	if _, err := r.d.Respond(v, "approve"); err != nil {
		t.Fatalf("approve takes Stop: %v", err)
	}
	if err := r.d.Close(v); err != nil {
		t.Fatalf("the close is decided on the same view: %v", err)
	}
	o, err := r.d.Open("T1", drive.Step{Line: "build it"}, drive.OpenOptions{Dir: c.Cwd, Resume: c.SID, Account: "other@example.com"})
	if err != nil || o.Account != "other@example.com" || o.Sent == nil || o.Sent.Outcome != "arrived" {
		t.Fatalf("it resumes on the other account with its step: %+v %v", o, err)
	}
}

// With no other account to take it, the owner is told, and the session
// parks until the limit lifts.
func TestAUsageLimitWithNoOtherAccountParksUntilItLifts(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	resets := r.w.Clock.Now().Add(45 * time.Hour)
	c.Reply = func(c *testkit.Claude, _ string) { c.Limit(resets) }
	_, err := r.d.Send(r.view("T1"), drive.Step{Line: "build it"}, drive.SendOptions{})
	v := r.view("T1")
	want := "tell the owner, and park it until then: drive --batch b1 respond " + v + " approve && drive --batch b1 park " + v +
		" --until '" + resets.In(time.UTC).Format("2006-01-02 15:04") + "' 'its account is at its usage limit'"
	if f, ok := errors.AsType[*drive.Failed](err); !ok || !strings.Contains(f.Next, want) {
		t.Fatalf("%v\nwant %s", err, want)
	}
}

// accounts makes each account a session could be relaunched on.
func accounts(r *rig, names ...string) {
	for _, n := range names {
		_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude-accounts", n, "sessions"), 0o755)
	}
}

// A limit a session meets mid-turn, which its wake or a driver's read finds,
// goes on record as one a step meets does, so a later reopen steers clear of
// the account: a driver that reads the limit and relaunches leaves no watch
// to find it.
func TestALimitMetMidTurnIsKeptOnRecord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		finds func(r *rig, c *testkit.Claude)
	}{
		{"its wake finds it", func(r *rig, c *testkit.Claude) { hook(r.t, r); watch(r.t, r, c, "T1") }},
		{"a read finds it", func(r *rig, c *testkit.Claude) {
			if _, err := r.d.Read("T1", drive.ReadLast, 10); err != nil {
				r.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "webhook")
			accounts(r, "acct@example.com", "other@example.com")
			a := adopted(t, r, "T1")
			b := adopted(t, r, "T2")
			a.Busy("build it")
			a.Limit(r.w.Clock.Now().Add(45 * time.Hour))
			tc.finds(r, a)
			if err := r.w.Close(b.Block); err != nil {
				t.Fatal(err)
			}
			rd, err := r.d.Read("T2", drive.ReadLast, 10)
			if err != nil || !strings.Contains(rd.Next, "--account other@example.com") {
				t.Fatalf("T2's reopen steers clear of the account found limited on T1: %+v %v", rd, err)
			}
		})
	}
}

// A limit lifts at its reset: an account whose limit has lifted takes steps
// again, so a later limit on another account relaunches there rather than
// parking for days.
func TestALimitThatLiftedFreesItsAccount(t *testing.T) {
	r := newRig(t, "webhook")
	accounts(r, "acct@example.com", "other@example.com")
	a := adopted(t, r, "T1")
	a.Reply = func(c *testkit.Claude, _ string) { c.Limit(r.w.Clock.Now().Add(time.Hour)) }
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "build it"}, drive.SendOptions{}); err == nil {
		t.Fatal("the step met the limit")
	}
	r.w.Clock.Sleep(2 * time.Hour)
	o, err := r.d.Open("T2", drive.Step{}, drive.OpenOptions{Dir: a.Cwd, Account: "other@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	b := r.w.Session(o.Block)
	b.Reply = func(c *testkit.Claude, _ string) { c.Limit(r.w.Clock.Now().Add(45 * time.Hour)) }
	_, err = r.d.Send(r.view("T2"), drive.Step{Line: "build it"}, drive.SendOptions{})
	if f, ok := errors.AsType[*drive.Failed](err); !ok || !strings.Contains(f.Next, "--account acct@example.com") || strings.Contains(f.Next, " park ") {
		t.Fatalf("relaunched on the account whose limit lifted: %v", err)
	}
}

// A relaunch's own first step can meet a limit too: its next steers clear of
// every account the batch found taking no step, not only the one it opened on.
func TestARelaunchThatMeetsALimitSkipsTheBatchsLimitedAccounts(t *testing.T) {
	r := newRig(t, "webhook")
	accounts(r, "acct@example.com", "other@example.com", "third@example.com")
	a := adopted(t, r, "T1")
	a.Reply = func(c *testkit.Claude, _ string) { c.Limit(r.w.Clock.Now().Add(45 * time.Hour)) }
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "build it"}, drive.SendOptions{}); err == nil {
		t.Fatal("the step met the limit")
	}
	r.w.OnOpen = func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) { c.Limit(r.w.Clock.Now().Add(45 * time.Hour)) }
	}
	_, err := r.d.Open("T9", drive.Step{Line: "build it"}, drive.OpenOptions{Dir: a.Cwd, Account: "other@example.com"})
	f, ok := errors.AsType[*drive.Failed](err)
	if !ok || !strings.Contains(f.Next, "--account third@example.com") || strings.Contains(f.Next, "--account acct@example.com") {
		t.Fatalf("the open's limit relaunches on the one account left: %v", err)
	}
}

// With no account that can take it, the open that brings a gone session back
// says why rather than naming the account that cannot.
func TestAGoneSessionWhoseAccountIsLimitedSaysWhyItCannotReopen(t *testing.T) {
	r := newRig(t, "webhook")
	a := adopted(t, r, "T1")
	resets := r.w.Clock.Now().Add(45 * time.Hour)
	a.Reply = func(c *testkit.Claude, _ string) { c.Limit(resets) }
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "build it"}, drive.SendOptions{}); err == nil {
		t.Fatal("the step met the limit")
	}
	if err := r.w.Close(a.Block); err != nil {
		t.Fatal(err)
	}
	rd, err := r.d.Read("T1", drive.ReadLast, 10)
	if err != nil || !strings.Contains(rd.Next, "takes no step until "+resets.Format("Mon 2 Jan 15:04")) || !strings.Contains(rd.Next, "tell the owner") {
		t.Fatalf("it says the account is out until its reset, and to tell the owner: %+v %v", rd, err)
	}
}

// The open it gives for once the limit lifts runs then: on the primary, whose
// launcher this drive does not know, it leaves the account to the launcher,
// as every open on the primary does.
func TestTheOpenForALimitedPrimaryRunsOnceItLifts(t *testing.T) {
	r := newRig(t, "webhook")
	_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude", "sessions"), 0o755)
	r.w.Primary, r.d.Cfg.PrimaryName = "owner", "owner"
	o, err := r.d.Open("T1", drive.Step{}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work"), Account: "primary"})
	if err != nil || o.Account != "primary" {
		t.Fatalf("opened on the primary: %+v %v", o, err)
	}
	r.w.Session(o.Block).Reply = func(c *testkit.Claude, _ string) { c.Limit(r.w.Clock.Now().Add(45 * time.Hour)) }
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "build it"}, drive.SendOptions{}); err == nil {
		t.Fatal("the step met the limit")
	}
	r.d.Cfg.PrimaryName = "" // read where the primary's launcher is not known
	if err := r.w.Close(o.Block); err != nil {
		t.Fatal(err)
	}
	rd, err := r.d.Read("T1", drive.ReadLast, 10)
	if err != nil || !strings.Contains(rd.Next, "tell the owner") || strings.Contains(rd.Next, "--account primary") {
		t.Fatalf("its open leaves the account to the launcher: %+v %v", rd, err)
	}
}
