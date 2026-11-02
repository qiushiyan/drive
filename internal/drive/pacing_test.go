package drive_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/testkit"
)

// Each post starts a model run, which is what a driver costs. These hold the
// decisions that pace them, and that a real ending is never among what they
// hold back.

// webhookLog is a driver's webhook that records what it is told.
type webhookLog struct {
	mu     sync.Mutex
	posts  []map[string]any
	status int
	onPost func()
}

func hook(t *testing.T, r *rig) *webhookLog {
	t.Helper()
	l := &webhookLog{status: 204}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var p map[string]any
		body, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(body, &p)
		l.mu.Lock()
		l.posts = append(l.posts, p)
		status, on := l.status, l.onPost
		l.mu.Unlock()
		if on != nil {
			on()
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	return l
}

func (l *webhookLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.posts)
}

func (l *webhookLog) output(i int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprint(l.posts[i]["output"])
}

// A turn ended while a job it started runs. What the reply says decides how
// long the wait lasts, and a question never waits on a job.
func TestWhatAReplySaysDecidesTheWaitWhileItsJobRuns(t *testing.T) {
	report := strings.Repeat("What the round found and how it was settled. ", 40)
	for _, tc := range []struct {
		name, reply string
		atLeast     time.Duration
		atMost      time.Duration
	}{
		{"ended on a question", "Run 5 passes three of five readings. Should it count as a pass?", time.Minute, 2 * time.Minute},
		{"a decision that needs someone", report + "\n\nOn your word I'll end the loop here.", time.Minute, 2 * time.Minute},
		{"asks, and waits as well", "Is the heads-up enough? Waiting on CI meanwhile.", time.Minute, 2 * time.Minute},
		{"a report with a job left behind", report + "\n\nThe scratch worktrees are removed.", 10 * time.Minute, 11 * time.Minute},
		{"anything unclear", "415 copies, no new failures.", 10 * time.Minute, 11 * time.Minute},
		{"says it waits", "Waiting on the replays.", 2 * time.Hour, 2*time.Hour + time.Minute},
		{"says what comes after", report + "\n\nWhen the replays finish, I'll score them and report.", 2 * time.Hour, 2*time.Hour + time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "webhook")
			c := adopted(t, r, "T1")
			c.Busy("run the gate")
			c.Job("gate-replay --arms 5")
			c.Done(tc.reply)
			start := r.w.Clock.Now()
			e, err := r.d.Wait("T1", time.Minute, 10*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if took := r.w.Clock.Now().Sub(start); took < tc.atLeast || took > tc.atMost {
				t.Fatalf("the wait ended after %s, not between %s and %s", took, tc.atLeast, tc.atMost)
			}
			if len(e.Jobs) != 1 || e.State != "done" || !strings.Contains(e.Text(), "gate-replay") {
				t.Fatalf("it says what still runs: %+v\n%s", e, e.Text())
			}
		})
	}
}

// What needs someone wakes at once, whatever still runs and whatever the
// reply said before it stopped.
func TestAJobHoldsBackNoPromptErrorOrInterrupt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		end   func(c *testkit.Claude)
		state drive.State
	}{
		{"a permission prompt", func(c *testkit.Claude) { c.Ask("rm -rf /tmp/x") }, "blocked:permission"},
		{"a question prompt", func(c *testkit.Claude) { c.Set("blocked", "question", "Which arm?") }, "blocked:question"},
		{"an API error", func(c *testkit.Claude) { c.Fail("overloaded", "Overloaded") }, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "webhook")
			c := adopted(t, r, "T1")
			c.Busy("run the gate")
			c.Say("Waiting on the replays.")
			c.Job("gate-replay")
			tc.end(c)
			start := r.w.Clock.Now()
			e, err := r.d.Wait("T1", time.Minute, 10*time.Minute)
			if err != nil || e.State != tc.state {
				t.Fatalf("%+v %v", e, err)
			}
			if took := r.w.Clock.Now().Sub(start); took > 2*time.Minute {
				t.Fatalf("it waited %s on a job", took)
			}
		})
	}
}

