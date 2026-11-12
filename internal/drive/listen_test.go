package drive_test

import (
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// A batch a Claude Code session drives is told of its sessions as a chat
// driver is, by the same watches and pacing; only the last hop differs: the
// wake waits in the log until the driver's listener takes it.

// sessionRig is a batch a Claude Code session drives: the driver runs in a
// block of its own, and every command runs as that session.
func sessionRig(t *testing.T) (*rig, *testkit.Claude) {
	t.Helper()
	r := newRig(t, batch.WakeSession)
	drv := r.w.Add("driver")
	done(drv, "plan the batch", "plan posted")
	r.d.Caller = drive.Caller{SID: drv.SID, Block: drv.Block, Config: drv.Config}
	if err := r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindDriver, SID: drv.SID, Block: drv.Block}); err != nil {
		t.Fatal(err)
	}
	return r, drv
}

const listenWatch = "drive-b1-listen"

func TestASessionDriversWakeWaitsForItsListenerWhichTakingDoesNotHear(t *testing.T) {
	r, _ := sessionRig(t)
	c := adopted(t, r, "T1")
	if !r.watches.Alive(watchOf(c)) {
		t.Fatal("a send in a session batch arms the session's wake, as in a webhook batch")
	}
	done(c, "start", "Built it; ready for review.")
	if _, err := r.d.Notify(watchOf(c), "T1", time.Second); err != nil {
		t.Fatal(err)
	}
	wakes := r.events(batch.KindWake)
	if len(wakes) != 1 || !wakes[0].Inbox || wakes[0].HTTP != 0 || !wakes[0].Delivered() {
		t.Fatalf("the wake is left in the log for the listener, delivered and posted nowhere: %+v", wakes)
	}
	evs, _ := r.b.Events()
	if h := batch.Hearings(evs, r.w.Clock.Now()); h.Strikes != 0 {
		t.Fatalf("a wake left is no failed post: %+v", h)
	}

	took, err := r.d.Listen()
	if err != nil {
		t.Fatal(err)
	}
	text := took.Text()
	for _, want := range []string{"- T1: done at T1@", "next: read its reply first (drive --batch b1 read T1)",
		"next: act on each, then listen again, in the background: drive --batch b1 listen"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the listener prints each woken session as it stands, and itself again last; missing %q in:\n%s", want, text)
		}
	}
	if r.watches.Alive(listenWatch) {
		t.Fatal("the listener is off the books once it returns")
	}
	evs, _ = r.b.Events()
	if len(batch.Untaken(evs)) != 0 || len(r.events(batch.KindTaken)) != 1 {
		t.Fatal("what it took is on record as taken")
	}
	// Taking is not hearing: a driver whose turn never ran after the take
	// is a driver not answering.
	r.w.Clock.Sleep(batch.HeardWithin + time.Minute)
	evs, _ = r.b.Events()
	if h := batch.Hearings(evs, r.w.Clock.Now()); h.Strikes != 1 {
		t.Fatalf("a wake taken and followed by no command of the driver's is unheard: %+v", h)
	}
	r.d.Seen()
	evs, _ = r.b.Events()
	if h := batch.Hearings(evs, r.w.Clock.Now()); h.Strikes != 0 {
		t.Fatalf("a command of the driver's own session is heard: %+v", h)
	}
}

func TestOnlyTheDriverListensAndOneListenerAtATime(t *testing.T) {
	r, _ := sessionRig(t)
	other := r.d.Caller
	other.SID = "some-other-session"
	d := *r.d
	d.Caller = other
	if _, err := d.Listen(); !isRefused(err) || !strings.Contains(err.Error(), "only batch b1's driver listens") || !strings.Contains(err.Error(), "batch use b1") {
		t.Fatalf("another session's listener is refused, saying how to take the batch over: %v", err)
	}
	if ok, _ := r.watches.Claim(listenWatch); !ok {
		t.Fatal("claim")
	}
	_, err := r.d.Listen()
	if !isRefused(err) || !strings.Contains(err.Error(), "a listener already runs") || strings.Contains(err.Error(), "listen\n") {
		t.Fatalf("a second listener is refused, with no next: that starts it again: %v", err)
	}
}

