package drive_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/rex"
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
	if err := r.d.Park(r.view("T2"), "waits for the owner's call on D2"); err != nil {
		t.Fatal(err)
	}
	r.w.Restart()

	e, err := r.d.Wait("T1", time.Minute, 10*time.Minute)
	if err != nil || e.State != "gone" || !strings.Contains(e.Next, "--resume "+busy.SID+" --account acct@example.com") {
		t.Fatalf("the gone wake names the open that brings it back: %+v %v", e, err)
	}
	st, err := r.d.Status(false)
	if err != nil || len(st.Rows) != 2 || st.Rows[1].Next == "" || !strings.Contains(st.Text(r.w.Clock.Now()), "T2 is gone: bring it back") {
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
	if w := st.Rows[1].Watch; w != "parked: waits for the owner's call on D2" || !st.Rows[0].Armed || st.Rows[1].Armed {
		t.Fatalf("the parked label gets no wake, the other one does: %+v", st.Rows)
	}
	if st.Rows[1].LastSent != "start" || st.Rows[1].SID != parked.SID {
		t.Fatalf("its own conversation carries on, with what was sent to it: %+v", st.Rows[1])
	}
	if e, err := r.d.Wait("T1", time.Minute, 10*time.Minute); err != nil || e.State != "interrupted" || strings.Contains(e.Reply, "No response requested") {
		t.Fatalf("a turn the restart cut short reads as interrupted: %+v %v", e, err)
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