// A recap goes before a park or a close, so a driver waits to read it,
// whatever it says and whatever still runs.
func TestARecapWakesWhateverStillRuns(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	c.Job("gh pr checks --watch")
	c.Edit(func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) { c.Done("Merged. The wait on the release is still running.") }
	})
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "recap"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	start := r.w.Clock.Now()
	if _, err := r.d.Wait("T1", time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if took := r.w.Clock.Now().Sub(start); took > 2*time.Minute {
		t.Fatalf("a recap waited %s on a job", took)
	}
}

// answeredBy has each post start a driver's run, which reads the batch half a
// minute on, as real runs did, and ends.
func (l *webhookLog) answeredBy(r *rig) *webhookLog {
	l.onPost = func() {
		run := *r.d
		at := r.w.Clock.Now().Add(30 * time.Second)
		go func() {
			waitForClock(r, at)
			run.Seen()
		}()
	}
	return l
}

// waitForClock returns once the fake clock has passed after: the test's own
// goroutine moving a session while a watch polls.
func waitForClock(r *rig, after time.Time) {
	for r.w.Clock.Now().Before(after) {
		time.Sleep(20 * time.Microsecond)
	}
}

// A five-hour job costs what it should: the one wake at its real end, and one
// backstop along the way, where a wake every ten minutes cost thirty. The
// watch stays armed throughout, so no run has to arm it again.
func TestAFiveHourJobCostsOneWakeAndABackstop(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	name := watchOf(c)
	c.Busy("run the gate")
	c.Job("gate-replay")
	c.Done("Replays are running. When they finish, I'll score them and report.")
	woke := make(chan *drive.Ending, 1)
	notify := *r.d
	go func() {
		e, _ := notify.Notify(name, "T1", time.Minute, 10*time.Minute)
		woke <- e
	}()
	start := r.w.Clock.Now()
	waitForClock(r, start.Add(5*time.Hour))
	if !r.watches.Alive(name) {
		t.Fatal("the watch stays armed while the job runs")
	}
	if n := l.count(); n != 1 {
		t.Fatalf("five hours of waiting cost %d wakes, not the one backstop at two hours", n)
	}
	if out := l.output(0); !strings.Contains(out, "gate-replay") || !strings.Contains(out, "drive --batch b1 watch T1") {
		t.Fatalf("the backstop says what runs, and the one command that answers it: %s", out)
	}
	c.JobDone("All five readings are scored. Should run 5 count as a pass?")
	e := <-woke
	if e == nil || e.State != "done" || !strings.Contains(e.Reply, "Should run 5 count") {
		t.Fatalf("the real ending wakes the driver: %+v", e)
	}
	if n := l.count(); n != 2 {
		t.Fatalf("the job's end is the second and last wake, not %d", n)
	}
	if r.watches.Alive(name) {
		t.Fatal("that wake was a real ending, so its watch has retired")
	}
}

// A report with a job left behind wakes once, and however often its watch is
// armed again, not again for that view until the backstop.
func TestAReplyIsToldOncePerView(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	name := watchOf(c)
	c.Busy("land it")
	c.Job("tail -f server.log")
	c.Done("PR #41 is open and its description is written.")
	stopped := make(chan struct{})
	notify := *r.d
	go func() {
		defer close(stopped)
		_, _ = notify.Notify(name, "T1", time.Minute, 10*time.Minute)
	}()
	start := r.w.Clock.Now()
	waitForClock(r, start.Add(90*time.Minute))
	if n := l.count(); n != 1 {
		t.Fatalf("one reply cost %d wakes in 90 minutes", n)
	}
	waitForClock(r, start.Add(5*time.Hour))
	if n := l.count(); n != 2 {
		t.Fatalf("a job past its purpose reaches the driver again some hours on: %d wakes", n)
	}
	waitForClock(r, start.Add(11*time.Hour))
	if n := l.count(); n != 2 {
		t.Fatalf("and later marks are further apart: %d wakes", n)
	}
	c.JobDone("Stopped.")
	<-stopped
}