func TestADriverSessionsCommandsActInItsBatchAndAnotherClaudeSessionIsToldToStartItsOwn(t *testing.T) {
	r, drv := sessionRig(t)
	bot, _, err := r.d.Store.New(batch.Meta{Name: "bot1", Started: r.w.Clock.Now(), Wake: batch.WakeWebhook, Webhook: "session-driver", Driver: "session-driver"})
	if err != nil {
		t.Fatal(err)
	}
	stranger, reader := *r.d, *r.d // copied before a command binds its batch
	if st, err := r.d.Status(false); err != nil || st.Batch == nil || st.Batch.Name != "b1" {
		t.Fatalf("the driver's command names no driver and acts in its own batch: %+v %v", st, err)
	}
	stranger.Caller = drive.Caller{SID: "a-session-driving-nothing", Block: "block:elsewhere"}
	_, err = stranger.Status(false)
	if !isRefused(err) || !strings.Contains(err.Error(), "this Claude Code session drives no batch") || !strings.Contains(err.Error(), "next: start your own, which makes this session its driver: `drive batch new NAME`") {
		t.Fatalf("a Claude Code session driving nothing is told to start its own batch, not to run as the bot: %v", err)
	}
	// A read by a Claude Code session in the bot's batch is no run of the bot's.
	reader.BatchName = "bot1"
	reader.Seen()
	evs, _ := bot.Events()
	for _, e := range evs {
		if e.Kind == batch.KindSeen {
			t.Fatal("a Claude Code session's read of a chat driver's batch reads as that driver hearing its wake")
		}
	}
	reader.Caller = drive.Caller{}
	reader.Seen()
	if evs, _ = bot.Events(); evs[len(evs)-1].Kind != batch.KindSeen {
		t.Fatal("the chat driver's own read is heard")
	}
	// drive types nothing into a driver.
	_, err = r.d.Send(r.view(drv.Block[len(drv.Block)-6:]), drive.Step{Line: "go on"}, drive.SendOptions{Label: "D"})
	if !isRefused(err) || !strings.Contains(err.Error(), "drives batch b1") {
		t.Fatalf("a send into the driver's own block is refused: %v", err)
	}
}

