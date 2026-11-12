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
	if !isRefused(err) || !strings.Contains(err.Error(), "a listener already runs") || strings.Contains(err.Error(), "drive --batch b1 listen") {
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
	if !isRefused(err) || !strings.Contains(err.Error(), "this Claude Code session drives no batch") || !strings.Contains(err.Error(), "start your own batch, which makes this session its driver: `drive batch new NAME`") ||
		!strings.Contains(err.Error(), "to read another's, name it: drive --batch bot1") {
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

// ring runs the machine's watchdog and says what it typed into b1's driver.
func ring(t *testing.T, r *rig) string {
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

// waitingWake leaves a wake for T1 that no listener takes.
func waitingWake(t *testing.T) (*rig, *testkit.Claude, *testkit.Claude) {
	t.Helper()
	r, drv := sessionRig(t)
	c := adopted(t, r, "T1")
	done(c, "start", "Built it.")
	if _, err := r.d.Notify(watchOf(c), "T1", time.Second); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.Sleep(3 * time.Minute)
	return r, drv, c
}

// A wake turn that died on an API error or an interrupt, as a Rex restart
// leaves one mid-turn, never started its listener: those are the drivers the
// doorbell is for. One a usage limit or a rejected login holds moves only once
// someone acts on it.
func TestTheDoorbellRingsADriverWhoseTurnDiedAndNotOneALimitHolds(t *testing.T) {
	for name, end := range map[string]func(*testkit.Claude){
		"an API error": func(c *testkit.Claude) { c.Busy("a wake"); c.Fail("overloaded_error", "Overloaded") },
		"an interrupt": func(c *testkit.Claude) { c.Busy("a wake"); c.Set("interrupted", "", "") },
	} {
		t.Run(name, func(t *testing.T) {
			r, drv, _ := waitingWake(t)
			end(drv)
			if got := ring(t, r); !strings.Contains(got, "[drive] Wakes wait") {
				t.Fatalf("the doorbell rings a driver whose turn died: %q", got)
			}
		})
	}
	t.Run("a usage limit", func(t *testing.T) {
		r, drv, _ := waitingWake(t)
		drv.Busy("a wake")
		drv.Limit(r.w.Clock.Now().Add(time.Hour))
		if got := ring(t, r); got != "" {
			t.Fatalf("a limit holds the driver until it resets: %q", got)
		}
	})
	t.Run("a screen with no input box", func(t *testing.T) {
		r, drv, _ := waitingWake(t)
		drv.Edit(func(c *testkit.Claude) { c.Screen = "a full-screen view" })
		if got := ring(t, r); got != "" {
			t.Fatalf("typing lands in whatever the screen shows: %q", got)
		}
	})
	t.Run("another conversation in the driver's block", func(t *testing.T) {
		r, drv, _ := waitingWake(t)
		if err := r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindDriver, SID: "an-older-conversation", Block: drv.Block}); err != nil {
			t.Fatal(err)
		}
		if got := ring(t, r); got != "" {
			t.Fatalf("the block runs other work now: %q", got)
		}
	})
	t.Run("again once the driver answered", func(t *testing.T) {
		r, _, _ := waitingWake(t)
		if ring(t, r) == "" {
			t.Fatal("first ring")
		}
		r.d.Seen() // the driver ran, and wakes still wait with no listener
		if got := ring(t, r); got == "" {
			t.Fatal("a driver that answered and stalled again is rung again, without the ten-minute wait")
		}
		evs, _ := r.b.Events()
		if log := drive.LogText(r.b, evs, r.w.Clock.Now()); !strings.Contains(log, "rang the driver in block") {
			t.Fatalf("the log says what a ring was: %s", log)
		}
	})
}

// The owner's page raises its alert only for wakes that waited past the
// doorbell's delay: between a wake and its listener is every healthy turn.
func TestThePageAlertsOnlyOnWakesThatWaitedPastTheDoorbell(t *testing.T) {
	r, drv := sessionRig(t)
	c := adopted(t, r, "T1")
	ov := func() string {
		t.Helper()
		o, err := r.d.Overview(true)
		if err != nil {
			t.Fatal(err)
		}
		return o.Unheard
	}
	if got := ov(); got != "" {
		t.Fatalf("a driver mid-turn with nothing waiting is no alert: %q", got)
	}
	done(c, "start", "Built it.")
	if _, err := r.d.Notify(watchOf(c), "T1", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := ov(); got != "" {
		t.Fatalf("a wake just left is no alert: %q", got)
	}
	r.w.Clock.Sleep(3 * time.Minute)
	if got := ov(); !strings.Contains(got, "no listener runs") {
		t.Fatalf("a wake that waited past the doorbell's delay is: %q", got)
	}
	drv.Busy("a long wake turn")
	if got := ov(); got != "" {
		t.Fatalf("a driver mid-turn starts its listener when the turn ends: %q", got)
	}
}

// A ring whose Enter was lost leaves its own line in the driver's box; the
// next run presses Enter on it rather than reading it as someone's typing.
func TestARingWhoseEnterWasLostIsPressedAgain(t *testing.T) {
	r, drv, _ := waitingWake(t)
	r.w.KeyFails = 1 // the line is typed; the Enter after it fails
	if _, err := r.d.Doorbell(r.b); err == nil {
		t.Fatal("a ring whose Enter failed says so")
	}
	failed := r.events(batch.KindTold)
	if len(failed) != 2 || failed[1].Reason == "" {
		t.Fatalf("the failure is on record: %+v", failed)
	}
	got, err := r.d.Doorbell(r.b)
	if err != nil || !strings.Contains(got, "pressed Enter") {
		t.Fatalf("the unsent ring is sent: %q %v", got, err)
	}
	if keys := strings.Join(drv.Keys, " "); !strings.HasSuffix(keys, "enter") {
		t.Fatalf("keys: %s", keys)
	}
}

// A listener started with nothing waiting is the driver's trace: it acted on
// what it took, or found nothing to do. One started while wakes wait has read
// none of them.
func TestAListenerStartedWithNothingWaitingIsHeard(t *testing.T) {
	r, _ := sessionRig(t)
	c := adopted(t, r, "T1")
	done(c, "start", "Built it.")
	if _, err := r.d.Notify(watchOf(c), "T1", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Listen(); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.Sleep(2 * time.Minute)
	before := len(r.events(batch.KindSeen))
	done := make(chan error, 1)
	go func() { _, err := r.d.Listen(); done <- err }()
	for range 2000 {
		if len(r.events(batch.KindSeen)) > before {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(r.events(batch.KindSeen)) == before {
		t.Fatal("the listener started with nothing waiting leaves the driver's trace")
	}
	if err := r.d.Ring("", "end the listener"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A session driving a batch under the name claude is never ended by
// another session starting a batch under it; one that stopped running is.
func TestABatchNewNeverEndsALiveSessionDriversBatch(t *testing.T) {
	r, drv := sessionRig(t)
	st, err := r.d.StartBatch(drive.BatchOptions{Name: "night", Wake: batch.WakeSession, Driver: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	other := r.w.Add("another-claude")
	second := *r.d
	second.Caller = drive.Caller{SID: other.SID, Block: other.Block}
	_, err = second.StartBatch(drive.BatchOptions{Name: "night-2", Wake: batch.WakeSession, Driver: "claude"})
	if !isRefused(err) || !strings.Contains(err.Error(), "driven by another Claude Code session") || !strings.Contains(err.Error(), "batch use night") ||
		!strings.Contains(err.Error(), "end it first (drive --batch night batch end)") {
		t.Fatalf("another session's batch new is refused while the driver runs: %v", err)
	}
	if live, _ := r.d.Store.Live(); !slicesHasBatch(live, st.Batch.Meta.Name) {
		t.Fatal("the driver's batch stays live")
	}
	drv.Crash()
	if _, err := second.StartBatch(drive.BatchOptions{Name: "night-3", Wake: batch.WakeSession, Driver: "claude"}); err != nil {
		t.Fatalf("a driver that stopped running drives nothing, so its batch is replaced: %v", err)
	}
}

func slicesHasBatch(bs []*batch.Batch, name string) bool {
	for _, b := range bs {
		if b.Meta.Name == name {
			return true
		}
	}
	return false
}

// A driver's tab reused for other work after its drive is no driver.
func TestADriversBlockRunningAnotherConversationTakesSteps(t *testing.T) {
	r, drv := sessionRig(t)
	if err := r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindDriver, SID: "an-older-conversation", Block: drv.Block}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Send(r.view(drv.Block[len(drv.Block)-6:]), drive.Step{Line: "go on"}, drive.SendOptions{Label: "D"}); err != nil && strings.Contains(err.Error(), "drives batch") {
		t.Fatalf("a block whose conversation is not the registered one is not a driver: %v", err)
	}
}

// Everything a session driver waits for reaches it through the listener: a
// timer, endings one post carried, and the asks open on a woken session.
func TestTheListenerTakesTimersCarriedEndingsAndNamesOpenAsks(t *testing.T) {
	r, _ := sessionRig(t)
	a, b := adopted(t, r, "A"), adopted(t, r, "B")
	if _, err := r.d.Ask("B", "Release before the read?", "wait", "it waits", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Timer(time.Minute, "veto"); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Ring("", "veto"); err != nil {
		t.Fatal(err)
	}
	done(a, "start", "Built A.")
	done(b, "start", "Built B.")
	if _, err := r.d.Notify(watchOf(a), "A", time.Second); err != nil {
		t.Fatal(err)
	}
	took, err := r.d.Listen()
	if err != nil {
		t.Fatal(err)
	}
	text := took.Text()
	for _, want := range []string{"- timer done: veto", "- A: done at A@", "- B: done at B@", "open asks, still the owner's: A1 (Release before the read?)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

// Within a session batch only the driver's own session is heard: a read
// from any other shell would hide a driver that is not answering.
func TestOnlyTheDriversOwnSessionIsHeardInASessionBatch(t *testing.T) {
	r, _ := sessionRig(t)
	r.w.Clock.Sleep(2 * time.Minute) // past the minute a fresh trace covers
	other := *r.d
	other.Caller = drive.Caller{}
	other.Seen()
	if len(r.events(batch.KindSeen)) != 0 {
		t.Fatal("a read from another shell is no driver's")
	}
	r.d.Seen()
	if len(r.events(batch.KindSeen)) != 1 {
		t.Fatal("the driver's own read is heard")
	}
}

// A listener started after its batch ended has nothing to wait for.
func TestAListenerEndsWithItsBatch(t *testing.T) {
	r, _ := sessionRig(t)
	if err := r.d.Store.End("b1", r.w.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	pinned := *r.d // a listener holds the batch it started in, as --batch does
	pinned.BatchName = "b1"
	took, err := pinned.Listen()
	if err != nil || !strings.Contains(took.Text(), "no longer live") {
		t.Fatalf("%+v %v", took, err)
	}
}

// A driver relaunched into the same conversation in another block is
// registered again by batch use, so the doorbell follows it.
func TestBatchUseRegistersADriverThatMovedBlocks(t *testing.T) {
	r, drv := sessionRig(t)
	moved := *r.d
	moved.Caller = drive.Caller{SID: drv.SID, Block: "block:somewhere-else"}
	if _, err := moved.UseBatch("b1"); err != nil {
		t.Fatal(err)
	}
	evs, _ := r.b.Events()
	if reg, _ := batch.Driving(evs); reg.Block != "block:somewhere-else" {
		t.Fatalf("%+v", reg)
	}
}