// A job that ends with the session still idle, which no notification resumed,
// is news the driver has no other way to hear, though the view is the same.
func TestAJobThatEndsWithoutResumingStillWakes(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	c.Busy("land it")
	c.Job("gate-replay")
	c.Done("Waiting on the replays.")
	done := make(chan struct{})
	notify := *r.d
	go func() {
		defer close(done)
		_, _ = notify.Notify(watchOf(c), "T1", time.Minute, 10*time.Minute)
	}()
	waitForClock(r, r.w.Clock.Now().Add(3*time.Hour))
	if n := l.count(); n != 1 {
		t.Fatalf("the backstop: %d wakes", n)
	}
	c.Edit(func(c *testkit.Claude) { c.Shells = nil })
	<-done
	if n := l.count(); n != 2 {
		t.Fatalf("the job's end is told too: %d wakes", n)
	}
}

// Endings close together make one post, naming them all, and each session's
// own watch then has nothing to add. The next ending after it waits for the
// run that post started.
func TestEndingsCloseTogetherShareOnePost(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	a, b, c := adopted(t, r, "A"), adopted(t, r, "B"), adopted(t, r, "C")
	for _, s := range []*testkit.Claude{a, b, c} {
		s.Busy("work")
		s.Done("the round is finished")
	}
	r.w.Clock.Sleep(2 * time.Minute)
	// A's watch reaches its post first; B's and C's are running.
	first := *r.d
	if _, err := first.Notify(watchOf(a), "A", time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := l.count(); n != 1 {
		t.Fatalf("one post: %d", n)
	}
	out := l.output(0)
	for _, want := range []string{"also ended, in this one wake: B", "also ended, in this one wake: C", "view: B@", "view: C@"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the post names %q:\n%s", want, out)
		}
	}
	for _, s := range []struct {
		label string
		c     *testkit.Claude
	}{{"B", b}, {"C", c}} {
		other := *r.d
		e, err := other.Notify(watchOf(s.c), s.label, time.Minute, 10*time.Minute)
		if err != nil || !e.Covered {
			t.Fatalf("%s's ending was carried: %+v %v", s.label, e, err)
		}
		if r.watches.Alive(watchOf(s.c)) {
			t.Fatalf("%s's watch has nothing left to do", s.label)
		}
	}
	if n := l.count(); n != 1 {
		t.Fatalf("still one post: %d", n)
	}
	st, _ := r.d.Status(false)
	for _, row := range st.Rows {
		if !row.Unhandled {
			t.Fatalf("each reads as woken, so nothing is lost from status: %+v", row)
		}
	}
	if m, _ := r.d.Metrics(); m.Wakes != 1 || m.Carried != 2 {
		t.Fatalf("metrics tell posts from endings carried: %+v", m)
	}
}