func TestABatchStartedFromAClaudeCodeSessionInRexIsThatSessions(t *testing.T) {
	r, drv := sessionRig(t)
	st, err := r.d.StartBatch(drive.BatchOptions{Name: "b2", Wake: batch.WakeSession, Driver: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := st.Batch.Events()
	if reg, ok := batch.Driving(evs); !ok || reg.SID != drv.SID || reg.Block != drv.Block {
		t.Fatalf("the session that started it is on record as its driver: %+v", reg)
	}
	if !strings.Contains(st.Text(), "next: keep one listener running, in the background: drive --batch b2 listen") {
		t.Fatalf("its start says to keep a listener running:\n%s", st.Text())
	}
	nowhere := *r.d
	nowhere.Caller = drive.Caller{SID: drv.SID}
	if _, err := nowhere.StartBatch(drive.BatchOptions{Name: "b3", Wake: batch.WakeSession, Driver: "claude"}); !isRefused(err) {
		t.Fatalf("a session batch starts only from a session in Rex: %v", err)
	}
	// Another session making it current takes it over.
	next := *r.d
	next.Caller = drive.Caller{SID: "relaunched", Block: drv.Block}
	if _, err := next.UseBatch("b2"); err != nil {
		t.Fatal(err)
	}
	evs, _ = st.Batch.Events()
	if reg, _ := batch.Driving(evs); reg.SID != "relaunched" {
		t.Fatalf("batch use from another session makes it the driver: %+v", reg)
	}
}

// When wakes wait and no listener runs, as after a Rex restart ended it, the
// watchdog types one line into the driver: never into a busy driver or over
// what someone typed, and not again until the driver ran or ten minutes pass.
func TestTheWatchdogRingsADriverWhoseWakesWaitWithNoListener(t *testing.T) {
	ring := func(r *rig) string {
		t.Helper()
		reports, err := r.d.DueAll()
		if err != nil {
			t.Fatal(err)
		}
		for _, rep := range reports {
			if rep.Batch == "b1" {
				if rep.Err != nil {
					t.Fatal(rep.Err)
				}
				return rep.Rang
			}
		}
		return ""
	}
	waiting := func(t *testing.T) (*rig, *testkit.Claude) {
		r, drv := sessionRig(t)
		c := adopted(t, r, "T1")
		done(c, "start", "Built it.")
		if _, err := r.d.Notify(watchOf(c), "T1", time.Second); err != nil {
			t.Fatal(err)
		}
		return r, drv
	}
	t.Run("rings once wakes have waited", func(t *testing.T) {
		r, drv := waiting(t)
		if got := ring(r); got != "" {
			t.Fatalf("a wake just left gives the driver's turn time to start its listener: %q", got)
		}
		r.w.Clock.Sleep(3 * time.Minute)
		got := ring(r)
		if !strings.Contains(got, "[drive] Wakes wait for you in batch b1 and no listener runs") || !strings.Contains(got, "drive --batch b1 listen") {
			t.Fatalf("the doorbell names the listener to start: %q", got)
		}
		if typed := strings.Join(drv.Typed, "\n"); !strings.Contains(typed, "[drive] Wakes wait") {
			t.Fatalf("the line reached the driver's own block: %q", typed)
		}
		if ring(r) != "" {
			t.Fatal("it rings once, not on every watchdog run")
		}
		if st, _ := r.d.Status(false); !strings.Contains(st.Unheard, "no listener runs, and 1 wakes have waited for the driver") {
			t.Fatalf("status says no listener runs while wakes wait: %q", st.Unheard)
		}
	})
	t.Run("never over a busy driver or a typed line", func(t *testing.T) {
		r, drv := waiting(t)
		r.w.Clock.Sleep(3 * time.Minute)
		drv.Edit(func(c *testkit.Claude) { c.Input = "half a message to the driver" })
		if got := ring(r); got != "" {
			t.Fatalf("the owner may be typing to the driver: %q", got)
		}
		drv.Edit(func(c *testkit.Claude) { c.Input = "" })
		drv.Busy("working on a wake")
		if got := ring(r); got != "" {
			t.Fatalf("a driver mid-turn starts its listener when the turn ends: %q", got)
		}
	})
	t.Run("never while a listener runs", func(t *testing.T) {
		r, _ := waiting(t)
		r.w.Clock.Sleep(3 * time.Minute)
		_, _ = r.watches.Claim(listenWatch)
		if got := ring(r); got != "" {
			t.Fatalf("a listener runs: %q", got)
		}
	})
}

// A wake names the asks still open on its session, whatever its reply says:
// a driver deciding from the reply alone merged a change the owner had not
// answered for.
func TestAWakeNamesTheOpenAsksOnItsSession(t *testing.T) {
	r := newRig(t, batch.WakeWebhook)
	l := hook(t, r)
	c := adopted(t, r, "T1")
	if _, err := r.d.Ask("T1", "Release before the read?", "wait for the read", "it waits for the read", ""); err != nil {
		t.Fatal(err)
	}
	done(c, "merge-when-ready", "All checks pass; ready to merge.")
	if _, err := r.d.Notify(watchOf(c), "T1", time.Second); err != nil {
		t.Fatal(err)
	}
	if out := l.output(0); !strings.Contains(out, "open asks, still the owner's: A1 (Release before the read?)") {
		t.Fatalf("the wake names the open ask:\n%s", out)
	}
}