// A wake close behind another waits for its run rather than joining it, and
// comes once that run is over: it is deferred, never dropped.
func TestAWakeCloseBehindAnotherWaitsForItsRun(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	a, b := adopted(t, r, "A"), adopted(t, r, "B")
	a.Busy("work")
	a.Done("finished")
	r.w.Clock.Sleep(2 * time.Minute)
	first := *r.d
	if _, err := first.Notify(watchOf(a), "A", time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	start := r.w.Clock.Now()
	b.Busy("work")
	b.Done("finished too")
	second := *r.d
	if _, err := second.Notify(watchOf(b), "B", time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if took := r.w.Clock.Now().Sub(start); took < 3*time.Minute || took > 5*time.Minute {
		t.Fatalf("it waited for the gap, not %s", took)
	}
	if n := l.count(); n != 2 {
		t.Fatalf("both endings were posted: %d", n)
	}
}

// A run that acts on a session meanwhile leaves nothing to post about it.
func TestASessionActedOnMeanwhilePostsNothing(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	a, b := adopted(t, r, "A"), adopted(t, r, "B")
	a.Busy("work")
	a.Done("finished")
	r.w.Clock.Sleep(2 * time.Minute)
	first := *r.d
	if _, err := first.Notify(watchOf(a), "A", time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	// B ended moments after A's post, so its wake is held for that run.
	b.Busy("work")
	b.Done("finished too")
	b.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	done := make(chan struct{})
	second := *r.d
	go func() {
		defer close(done)
		_, _ = second.Notify(watchOf(b), "B", time.Minute, 10*time.Minute)
	}()
	waitForClock(r, r.w.Clock.Now().Add(90*time.Second))
	acting := *r.d
	if _, err := acting.Send(r.view("B"), drive.Step{Line: "next step"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForClock(r, r.w.Clock.Now().Add(20*time.Minute))
	if n := l.count(); n != 1 {
		t.Fatalf("B moved on, so nothing is posted for its old turn: %d posts", n)
	}
	b.Done("next step finished")
	<-done
	if n := l.count(); n != 2 {
		t.Fatalf("its new turn's end wakes the driver: %d posts", n)
	}
}

// A webhook that answers 2xx while no run starts, as when the bot's usage is
// out, is a wake lost as surely as a post that failed. Three in a row and the
// batch backs off; a driver's next command clears it, and what waited goes.
func TestWakesNobodyAnswersBackOffAndNoneIsLost(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	sessions := []*testkit.Claude{adopted(t, r, "A"), adopted(t, r, "B"), adopted(t, r, "C"), adopted(t, r, "D")}
	for _, s := range sessions {
		s.Busy("work")
	}
	end := func(i int) {
		t.Helper()
		sessions[i].Done("finished")
		r.w.Clock.Sleep(2 * time.Minute)
		first := *r.d
		if _, err := first.Notify(watchOf(sessions[i]), string(rune('A'+i)), time.Minute, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		r.w.Clock.Sleep(11 * time.Minute) // nobody answers within the time a run takes to start
	}
	end(0)
	end(1)
	end(2)
	if n := l.count(); n != 3 {
		t.Fatalf("three wakes went out: %d", n)
	}
	st, _ := r.d.Status(false)
	if !strings.Contains(st.Unheard, "has not answered its last 3 wakes") || !strings.Contains(st.Unheard, "none is lost") {
		t.Fatalf("status says so: %q", st.Unheard)
	}
	// D's turn ends: its wake waits, and nothing about it is lost.
	sessions[3].Done("finished, and a question for you?")
	done := make(chan struct{})
	second := *r.d
	go func() {
		defer close(done)
		_, _ = second.Notify(watchOf(sessions[3]), "D", time.Minute, 10*time.Minute)
	}()
	waitForClock(r, r.w.Clock.Now().Add(3*time.Minute))
	if n := l.count(); n != 3 {
		t.Fatalf("no post while the driver is not answering: %d", n)
	}
	if st, _ := r.d.Status(false); !strings.Contains(st.Rows[3].Watch, "waits until the driver answers") || st.Rows[3].Due != "" {
		t.Fatalf("its row says what it waits for, and it is not stuck: %+v", st.Rows[3].Wake)
	}
	// The usage comes back and the owner's own message starts a run.
	run := *r.d
	run.Seen()
	<-done
	if n := l.count(); n != 4 || !strings.Contains(l.output(3), "a question for you?") {
		t.Fatalf("what waited is posted once a driver answers: %d", n)
	}
	if st, _ := r.d.Status(false); st.Unheard != "" {
		t.Fatalf("and the batch reads as heard again: %q", st.Unheard)
	}
}

// While nobody answers, tries come further and further apart.
func TestUnansweredWakesGetFurtherApart(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var evs []batch.Event
	h := func() batch.Hearing { return batch.Hearings(evs, now) }
	for range 3 {
		evs = append(evs, batch.Event{At: now, Kind: batch.KindWake, Session: "A", HTTP: 204})
		now = now.Add(11 * time.Minute)
	}
	if h().Strikes != 3 {
		t.Fatalf("%+v", h())
	}
	// A wake posted a moment ago has not had the time to be answered.
	evs = append(evs, batch.Event{At: now, Kind: batch.KindWake, Session: "A", HTTP: 204})
	if h().Strikes != 3 {
		t.Fatalf("a wake just posted is not unheard yet: %+v", h())
	}
	if !h().Last.Equal(now) {
		t.Fatalf("a back-off runs from the newest post, counted or not: %+v", h())
	}
	// A failed post counts at once.
	evs = append(evs, batch.Event{At: now, Kind: batch.KindWake, Session: "A", Reason: "http 429 no usage"})
	if h().Strikes != 4 {
		t.Fatalf("%+v", h())
	}
	// A wake armed by the watchdog proves no driver ran; a read does.
	evs = append(evs, batch.Event{At: now, Kind: batch.KindWatch, Session: "A"})
	if h().Strikes != 4 {
		t.Fatalf("%+v", h())
	}
	evs = append(evs, batch.Event{At: now, Kind: batch.KindSeen})
	if h().Strikes != 0 {
		t.Fatalf("%+v", h())
	}
}

// A wake nobody heard is no wake: the one-repost rule counts only what a run
// answered, so a question that came during an outage is still posted after it.
func TestAWakeNobodyHeardIsDueAgainWhateverItsCount(t *testing.T) {
	r := newRig(t, "webhook")
	hook(t, r)
	c := adopted(t, r, "T1")
	view := r.view("T1")
	woken(r, c)
	for range 2 {
		_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWake, Session: "T1", HTTP: 204, View: view})
		r.w.Clock.Sleep(11 * time.Minute)
	}
	armed, _, err := r.d.Due()
	if err != nil || len(armed) != 1 || !strings.Contains(armed[0].Why, "no driver has run since") {
		t.Fatalf("still due: %+v %v", armed, err)
	}
}

// A timer posted while nobody answers is a timer lost, so it waits too.
func TestATimerWaitsWhileNobodyAnswers(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	now := r.w.Clock.Now()
	for i := range 3 {
		_ = r.b.Append(batch.Event{At: now.Add(time.Duration(i) * time.Second), Kind: batch.KindWake, Session: "A", HTTP: 204})
	}
	r.w.Clock.Sleep(11 * time.Minute)
	done := make(chan struct{})
	ring := *r.d
	go func() {
		defer close(done)
		_ = ring.Ring("", "veto")
	}()
	<-done
	if took := r.w.Clock.Now().Sub(now); took < 15*time.Minute {
		t.Fatalf("it rang after %s, inside the back-off", took)
	}
	if n := l.count(); n != 1 {
		t.Fatalf("it still rings: %d", n)
	}
}

// The machine's watchdog runs no model unless something needs one.
func TestTheWatchdogPostsOnlyWhatNeedsADriver(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	work := filepath.Join(r.w.Root, "work")
	s := r.w.Add("S1")
	done(s, "onboard", "analysis")
	if _, err := r.d.Send(r.view(s.Block[len(s.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "S1"}); err != nil {
		t.Fatal(err)
	}
	_ = work
	reports, err := r.d.DueAll()
	if err != nil || len(reports) != 1 || reports[0].Posted != "" || l.count() != 0 {
		t.Fatalf("a watch runs, so nothing is due and nothing posts: %+v %v", reports, err)
	}
	if out := drive.DueText(reports); !strings.Contains(out, "batch b1: nothing due") {
		t.Fatalf("%q", out)
	}
	// A watch that died is armed again, with no post: its own wake carries the rest.
	r.watches.Stop(watchOf(s))
	reports, _ = r.d.DueAll()
	if len(reports[0].Armed) != 1 || l.count() != 0 || !r.watches.Alive(watchOf(s)) {
		t.Fatalf("armed with no model run: %+v, %d posts", reports[0], l.count())
	}
	// Every session parked: still nothing.
	if err := r.d.Park(r.view("S1"), "waits on the owner"); err != nil {
		t.Fatal(err)
	}
	if reports, _ = r.d.DueAll(); len(reports[0].Armed) != 0 || l.count() != 0 {
		t.Fatalf("a parked batch costs nothing: %+v", reports[0])
	}
	// An open ask gets rechecked, every six hours at most, since no session wakes for it.
	if _, err := r.d.Ask("S1", "Take the lab host down?", "yes", "it keeps running"); err != nil {
		t.Fatal(err)
	}
	if reports, _ = r.d.DueAll(); l.count() != 0 {
		t.Fatalf("an ask just asked needs no recheck: %+v", reports[0])
	}
	r.w.Clock.Sleep(7 * time.Hour)
	if reports, _ = r.d.DueAll(); l.count() != 1 || !strings.Contains(l.output(0), "A1") || !strings.Contains(reports[0].Posted, "recheck") {
		t.Fatalf("recheck: %+v, %d posts", reports[0], l.count())
	}
	if l.posts[0]["kind"] != "watchdog" {
		t.Fatalf("its post says what it is: %v", l.posts[0])
	}
	r.w.Clock.Sleep(time.Hour)
	if _, _ = r.d.DueAll(); l.count() != 1 {
		t.Fatalf("not again so soon: %d posts", l.count())
	}
	// A session that went away is told once.
	r.d.Seen()
	r.w.Restart()
	r.w.Clock.Sleep(7 * time.Hour)
	reports, _ = r.d.DueAll()
	if l.count() != 2 || !strings.Contains(l.output(1), "S1 is gone:") || !strings.Contains(l.output(1), "drive --batch b1 open S1") {
		t.Fatalf("gone: %+v %d posts", reports[0], l.count())
	}
	r.w.Clock.Sleep(time.Minute)
	if reports, _ := r.d.DueAll(); strings.Contains(reports[0].Posted, "gone") {
		t.Fatalf("told once: %+v", reports[0])
	}
}

// One command answers an ask and sends what follows: no read for the view.
func TestAnAnswerCanCarryItsStep(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	if err := r.d.Park(r.view("T1"), "waits on A1"); err != nil {
		t.Fatal(err)
	}
	id, err := r.d.Ask("T1", "Count run 5 as a pass?", "yes", "T1 stays parked")
	if err != nil {
		t.Fatal(err)
	}
	sent, got, err := r.d.AnswerAndSend(id, "yes (chat)", "recommended", false, drive.Step{Line: "Count it, and run the pairs."}, drive.SendOptions{})
	if err != nil || got != id || sent == nil || sent.Outcome != "arrived" {
		t.Fatalf("%+v %q %v", sent, got, err)
	}
	if typed, _ := c.Seen(); !strings.Contains(strings.Join(typed, "|"), "Count it, and run the pairs.") {
		t.Fatalf("the step went: %q", typed)
	}
	evs, _ := r.b.Events()
	_, asks, _ := batch.Records(evs)
	if asks[0].Answer == nil || asks[0].Answer.Took != "recommended" {
		t.Fatalf("the answer is on record: %+v", asks[0])
	}
	if !r.watches.Alive(watchOf(c)) {
		t.Fatal("the step armed its wake")
	}
	// Twice, as two runs would: the second touches nothing.
	if _, _, err := r.d.AnswerAndSend(id, "yes", "recommended", false, drive.Step{Line: "again"}, drive.SendOptions{}); !isRefused(err) {
		t.Fatalf("already answered: %v", err)
	}
}

// A refused step leaves the ask open, so the one command can run again.
func TestARefusedStepRecordsNoAnswer(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	id, _ := r.d.Ask("T1", "Go on?", "yes", "it waits")
	c.Busy("still at work")
	_, got, err := r.d.AnswerAndSend(id, "yes", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || got != "" {
		t.Fatalf("%q %v", got, err)
	}
	evs, _ := r.b.Events()
	if _, asks, _ := batch.Records(evs); asks[0].Answer != nil {
		t.Fatalf("no answer recorded: %+v", asks[0])
	}
	batchAsk, _ := r.d.Ask("", "Release tonight?", "no", "it waits")
	if _, _, err := r.d.AnswerAndSend(batchAsk, "no", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "names no session") {
		t.Fatalf("an ask about the batch has no session to send to: %v", err)
	}
}

// cleanupWorld answers as gwt does: which branches are merged, and removing them.
type cleanupWorld struct {
	prs
	merged  map[string]bool
	refuse  map[string]string
	removed []string
}

func (w *cleanupWorld) Landed(dir string, branches []string, commits []progress.Commit) (progress.Landing, error) {
	l := progress.Landing{Repo: "/work/tool", Trunk: "main", Branches: branches, Merged: map[string]bool{}}
	for _, b := range branches {
		if m, ok := w.merged[b]; ok {
			l.Merged[b] = m
		}
	}
	return l, nil
}

func (w *cleanupWorld) Remove(dir, branch string) error {
	if why, ok := w.refuse[branch]; ok {
		return errors.New(why)
	}
	w.removed = append(w.removed, branch)
	delete(w.merged, branch)
	return nil
}

func closedOn(t *testing.T, r *rig, label string, branches ...string) *testkit.Claude {
	t.Helper()
	c := adopted(t, r, label)
	for _, b := range branches {
		c.Write(map[string]any{"type": "attachment", "gitBranch": b})
	}
	if err := r.d.Close(r.view(label)); err != nil {
		t.Fatal(err)
	}
	return c
}

// What a closed session left is resolved once, and what stays reads as a fact
// in status, naming no one to ask.
func TestCleanupResolvesWhatClosedSessionsLeft(t *testing.T) {
	r := newRig(t, "webhook")
	w := &cleanupWorld{prs: prs{}, merged: map[string]bool{"done-a": true, "open-b": false, "dirty-c": true, "in-use": true}, refuse: map[string]string{"dirty-c": "dirty-c has uncommitted work"}}
	r.d.Progress = w
	closedOn(t, r, "A", "main", "done-a")
	closedOn(t, r, "B", "open-b")
	closedOn(t, r, "C", "dirty-c")
	closedOn(t, r, "D", "in-use")
	closedOn(t, r, "E")
	still := adopted(t, r, "S")
	still.Write(map[string]any{"type": "attachment", "gitBranch": "in-use"})

	st, err := r.d.Status(true)
	if err != nil || len(st.Leftovers) != 1 || !strings.Contains(st.Leftovers[0], "5 closed sessions not yet checked") || !strings.Contains(st.Leftovers[0], "drive --batch b1 cleanup") {
		t.Fatalf("status names what nobody has checked, and the one command: %q %v", st.Leftovers, err)
	}
	cleaned, err := r.d.Cleanup("")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.removed) != 1 || w.removed[0] != "done-a" {
		t.Fatalf("only the merged branch nothing uses goes: %q", w.removed)
	}
	text := drive.CleanedText(cleaned)
	for _, want := range []string{"removed branch done-a", "branch open-b is not merged, so it stays", "branch dirty-c stays: dirty-c has uncommitted work", "branch in-use stays: S still works on it"} {
		if !strings.Contains(text, want) {
			t.Fatalf("it says %q:\n%s", want, text)
		}
	}
	st, _ = r.d.Status(true)
	joined := strings.Join(st.Leftovers, "\n")
	if len(st.Leftovers) != 3 || !strings.Contains(joined, "left by B: branch open-b stays: it is not merged") || strings.Contains(joined, "done-a") || strings.Contains(joined, "not yet checked") {
		t.Fatalf("status lists what stays, and no more: %q", st.Leftovers)
	}
	for _, askWord := range []string{"your OK", "?", "waiting for"} {
		if strings.Contains(joined, askWord) {
			t.Fatalf("what stays is stated, not asked: %q", joined)
		}
	}
	// Once B's pull request merges, the next run takes it; what was removed is not judged again.
	w.merged["open-b"] = true
	w.removed = nil
	if _, err := r.d.Cleanup(""); err != nil {
		t.Fatal(err)
	}
	if len(w.removed) != 1 || w.removed[0] != "open-b" {
		t.Fatalf("%q", w.removed)
	}
	// A branch removed by hand stops being listed, its session resolved.
	delete(w.merged, "dirty-c")
	if _, err := r.d.Cleanup(""); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.d.Status(true); strings.Contains(strings.Join(st.Leftovers, "\n"), "dirty-c") {
		t.Fatalf("%q", st.Leftovers)
	}
}

// A close can take its cleanup along; an open session's branch is refused.
func TestCleanupOfOneSession(t *testing.T) {
	r := newRig(t, "none")
	w := &cleanupWorld{prs: prs{}, merged: map[string]bool{"done-a": true}}
	r.d.Progress = w
	closedOn(t, r, "A", "done-a")
	adopted(t, r, "B")
	if _, err := r.d.Cleanup("B"); !isRefused(err) || !strings.Contains(err.Error(), "still open") {
		t.Fatalf("%v", err)
	}
	cleaned, err := r.d.Cleanup("A")
	if err != nil || len(cleaned) != 1 || cleaned[0].Outcome != "removed" {
		t.Fatalf("%+v %v", cleaned, err)
	}
}

// The after-wake command archives a chat whose window holds 200 entries, so it
// runs hourly at most, not once per wake.
func TestAfterWakeRunsHourlyAtMost(t *testing.T) {
	r := newRig(t, "webhook")
	hook(t, r)
	marker := filepath.Join(r.w.Root, "ran")
	r.d.Cfg.AfterWake = []string{"echo ran >> " + marker}
	adopted(t, r, "T1")
	wake := func() {
		t.Helper()
		e, err := r.d.Wait("T1", time.Minute, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.d.Wake("T1", e, ""); err != nil {
			t.Fatal(err)
		}
	}
	runs := func() int {
		b, _ := os.ReadFile(marker)
		return strings.Count(string(b), "ran")
	}
	wake()
	r.w.Clock.Sleep(10 * time.Minute)
	wake()
	if n := runs(); n != 1 {
		t.Fatalf("ran %d times within the hour", n)
	}
	r.w.Clock.Sleep(time.Hour)
	wake()
	if n := runs(); n != 2 {
		t.Fatalf("ran %d times", n)
	}
}

// Every wake opens on how a chat driver talks, since a routine's prompt, not
// the bot's persona, starts its run, and closes on what to do.
func TestAWakeSaysWhatToReadAndWhatToDo(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	brief := filepath.Join(r.w.Root, "BOT.md")
	_ = os.WriteFile(brief, []byte("# bot\n"), 0o644)
	r.d.Cfg.Brief = brief
	adopted(t, r, "T1")
	e, _ := r.d.Wait("T1", time.Minute, 10*time.Minute)
	if err := r.d.Wake("T1", e, ""); err != nil {
		t.Fatal(err)
	}
	out := l.output(0)
	if !strings.HasPrefix(out, "Before you message anyone, read "+brief+" § The chat") {
		t.Fatalf("%s", out)
	}
	for _, want := range []string{"next: decide from its reply above", "drive --batch b1 send T1@", "drive --batch b1 park T1@", "drive --batch b1 close T1@"} {
		if !strings.Contains(out, want) {
			t.Fatalf("it says %q:\n%s", want, out)
		}
	}
	// A brief that is not on this machine is not pointed at, and a long reply
	// never pushes either end out.
	r.d.Cfg.Brief = filepath.Join(r.w.Root, "nope.md")
	if err := r.d.Wake("T1", e, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(l.output(1), "Before you message") {
		t.Fatalf("%s", l.output(1))
	}
}
