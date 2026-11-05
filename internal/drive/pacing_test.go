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
	"slices"
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

// webhookLog is a driver's webhook that records what it is told, and when.
type webhookLog struct {
	mu     sync.Mutex
	posts  []map[string]any
	at     []time.Time
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
		l.at = append(l.at, r.w.Clock.Now())
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

// after is how long after start the i-th post came.
func (l *webhookLog) after(i int, start time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.at[i].Sub(start)
}

// answeredBy has each post start a driver's run, which reads the batch half a
// minute on, as real runs did, and ends.
func (l *webhookLog) answeredBy(r *rig) *webhookLog {
	l.onPost = func() {
		run := *r.d
		r.w.Clock.At(r.w.Clock.Now().Add(30*time.Second), run.Seen)
	}
	return l
}

// watch runs a session's watch to its end, as the process does, on this
// goroutine: what happens meanwhile is scheduled on the clock.
func watch(t *testing.T, r *rig, c *testkit.Claude, label string) *drive.Ending {
	t.Helper()
	run := *r.d
	e, err := run.Notify(watchOf(c), label, drive.WakeQuiet)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A turn ended while a job it started runs. What the reply says decides when
// the driver hears, and a question never waits on a job.
func TestWhatAReplySaysDecidesTheWakeWhileItsJobRuns(t *testing.T) {
	t.Parallel()
	report := strings.Repeat("What the round found and how it was settled. ", 40)
	for _, tc := range []struct {
		name, reply string
		atLeast     time.Duration
		atMost      time.Duration
	}{
		{"ended on a question", "Run 5 passes three of five readings. Should it count as a pass?", time.Minute, 2 * time.Minute},
		{"a decision that needs someone", report + "\n\nOn your word I'll end the loop here.", time.Minute, 2 * time.Minute},
		{"asks, and waits as well", "Is the heads-up enough? Waiting on CI meanwhile.", time.Minute, 2 * time.Minute},
		{"an approval, and a wait", "Please approve the production migration. Waiting on CI meanwhile.", time.Minute, 2 * time.Minute},
		// Neither asking nor waiting, its own job's end usually resumes the turn.
		{"a report with a job left behind", report + "\n\nThe scratch worktrees are removed.", 2 * time.Hour, 2*time.Hour + time.Minute},
		{"anything unclear", "415 copies, no new failures.", 2 * time.Hour, 2*time.Hour + time.Minute},
		{"a wait that is over", "No longer waiting for CI. The change is merged and released.", 2 * time.Hour, 2*time.Hour + time.Minute},
		{"a wait for approval", "Waiting for approval before applying the production migration.", time.Minute, 2 * time.Minute},
		{"says it waits", "Waiting on the replays.", 2 * time.Hour, 2*time.Hour + time.Minute},
		{"says what comes after", report + "\n\nWhen the replays finish, I'll score them and report.", 2 * time.Hour, 2*time.Hour + time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, "webhook")
			l := hook(t, r).answeredBy(r)
			c := adopted(t, r, "T1")
			c.Busy("run the gate")
			c.Job("gate-replay --arms 5")
			c.Done(tc.reply)
			start := r.w.Clock.Now()
			r.w.Clock.At(start.Add(3*time.Hour), func() { c.JobDone("Replays are in.") })
			watch(t, r, c, "T1")
			if took := l.after(0, start); took < tc.atLeast || took > tc.atMost {
				t.Fatalf("the driver heard after %s, not between %s and %s", took, tc.atLeast, tc.atMost)
			}
			if !strings.Contains(l.output(0), "gate-replay") {
				t.Fatalf("it says what still runs:\n%s", l.output(0))
			}
		})
	}
}

// A driver that waits itself pays for no wake, so its wait keeps the contract
// its flags state, whatever the reply says.
func TestAWaitKeepsItsOwnTiming(t *testing.T) {
	for _, wake := range []string{"none", "webhook"} {
		r := newRig(t, wake)
		c := adopted(t, r, "T1")
		c.Busy("run the gate")
		c.Job("gate-replay")
		c.Done("Waiting on the replays.")
		start := r.w.Clock.Now()
		if _, err := r.d.Wait("T1", time.Second, time.Minute); err != nil {
			t.Fatal(err)
		}
		if took := r.w.Clock.Now().Sub(start); took > 2*time.Minute {
			t.Fatalf("%s: --bg 60 waited %s", wake, took)
		}
	}
}

// What needs someone wakes at once, whatever still runs and whatever the
// reply said before it stopped.
func TestAJobHoldsBackNoPromptErrorOrInterrupt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		end   func(c *testkit.Claude)
		state string
	}{
		{"a permission prompt", func(c *testkit.Claude) { c.Ask("rm -rf /tmp/x") }, "blocked:permission"},
		{"a question prompt", func(c *testkit.Claude) { c.Set("blocked", "question", "Which arm?") }, "blocked:question"},
		{"an API error", func(c *testkit.Claude) { c.Fail("overloaded", "Overloaded") }, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "webhook")
			l := hook(t, r)
			c := adopted(t, r, "T1")
			c.Busy("run the gate")
			c.Say("Waiting on the replays.")
			c.Job("gate-replay")
			tc.end(c)
			start := r.w.Clock.Now()
			e := watch(t, r, c, "T1")
			if string(e.State) != tc.state || l.count() != 1 {
				t.Fatalf("%+v, %d posts", e, l.count())
			}
			if took := l.after(0, start); took > 2*time.Minute {
				t.Fatalf("it waited %s on a job", took)
			}
		})
	}
}

// A recap goes before a park or a close, so a driver waits to read it,
// whatever it says and whatever still runs.
func TestARecapWakesWhateverStillRuns(t *testing.T) {
	t.Parallel()
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	c.Job("gh pr checks --watch")
	c.Edit(func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) { c.Done("Merged. The wait on the release is still running.") }
	})
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "recap"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	start := r.w.Clock.Now()
	r.w.Clock.At(start.Add(3*time.Hour), func() { c.JobDone("Released.") })
	watch(t, r, c, "T1")
	if took := l.after(0, start); took > 2*time.Minute {
		t.Fatalf("a recap waited %s on a job", took)
	}
}

// A five-hour job costs what it should: the one wake at its real end, and one
// backstop along the way, where a wake every ten minutes cost thirty. The
// watch stays armed throughout, so no run has to arm it again.
func TestAFiveHourJobCostsOneWakeAndABackstop(t *testing.T) {
	t.Parallel()
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	name := watchOf(c)
	c.Busy("run the gate")
	c.Job("gate-replay")
	c.Done("Replays are running. When they finish, I'll score them and report.")
	var alive bool
	var posts int
	var first string
	r.w.Clock.At(r.w.Clock.Now().Add(5*time.Hour), func() {
		alive, posts, first = r.watches.Alive(name), l.count(), l.output(0)
		c.JobDone("All five readings are scored. Should run 5 count as a pass?")
	})
	e := watch(t, r, c, "T1")
	if !alive {
		t.Fatal("the watch stays armed while the job runs")
	}
	if posts != 1 {
		t.Fatalf("five hours of waiting cost %d wakes, not the one backstop at two hours", posts)
	}
	if !strings.Contains(first, "gate-replay") || !strings.Contains(first, "drive --batch b1 watch T1") {
		t.Fatalf("the backstop says what runs, and the one command that answers it: %s", first)
	}
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

// A report with a job left behind wakes once that job has run two hours, and
// not again for that view until the next mark, the marks growing further
// apart.
func TestAReplyIsToldOncePerView(t *testing.T) {
	t.Parallel()
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	c.Busy("land it")
	c.Job("tail -f server.log")
	c.Done("PR #41 is open and its description is written.")
	start := r.w.Clock.Now()
	var at3h, at7h, at13h int
	r.w.Clock.At(start.Add(3*time.Hour), func() { at3h = l.count() })
	r.w.Clock.At(start.Add(7*time.Hour), func() { at7h = l.count() })
	r.w.Clock.At(start.Add(13*time.Hour), func() {
		at13h = l.count()
		c.JobDone("Stopped.")
	})
	watch(t, r, c, "T1")
	if at3h != 1 || at7h != 2 || at13h != 2 {
		t.Fatalf("one wake, then one some hours on, and none soon after: %d, %d, %d", at3h, at7h, at13h)
	}
}

// A run can act before its post returns: that run heard the wake.
func TestARunThatAnswersBeforeThePostReturnsHeardIt(t *testing.T) {
	t.Parallel()
	r := newRig(t, "webhook")
	l := hook(t, r)
	run := *r.d
	l.onPost = func() { run.Seen() }
	c := adopted(t, r, "T1")
	r.w.Clock.Sleep(2 * time.Minute) // past the run that set it up
	c.Busy("land it")
	c.Job("tail -f server.log")
	c.Done("PR #41 is open and its description is written.")
	got := -1
	r.w.Clock.At(r.w.Clock.Now().Add(3*time.Hour), func() {
		got = l.count()
		c.JobDone("Stopped.")
	})
	watch(t, r, c, "T1")
	if got != 1 {
		t.Fatalf("heard before the post returned, it needs no repeat: %d wakes in 3 hours", got)
	}
}

// The two hours a job holds a turn's wake run from the turn's end, not from
// when its watch was armed: a watch armed late, by watch --due or a driver's
// watch, does not push the wake further off.
func TestAJobsHoldRunsFromTheTurnsEnd(t *testing.T) {
	t.Parallel()
	report := strings.Repeat("What the round found and how it was settled. ", 40)
	for _, tc := range []struct{ name, reply, why string }{
		{"it reports", report, "job-running"},
		{"it says it waits", "Waiting on the replays.", "job-wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, "webhook")
			hook(t, r).answeredBy(r)
			c := adopted(t, r, "T1")
			c.Busy("run the gate")
			c.Job("gate-replay")
			c.Done(tc.reply)
			ended := r.w.Clock.Now()
			r.w.Clock.Sleep(time.Hour)
			r.w.Clock.At(ended.Add(4*time.Hour), func() { c.JobDone("Replays are in.") })
			watch(t, r, c, "T1")
			p := postings(r, "T1")[0]
			if got := p.Due.Sub(ended); p.Why != tc.why || got < 2*time.Hour || got > 2*time.Hour+10*time.Second {
				t.Fatalf("%s due %s after the turn ended, not two hours", p.Why, got)
			}
		})
	}
}

// Attempts nobody heard do not stretch the backstop: a driver back from an outage
// hears of a job at the first mark, not days on.
func TestUnheardAttemptsDoNotStretchTheBackstop(t *testing.T) {
	t.Parallel()
	r := newRig(t, "webhook")
	l := hook(t, r)
	c := adopted(t, r, "T1")
	c.Busy("land it")
	c.Job("tail -f server.log")
	c.Done("PR #41 is open and its description is written.")
	view := r.view("T1")
	r.w.Clock.Sleep(2 * time.Minute)
	for range 6 {
		_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindPosting, Session: "T1", View: view},
			batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWake, Session: "T1", HTTP: 204, View: view, Jobs: 1})
		r.w.Clock.Sleep(time.Minute)
	}
	r.d.Seen()
	got := -1
	r.w.Clock.At(r.w.Clock.Now().Add(5*time.Hour), func() {
		got = l.count()
		c.JobDone("Stopped.")
	})
	watch(t, r, c, "T1")
	if got != 1 {
		t.Fatalf("heard once, the next mark is hours off, not days: %d posts in five hours", got)
	}
}

// A job that ends with the session still idle, which no notification resumed,
// is news the driver has no other way to hear, though the view is the same.
func TestAJobThatEndsWithoutResumingStillWakes(t *testing.T) {
	t.Parallel()
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	c.Busy("land it")
	c.Job("gate-replay")
	c.Done("Waiting on the replays.")
	got := -1
	r.w.Clock.At(r.w.Clock.Now().Add(3*time.Hour), func() {
		got = l.count()
		c.Edit(func(c *testkit.Claude) { c.Shells = nil })
	})
	watch(t, r, c, "T1")
	if got != 1 || l.count() != 2 {
		t.Fatalf("the backstop, then the job's end: %d, %d", got, l.count())
	}
}

// What a wake cuts may be what the session asks, so a wake that cannot show it
// says to read it first, and never says only to wait.
func TestAQuestionTheWakeCutIsReadFirst(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	c.Busy("run the gate")
	c.Job("gate-replay")
	c.Done("Should run 5 count as a pass?\n\n" + strings.Repeat("What the round found and how it was settled. ", 100) + "\n\nWaiting on CI.")
	r.w.Clock.At(r.w.Clock.Now().Add(time.Hour), func() { c.JobDone("Done.") })
	watch(t, r, c, "T1")
	out := l.output(0)
	if strings.Contains(out, "Should run 5") {
		t.Fatal("the question falls outside the wake's tail, or this tests nothing")
	}
	if !strings.Contains(out, "its reply is cut above: read all of it first (drive --batch b1 read T1)") || strings.Contains(out, "run only") {
		t.Fatalf("it is told to read what it cannot see, not to answer the wait:\n%s", out[len(out)-600:])
	}
}

// A reply that asks gets an answer, not the line for one that waits.
func TestAReplyThatAsksIsNotToldToWait(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r).answeredBy(r)
	c := adopted(t, r, "T1")
	c.Busy("run the gate")
	c.Job("gate-replay")
	c.Done("Should run 5 count as a pass?")
	r.w.Clock.At(r.w.Clock.Now().Add(time.Hour), func() { c.JobDone("Done.") })
	watch(t, r, c, "T1")
	out := l.output(0)
	if strings.Contains(out, "run only") || !strings.Contains(out, "its reply asks something") {
		t.Fatalf("%s", out)
	}
}

// Endings close together make one post, naming them all, however the watches
// race for it, and each other watch then has nothing to add.
func TestWatchesEndingTogetherMakeOnePost(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	sessions := map[string]*testkit.Claude{}
	for _, label := range []string{"A", "B", "C", "D", "E", "F"} {
		c := adopted(t, r, label)
		c.Busy("work")
		c.Done("the round is finished")
		sessions[label] = c
	}
	r.w.Clock.Sleep(2 * time.Minute)
	var wg sync.WaitGroup
	var mu sync.Mutex
	covered := 0
	for label, c := range sessions {
		run := *r.d
		wg.Go(func() {
			e, err := run.Notify(watchOf(c), label, drive.WakeQuiet)
			if err != nil {
				t.Error(err)
				return
			}
			if e.Covered {
				mu.Lock()
				covered++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if n := l.count(); n != 1 {
		t.Fatalf("six endings, %d posts", n)
	}
	if covered != 5 || strings.Count(l.output(0), "also ended, in this one wake") != 5 {
		t.Fatalf("one post tells of them all: %d covered\n%s", covered, l.output(0))
	}
	if m, _ := r.d.Metrics(); m.Wakes != 1 || m.Carried != 5 {
		t.Fatalf("metrics tell posts from endings carried: %+v", m)
	}
	st, _ := r.d.Status(false)
	for _, row := range st.Rows {
		if !row.Unhandled {
			t.Fatalf("each reads as woken, so nothing is lost from status: %+v", row)
		}
	}
}

// What another post carries shows no reply, so it says to read one, whatever else
// it says to do.
func TestAnEndingCarriedSaysToReadItsReply(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	a, b := adopted(t, r, "A"), adopted(t, r, "B")
	a.Busy("work")
	a.Done("finished")
	b.Busy("work")
	b.Job("gate-replay")
	b.Done("Should run 5 count as a pass?")
	r.w.Clock.Sleep(2 * time.Minute)
	watch(t, r, a, "A")
	out := l.output(0)
	if !strings.Contains(out, "also ended, in this one wake: B") || !strings.Contains(out, "read its reply first (drive --batch b1 read B)") {
		t.Fatalf("%s", out)
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
	watch(t, r, a, "A")
	start := r.w.Clock.Now()
	b.Busy("work")
	b.Done("finished too")
	watch(t, r, b, "B")
	if took := l.after(1, start); took < 3*time.Minute || took > 3*time.Minute+10*time.Second {
		t.Fatalf("it waited for the gap, not %s", took)
	}
}

// A run that acts on a session while pacing holds its wake leaves nothing to post
// about the turn it moved past, and the new turn's end wakes the driver.
func TestASessionActedOnMeanwhilePostsNothing(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	a, b := adopted(t, r, "A"), adopted(t, r, "B")
	a.Busy("work")
	a.Done("finished")
	r.w.Clock.Sleep(2 * time.Minute)
	watch(t, r, a, "A")
	// B ended moments after A's post, so pacing holds its wake for that run.
	b.Busy("work")
	b.Done("finished too")
	b.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	start := r.w.Clock.Now()
	acting := *r.d
	r.w.Clock.At(start.Add(90*time.Second), func() {
		rd, err := acting.Read("B", drive.ReadLast, 10)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := acting.Send(rd.View.String(), drive.Step{Line: "next step"}, drive.SendOptions{}); err != nil {
			t.Error(err)
		}
	})
	got := -1
	r.w.Clock.At(start.Add(20*time.Minute), func() {
		got = l.count()
		b.Done("next step finished")
	})
	watch(t, r, b, "B")
	if got != 1 {
		t.Fatalf("B moved on, so nothing is posted for its old turn: %d posts", got)
	}
	if n := l.count(); n != 2 || !strings.Contains(l.output(1), "next step finished") {
		t.Fatalf("its new turn's end wakes the driver: %d posts", n)
	}
}

// A post in flight holds the next, however long that one has waited: it would
// otherwise cross the post that may yet carry it.
func TestHeldPastItsLimitAWakeStillWaitsForAPostInFlight(t *testing.T) {
	r := newRig(t, "webhook")
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free()
	l := hook(t, r)
	inFlight := make(chan struct{})
	var first sync.Once
	l.onPost = func() {
		first.Do(func() { close(inFlight) })
		<-release
	}
	a, b := adopted(t, r, "A"), adopted(t, r, "B")
	a.Busy("work")
	a.Done("finished")
	r.w.Clock.Sleep(2 * time.Minute)
	aDone := make(chan struct{})
	run := *r.d
	go func() {
		defer close(aDone)
		_, _ = run.Notify(watchOf(a), "A", drive.WakeQuiet)
	}()
	<-inFlight
	b.Busy("work")
	b.Done("finished too")
	seen := -1
	r.w.Clock.At(r.w.Clock.Now().Add(11*time.Minute+30*time.Second), func() {
		seen = l.count()
		free()
	})
	watch(t, r, b, "B")
	<-aDone
	if seen != 1 {
		t.Fatalf("%d posts in flight at once", seen)
	}
}

// A driver acting on other sessions holds a wake until its run is quiet, for a
// minute, and no longer than ten: it is busy, not gone.
func TestARunUnderWayHoldsAWakeUntilItIsQuiet(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	c := adopted(t, r, "T1")
	r.w.Clock.Sleep(2 * time.Minute)
	c.Busy("work")
	c.Done("finished")
	start := r.w.Clock.Now()
	acted := start.Add(50 * time.Second)
	r.w.Clock.At(acted, func() { _, _ = r.d.Note("", "the run acts on something else") })
	watch(t, r, c, "T1")
	if got := l.after(0, acted); got < time.Minute || got > time.Minute+10*time.Second {
		t.Fatalf("it posted %s after the driver's last action", got)
	}
}

func TestABusyDriverHoldsAWakeNoLongerThanTenMinutes(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	c := adopted(t, r, "T1")
	r.w.Clock.Sleep(2 * time.Minute)
	c.Busy("work")
	c.Done("finished")
	start := r.w.Clock.Now()
	var act func()
	act = func() {
		_, _ = r.d.Note("", "the run acts on something else")
		r.w.Clock.At(r.w.Clock.Now().Add(30*time.Second), act)
	}
	r.w.Clock.At(start.Add(30*time.Second), act)
	watch(t, r, c, "T1")
	// Ready after its minute's quiet, then held for mostHeld.
	if got := l.after(0, start); got < 11*time.Minute || got > 11*time.Minute+10*time.Second {
		t.Fatalf("it posted %s on", got)
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
		watch(t, r, sessions[i], string(rune('A'+i)))
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
	start := r.w.Clock.Now()
	var whileOut int
	var row drive.Row
	r.w.Clock.At(start.Add(3*time.Minute), func() {
		whileOut = l.count()
		st, _ := r.d.Status(false)
		row = st.Rows[3]
		// The usage comes back and the owner's own message starts a run.
		run := *r.d
		run.Seen()
	})
	watch(t, r, sessions[3], "D")
	if whileOut != 3 {
		t.Fatalf("no post while the driver is not answering: %d", whileOut)
	}
	if !strings.Contains(row.Watch, "waits until the driver answers") || row.Due != "" {
		t.Fatalf("its row says what it waits for, and it is not stuck: %+v", row.Wake)
	}
	if n := l.count(); n != 4 || !strings.Contains(l.output(3), "a question for you?") {
		t.Fatalf("what waited is posted once a driver answers: %d", n)
	}
	if st, _ := r.d.Status(false); st.Unheard != "" {
		t.Fatalf("and the batch reads as heard again: %q", st.Unheard)
	}
}

// While nobody answers, posts come further and further apart, to one every two
// hours, and what they carry still arrives.
func TestUnansweredWakesGetFurtherApart(t *testing.T) {
	t.Parallel()
	r := newRig(t, "webhook")
	l := hook(t, r)
	c := adopted(t, r, "T1")
	r.w.Clock.Sleep(2 * time.Minute)
	c.Busy("land it")
	c.Job("gate-replay")
	c.Done("Should run 5 count as a pass?")
	start := r.w.Clock.Now()
	r.w.Clock.At(start.Add(8*time.Hour), func() { c.JobDone("All done. Merged.") })
	watch(t, r, c, "T1")
	var gaps []time.Duration
	for i := 1; i < l.count(); i++ {
		gaps = append(gaps, (l.after(i, start) - l.after(i-1, start)).Round(time.Minute))
	}
	want := []time.Duration{10 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour}
	if len(gaps) < len(want) {
		t.Fatalf("gaps %v", gaps)
	}
	for i, g := range want {
		if d := gaps[i] - g; d < -time.Minute || d > time.Minute {
			t.Fatalf("gaps %v, want %v", gaps, want)
		}
	}
	if !strings.Contains(l.output(l.count()-1), "All done. Merged.") {
		t.Fatalf("the ending that came meanwhile arrives: %s", l.output(l.count()-1))
	}
}

// An older drive's runs left no trace of a read, so wakes it posted, with a run
// that only read and armed the watch again behind each, say nothing of whether
// its driver answers. A batch carried across an install reads as heard, and its
// next wake goes when due. A post that failed is evidence whichever drive made it.
func TestWakesAnOlderDrivePostedSayNothingOfHearing(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	c := adopted(t, r, "T1")
	older := func(http int, reason string) {
		now := r.w.Clock.Now()
		_ = r.b.Append(batch.Event{At: now, Kind: batch.KindPosting, Session: "T1"},
			batch.Event{At: now, Kind: batch.KindWake, Session: "T1", HTTP: http, Reason: reason},
			batch.Event{At: now, Kind: batch.KindWatch, Session: "T1"})
		r.w.Clock.Sleep(11 * time.Minute)
	}
	for range 8 { // as the mini's log read at the install
		older(200, "")
	}
	if st, _ := r.d.Status(false); st.Unheard != "" {
		t.Fatalf("%q", st.Unheard)
	}
	c.Busy("work")
	c.Done("finished")
	start := r.w.Clock.Now()
	watch(t, r, c, "T1")
	if took := l.after(0, start); took > 2*time.Minute {
		t.Fatalf("its first wake after the install waited %s on wakes nobody can judge", took)
	}
	// A wake this drive posts counts from here on.
	r.w.Clock.Sleep(11 * time.Minute)
	evs, err := r.b.Events()
	if err != nil {
		t.Fatal(err)
	}
	if h := batch.Hearings(evs, r.w.Clock.Now()); h.Strikes != 1 {
		t.Fatalf("%+v", h)
	}
	for range 3 {
		older(0, "http 429 no usage")
	}
	if st, _ := r.d.Status(false); !strings.Contains(st.Unheard, "has not answered its last 4 wakes") {
		t.Fatalf("%q", st.Unheard)
	}
}

// How wakes are heard, from the log alone.
func TestHearingIsReadFromTheLog(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var evs []batch.Event
	h := func() batch.Hearing { return batch.Hearings(evs, now) }
	for range 3 {
		evs = append(evs, batch.Event{At: now, Kind: batch.KindWake, Session: "A", HTTP: 204, Why: "turn-ended"})
		now = now.Add(11 * time.Minute)
	}
	if h().Strikes != 3 {
		t.Fatalf("%+v", h())
	}
	// A wake posted a moment ago has not had the time to be answered.
	evs = append(evs, batch.Event{At: now, Kind: batch.KindWake, Session: "A", HTTP: 204, Why: "turn-ended"})
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

// A timer posted while nobody answers is a timer lost, so it waits too, on the
// books, so ending its batch still stops it, and says so.
func TestATimerWaitsWhileNobodyAnswers(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	now := r.w.Clock.Now()
	for i := range 3 {
		_ = r.b.Append(batch.Event{At: now.Add(time.Duration(i) * time.Second), Kind: batch.KindWake, Session: "A", HTTP: 204, Why: "turn-ended"})
	}
	r.w.Clock.Sleep(11 * time.Minute)
	name := "drive-b1-timer-veto"
	if err := r.watches.Start(name, []string{"x"}, ""); err != nil {
		t.Fatal(err)
	}
	alive := false
	var shown string
	r.w.Clock.At(r.w.Clock.Now().Add(time.Minute), func() {
		alive = r.watches.Alive(name)
		st, _ := r.d.Status(false)
		shown = strings.Join(st.TimersHeld, "\n")
	})
	if err := r.d.Ring(name, "veto"); err != nil {
		t.Fatal(err)
	}
	if took := r.w.Clock.Now().Sub(now); took < 15*time.Minute {
		t.Fatalf("it rang after %s, inside the back-off", took)
	}
	if n := l.count(); n != 1 {
		t.Fatalf("it still rings: %d", n)
	}
	if !alive {
		t.Fatal("a timer that waits has left the books, so ending the batch cannot stop it")
	}
	if r.watches.Alive(name) {
		t.Fatal("it retires once it has posted")
	}
	if !strings.Contains(shown, `timer "veto" is due`) {
		t.Fatalf("status says it waits: %q", shown)
	}
	if st, _ := r.d.Status(false); len(st.TimersHeld) != 0 {
		t.Fatalf("not once it has rung: %q", st.TimersHeld)
	}
}

// A timer stopped while it waits no longer waits, in status or on the page.
func TestATimerStoppedWhileItWaitsIsNoLongerShown(t *testing.T) {
	r := newRig(t, "webhook")
	hook(t, r)
	now := r.w.Clock.Now()
	for i := range 3 {
		_ = r.b.Append(batch.Event{At: now.Add(time.Duration(i) * time.Second), Kind: batch.KindWake, Session: "A", HTTP: 204, Why: "turn-ended"})
	}
	r.w.Clock.Sleep(11 * time.Minute)
	name := "drive-b1-timer-veto"
	if err := r.watches.Start(name, []string{"x"}, ""); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.At(r.w.Clock.Now().Add(time.Minute), func() {
		ov, err := r.d.Overview(false)
		if err != nil || !strings.Contains(string(ov.HTML(r.w.Clock.Now())), `timer &#34;veto&#34; is due`) {
			t.Errorf("the page shows it: %v", err)
		}
		_ = r.watches.Stop(name)
		st, _ := r.d.Status(false)
		if len(st.TimersHeld) != 0 {
			t.Errorf("stopped, it no longer waits: %q", st.TimersHeld)
		}
		_ = r.watches.Start(name, []string{"x"}, "") // for the rest of this run
	})
	if err := r.d.Ring(name, "veto"); err != nil {
		t.Fatal(err)
	}
}

// The machine's watchdog runs no model unless something needs one.
func TestTheWatchdogPostsOnlyWhatNeedsADriver(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	s := r.w.Add("S1")
	done(s, "onboard", "analysis")
	if _, err := r.d.Send(r.view(s.Block[len(s.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "S1"}); err != nil {
		t.Fatal(err)
	}
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
	if err := r.d.Park(r.view("S1"), "waits on the owner", r.w.Clock.Now().Add(48*time.Hour)); err != nil {
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
}

// A session that is gone is told until a run hears it: a post that answers 200
// while the bot's usage is out told no one, and nothing else will.
func TestTheWatchdogTellsOfAGoneSessionUntilARunHears(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	adopted(t, r, "S1")
	r.w.Restart()
	r.w.Clock.Sleep(2 * time.Minute) // past the run that opened it
	due := func() {
		t.Helper()
		if _, err := r.d.DueAll(); err != nil {
			t.Fatal(err)
		}
	}
	due()
	if l.count() != 1 || !strings.Contains(l.output(0), "S1 is gone:") || !strings.Contains(l.output(0), "drive --batch b1 open S1") {
		t.Fatalf("%d posts", l.count())
	}
	r.w.Clock.Sleep(5 * time.Minute)
	if due(); l.count() != 1 {
		t.Fatalf("not again before a run has had the time: %d", l.count())
	}
	r.w.Clock.Sleep(25 * time.Minute)
	if due(); l.count() != 2 {
		t.Fatalf("no run answered, so it goes again: %d", l.count())
	}
	r.w.Clock.Sleep(time.Second)
	r.d.Seen()
	r.w.Clock.Sleep(30 * time.Minute)
	if due(); l.count() != 2 {
		t.Fatalf("heard, it goes no more: %d", l.count())
	}
}

// A post that failed told no one, even if a run comes for another reason.
func TestAFailedNoticeIsNotTold(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	l.status = 500
	r.d.Sleep = func(time.Duration) {} // the post's own retries
	adopted(t, r, "S1")
	r.w.Restart()
	r.w.Clock.Sleep(2 * time.Minute)
	if _, err := r.d.DueAll(); err != nil {
		t.Fatal(err)
	}
	r.d.Seen()
	l.status = 204
	r.w.Clock.Sleep(4 * time.Minute)
	before := l.count()
	if _, err := r.d.DueAll(); err != nil {
		t.Fatal(err)
	}
	if l.count() != before+1 {
		t.Fatalf("it goes again: %d then %d", before, l.count())
	}
}

// One command answers an ask and sends what follows: no read for the view.
func TestAnAnswerCanCarryItsStep(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	id, err := r.d.Ask("T1", "Count run 5 as a pass?", "yes", "T1 stays parked")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.Park(r.view("T1"), "waits on A1", time.Time{}); err != nil {
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

// The answer goes on record with the step's intent, before anything is typed,
// so a command cut off while typing leaves no open ask for a retry to send
// again.
func TestTheAnswerIsOnRecordBeforeTheStepIsTyped(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	id, _ := r.d.Ask("T1", "Go on?", "yes", "it waits")
	gate := make(chan struct{})
	c.Edit(func(c *testkit.Claude) { c.TypeGate = gate })
	done := make(chan error, 1)
	run := *r.d
	go func() {
		_, _, err := run.AnswerAndSend(id, "yes", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{})
		done <- err
	}()
	var answered, intent bool
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && !intent; time.Sleep(time.Millisecond) {
		evs, _ := r.b.Events()
		for _, e := range evs {
			intent = intent || e.Kind == batch.KindSendIntent && e.Text == "go on"
			answered = answered || e.Kind == batch.KindAnswer
		}
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !intent || !answered {
		t.Fatalf("intent %v, answer %v", intent, answered)
	}
}

// A retry says what the record knows of the step, which is whether it went only
// once something settled that.
func TestARetryOfAnAnswerSaysWhatBecameOfItsStep(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	id, _ := r.d.Ask("T1", "Go on?", "yes", "it waits")
	gate := make(chan struct{})
	c.Edit(func(c *testkit.Claude) { c.TypeGate = gate })
	done := make(chan error, 1)
	run := *r.d
	go func() {
		_, _, err := run.AnswerAndSend(id, "yes", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{})
		done <- err
	}()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if len(r.events(batch.KindAnswer)) > 0 {
			break
		}
	}
	retry := *r.d
	_, _, err := retry.AnswerAndSend(id, "yes", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "nothing says yet whether its step reached the session") || !strings.Contains(err.Error(), "drive --batch b1 read T1 --user") {
		t.Fatalf("mid-delivery it cannot say the step went: %v", err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, _, err = retry.AnswerAndSend(id, "yes", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "its step arrived") {
		t.Fatalf("settled, it says how it went: %v", err)
	}
	// An answer recorded with no step says only that.
	alone, _ := r.d.Ask("T1", "Again?", "no", "it waits")
	if _, err := r.d.Answer(alone, "no", "recommended", false); err != nil {
		t.Fatal(err)
	}
	_, _, err = retry.AnswerAndSend(alone, "no", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "no step went with it") {
		t.Fatalf("%v", err)
	}
}

// A refused step leaves the ask open, so the one command can run again. The
// refusal's next sends the step alone, so it also says how to record the
// answer, in the owner's words as given, or the answer is lost to whatever
// the driver runs instead.
func TestARefusedStepRecordsNoAnswer(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	id, _ := r.d.Ask("T1", "Go on?", "yes", "it waits")
	c.Busy("still at work")
	_, got, err := r.d.AnswerAndSend(id, "Qiushi in chat: yes, go on", "recommended", true, drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || got != "" {
		t.Fatalf("%q %v", got, err)
	}
	evs, _ := r.b.Events()
	if _, asks, _ := batch.Records(evs); asks[0].Answer != nil {
		t.Fatalf("no answer recorded: %+v", asks[0])
	}
	record := "drive --batch b1 record answer A1 --took recommended '--source=Qiushi in chat: yes, go on' --explained"
	if msg := err.Error(); !strings.Contains(msg, "T1 is working") || !strings.Contains(msg, "A1 is not recorded") || !strings.Contains(msg, record) {
		t.Fatalf("the refusal says the answer waits, and how to record it:\n%s", msg)
	}
	batchAsk, _ := r.d.Ask("", "Release tonight?", "no", "it waits")
	_, _, err = r.d.AnswerAndSend(batchAsk, "no, not tonight", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "names no session") || !strings.Contains(err.Error(), "'--source=no, not tonight'") {
		t.Fatalf("an ask about the batch has no session to send to, and its answer keeps the owner's words: %v", err)
	}
	// A session that cannot be read refuses the same way.
	gone, _ := r.d.Ask("T1", "Close it?", "yes", "it waits")
	r.w.Close(c.Block)
	_, _, err = r.d.AnswerAndSend(gone, "yes", "recommended", false, drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), gone+" is not recorded") || !strings.Contains(err.Error(), "record answer "+gone+" --took recommended --source=yes") {
		t.Fatalf("a session gone leaves the answer to record: %v", err)
	}
}

// cleanupWorld answers as gwt does: which branches are merged, and removing them.
type cleanupWorld struct {
	prs
	merged    map[string]bool
	refuse    map[string]string
	removed   []string
	landedErr error
	stepsErr  error
	unjudged  map[string]string // branches gwt failed on, and what it said
}

func (w *cleanupWorld) Steps(string) (progress.Steps, error) { return progress.Steps{}, w.stepsErr }

func (w *cleanupWorld) Landed(dir string, branches []string, commits []progress.Commit) (progress.Landing, error) {
	if w.landedErr != nil {
		return progress.Landing{}, w.landedErr
	}
	l := progress.Landing{Repo: "/work/tool", Trunk: "main", Branches: branches, Merged: map[string]bool{}, Errors: w.unjudged}
	for _, b := range branches {
		if m, ok := w.merged[b]; ok && w.unjudged[b] == "" {
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

// A cleanup that could not read a session says so, and reads it again next time:
// it left what it left whether or not gwt could run.
func TestACleanupThatCouldNotReadASessionStaysOpen(t *testing.T) {
	r := newRig(t, "webhook")
	w := &cleanupWorld{prs: prs{}, merged: map[string]bool{"done-a": true, "odd-b": true}, landedErr: errors.New("gwt: cannot run"), unjudged: map[string]string{"odd-b": "bad object"}}
	r.d.Progress = w
	closedOn(t, r, "A", "done-a", "odd-b")
	if _, err := r.d.Cleanup(""); err != nil {
		t.Fatal(err)
	}
	st, _ := r.d.Status(true)
	if !strings.Contains(strings.Join(st.Leftovers, "\n"), "what A left could not be checked: gwt: cannot run") {
		t.Fatalf("%q", st.Leftovers)
	}
	w.landedErr = nil
	if _, err := r.d.Cleanup(""); err != nil {
		t.Fatal(err)
	}
	if len(w.removed) != 1 || w.removed[0] != "done-a" {
		t.Fatalf("the next run judges it: %q", w.removed)
	}
	st, _ = r.d.Status(true)
	if !strings.Contains(strings.Join(st.Leftovers, "\n"), "bad object") || strings.Contains(strings.Join(st.Leftovers, "\n"), "cannot run") {
		t.Fatalf("a branch gwt could not judge stays open, by what it said: %q", st.Leftovers)
	}
}

// A session whose steps could not be read may have worked in a worktree only they
// name, so that says nothing of what it left.
func TestACleanupThatCouldNotReadItsStepsStaysOpen(t *testing.T) {
	r := newRig(t, "webhook")
	w := &cleanupWorld{prs: prs{}, merged: map[string]bool{"done-a": true}, stepsErr: errors.New("claude-steps: no answer within 30s")}
	r.d.Progress = w
	closedOn(t, r, "A", "done-a")
	if _, err := r.d.Cleanup(""); err != nil {
		t.Fatal(err)
	}
	st, _ := r.d.Status(true)
	if !strings.Contains(strings.Join(st.Leftovers, "\n"), "what A left could not be checked: claude-steps") || len(w.removed) != 0 {
		t.Fatalf("%q %q", st.Leftovers, w.removed)
	}
	w.stepsErr = nil
	if _, err := r.d.Cleanup(""); err != nil {
		t.Fatal(err)
	}
	if len(w.removed) != 1 {
		t.Fatalf("the next run judges it: %q", w.removed)
	}
}

// Cleanup reaches back through this driver's batches, for what an earlier one
// left, and never into another driver's.
func TestCleanupCoversThisDriversBatchesOnly(t *testing.T) {
	r := newRig(t, "webhook")
	w := &cleanupWorld{prs: prs{}, merged: map[string]bool{"here": true, "older": true, "stays": false, "theirs": true}}
	r.d.Progress = w
	closedOn(t, r, "A", "here")
	closedIn := func(name, driver, label string, branch string) {
		t.Helper()
		b, _, err := r.d.Store.New(batch.Meta{Name: name, Started: r.w.Clock.Now(), Wake: "none", Driver: driver})
		if err != nil {
			t.Fatal(err)
		}
		c := r.w.Add(label)
		c.Write(map[string]any{"type": "attachment", "gitBranch": branch})
		_ = b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindOpen, Session: label, Block: c.Block, SID: c.SID, Dir: c.Cwd},
			batch.Event{At: r.w.Clock.Now(), Kind: batch.KindClose, Session: label, Block: c.Block})
		if err := r.w.Close(c.Block); err != nil {
			t.Fatal(err)
		}
	}
	closedIn("older", "", "O", "older")
	closedIn("older2", "", "P", "stays")
	closedIn("elsewhere", "someone-else", "T", "theirs")
	r.d.BatchName = "b1"
	if _, err := r.d.Cleanup(""); err != nil {
		t.Fatal(err)
	}
	slices.Sort(w.removed)
	if !slices.Equal(w.removed, []string{"here", "older"}) {
		t.Fatalf("%q", w.removed)
	}
	st, _ := r.d.Status(true)
	if joined := strings.Join(st.Leftovers, "\n"); !strings.Contains(joined, "left by P (batch older2): branch stays stays") || strings.Contains(joined, "theirs") {
		t.Fatalf("%q", st.Leftovers)
	}
}

// What is held back shows where the owner reads, live and from the record alone,
// and in the document a driver parses.
func TestWhatIsHeldBackShowsOnThePageAndInStatus(t *testing.T) {
	r := newRig(t, "webhook")
	w := &cleanupWorld{prs: prs{}, merged: map[string]bool{"stays": false}}
	r.d.Progress = w
	closedOn(t, r, "A", "stays")
	if _, err := r.d.Cleanup(""); err != nil {
		t.Fatal(err)
	}
	c := adopted(t, r, "T1")
	c.Busy("run the gate")
	c.Job("gate-replay")
	c.Done("Waiting on the replays.")
	r.w.Clock.Sleep(time.Minute)
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	page := string(ov.HTML(r.w.Clock.Now()))
	if !strings.Contains(page, "it says it waits on its own job") || !strings.Contains(page, "at the latest") {
		t.Fatalf("the page says a job holds its wake, and until when:\n%s", page)
	}
	if !strings.Contains(page, "branch stays stays") {
		t.Fatal("the page lists what a closed session left")
	}
	st, _ := r.d.Status(true)
	if !strings.Contains(st.Rows[0].Watch, "it says it waits on its own job") {
		t.Fatalf("%q", st.Rows[0].Watch)
	}
	// A driver that heard a question says so, not that the session waits.
	c.Done("Should run 5 count as a pass?")
	view := r.view("T1")
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindPosting, Session: "T1", View: view},
		batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWake, Session: "T1", HTTP: 204, View: view, Jobs: 1})
	r.w.Clock.Sleep(time.Second)
	r.d.Seen()
	if st, _ := r.d.Status(false); !strings.Contains(st.Rows[0].Watch, "its driver has heard its reply") || strings.Contains(st.Rows[0].Watch, "says it waits") {
		t.Fatalf("%q", st.Rows[0].Watch)
	}
	// With nobody answering, the header says so, Rex down or not.
	for range 3 {
		_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWake, Session: "T1", HTTP: 204, Why: "turn-ended"})
	}
	r.w.Clock.Sleep(11 * time.Minute)
	ov, err = r.d.Overview(false)
	if err != nil {
		t.Fatal(err)
	}
	page = string(ov.HTML(r.w.Clock.Now()))
	for _, want := range []string{"Not answered", "has not answered its last 3 wakes", "branch stays stays"} {
		if !strings.Contains(page, want) {
			t.Fatalf("from the record alone it says %q:\n%s", want, page)
		}
	}
	st, _ = r.d.Status(true)
	body, _ := json.Marshal(st.JSON())
	for _, want := range []string{`"unheard":`, `"leftovers":`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("status --json carries %s: %s", want, body)
		}
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
	for _, want := range []string{"next: decide from its reply:", "drive --batch b1 send T1@", "drive --batch b1 park T1@", "drive --batch b1 close T1@"} {
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

// postings are a session's posts as the log records them, oldest first.
func postings(r *rig, label string) []batch.Event {
	var out []batch.Event
	for _, e := range r.events(batch.KindPosting) {
		if e.Session == label {
			out = append(out, e)
		}
	}
	return out
}

// A post records what made it go and when it was due, and what held it back if
// that took longer, so a late wake can be explained and a batch's wakes counted by
// cause from its record instead of inferred from when they came.
func TestAPostRecordsWhyItWent(t *testing.T) {
	t.Parallel()
	report := strings.Repeat("What the round found and how it was settled. ", 40)
	for _, tc := range []struct {
		name, reply, why string
		after            time.Duration
	}{
		{"it asks", "Should run 5 count as a pass?", "asks", time.Minute},
		{"it reports", report, "job-running", 2 * time.Hour},
		{"it says it waits", "Waiting on the replays.", "job-wait", 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, "webhook")
			hook(t, r).answeredBy(r)
			c := adopted(t, r, "T1")
			c.Busy("run the gate")
			c.Job("gate-replay")
			c.Done(tc.reply)
			start := r.w.Clock.Now()
			r.w.Clock.At(start.Add(3*time.Hour), func() { c.JobDone("Replays are in.") })
			watch(t, r, c, "T1")
			p := postings(r, "T1")[0]
			if p.Why != tc.why {
				t.Fatalf("why %q, not %q", p.Why, tc.why)
			}
			if got := p.Due.Sub(start); got < tc.after || got > tc.after+10*time.Second {
				t.Fatalf("due %s on, not %s", got, tc.after)
			}
			if p.HeldBy != "" {
				t.Fatalf("nothing held it: %q", p.HeldBy)
			}
		})
	}
	t.Run("the same view again", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "webhook")
		hook(t, r).answeredBy(r)
		c := adopted(t, r, "T1")
		c.Busy("land it")
		c.Job("tail -f server.log")
		c.Done("PR #41 is open.")
		r.w.Clock.At(r.w.Clock.Now().Add(7*time.Hour), func() { c.Edit(func(c *testkit.Claude) { c.Shells = nil }) })
		watch(t, r, c, "T1")
		var whys []string
		for _, p := range postings(r, "T1") {
			whys = append(whys, p.Why)
		}
		if !slices.Equal(whys, []string{"job-running", "job-check", "job-ended"}) {
			t.Fatalf("a report, a check while its job runs, and the job's end: %q", whys)
		}
	})
	t.Run("nobody answered", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "webhook")
		hook(t, r)
		c := adopted(t, r, "T1")
		c.Busy("land it")
		c.Job("gate-replay")
		c.Done("Should run 5 count as a pass?")
		r.w.Clock.At(r.w.Clock.Now().Add(15*time.Minute), func() { c.JobDone("Merged.") })
		watch(t, r, c, "T1")
		if p := postings(r, "T1"); len(p) < 2 || p[1].Why != "unheard" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("a prompt and an error", func(t *testing.T) {
		t.Parallel()
		for why, end := range map[string]func(c *testkit.Claude){
			"prompt":  func(c *testkit.Claude) { c.Ask("rm -rf /tmp/x") },
			"stopped": func(c *testkit.Claude) { c.Fail("overloaded", "Overloaded") },
		} {
			r := newRig(t, "webhook")
			hook(t, r)
			c := adopted(t, r, "T1")
			c.Busy("work")
			end(c)
			watch(t, r, c, "T1")
			if p := postings(r, "T1"); len(p) != 1 || p[0].Why != why {
				t.Fatalf("%s: %+v", why, p)
			}
		}
	})
	t.Run("a repost", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "webhook")
		hook(t, r)
		c := adopted(t, r, "T1")
		c.Busy("work")
		c.Done("finished")
		watch(t, r, c, "T1")
		r.w.Clock.Sleep(time.Second)
		r.d.Seen()
		r.w.Clock.Sleep(11 * time.Minute)
		if _, err := r.d.Watch("T1"); err != nil {
			t.Fatal(err)
		}
		watch(t, r, c, "T1")
		p := postings(r, "T1")
		if len(p) != 2 || p[0].Why != "turn-ended" || p[1].Why != "repost" {
			t.Fatalf("%+v", p)
		}
	})
}

// Hearing counts only wakes that carry a cause, so every post this drive makes
// has one, a session that is gone included.
func TestEveryPostCarriesItsCause(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	gone, done := adopted(t, r, "G"), adopted(t, r, "D")
	done.Busy("work")
	done.Done("finished")
	r.w.Clock.Sleep(2 * time.Minute)
	watch(t, r, done, "D")
	r.w.Clock.Sleep(11 * time.Minute)
	if err := r.w.Close(gone.Block); err != nil {
		t.Fatal(err)
	}
	watch(t, r, gone, "G")
	if err := r.d.Ring("", "veto"); err != nil {
		t.Fatal(err)
	}
	if l.count() != 3 {
		t.Fatalf("%d posts", l.count())
	}
	for _, e := range r.events(batch.KindWake) {
		if e.Why == "" {
			t.Fatalf("a post with no cause reads as an older drive's: %+v", e)
		}
	}
}

// What held a post back is on its record, with the time it was due.
func TestAPostRecordsWhatHeldItBack(t *testing.T) {
	t.Run("the gap after another post", func(t *testing.T) {
		r := newRig(t, "webhook")
		hook(t, r)
		a, b := adopted(t, r, "A"), adopted(t, r, "B")
		a.Busy("work")
		a.Done("finished")
		b.Busy("work") // so A's post has nothing of it to carry
		r.w.Clock.Sleep(2 * time.Minute)
		watch(t, r, a, "A")
		b.Done("finished too")
		watch(t, r, b, "B")
		p := postings(r, "B")[0]
		if p.HeldBy != "gap" || !p.At.After(p.Due) {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("a run under way", func(t *testing.T) {
		r := newRig(t, "webhook")
		hook(t, r)
		c := adopted(t, r, "T1")
		r.w.Clock.Sleep(2 * time.Minute)
		c.Busy("work")
		c.Done("finished")
		r.w.Clock.At(r.w.Clock.Now().Add(50*time.Second), func() { _, _ = r.d.Note("", "acts on something else") })
		watch(t, r, c, "T1")
		if p := postings(r, "T1")[0]; p.HeldBy != "run" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("nobody answering", func(t *testing.T) {
		r := newRig(t, "webhook")
		hook(t, r)
		c := adopted(t, r, "T1")
		now := r.w.Clock.Now()
		for i := range 3 {
			_ = r.b.Append(batch.Event{At: now.Add(time.Duration(i) * time.Second), Kind: batch.KindWake, Session: "A", HTTP: 204, Why: "turn-ended"})
		}
		c.Busy("work")
		c.Done("finished")
		r.w.Clock.Sleep(11 * time.Minute)
		watch(t, r, c, "T1")
		if p := postings(r, "T1")[0]; p.HeldBy != "unheard" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("carried by another's post", func(t *testing.T) {
		r := newRig(t, "webhook")
		hook(t, r)
		a, b := adopted(t, r, "A"), adopted(t, r, "B")
		a.Busy("work")
		a.Done("finished")
		b.Busy("work")
		b.Ask("rm -rf /tmp/x")
		r.w.Clock.Sleep(2 * time.Minute)
		watch(t, r, a, "A")
		if p := postings(r, "B")[0]; p.Via != "A" || p.Why != "prompt" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("a timer and the watchdog", func(t *testing.T) {
		r := newRig(t, "webhook")
		hook(t, r)
		now := r.w.Clock.Now()
		for i := range 3 {
			_ = r.b.Append(batch.Event{At: now.Add(time.Duration(i) * time.Second), Kind: batch.KindWake, Session: "A", HTTP: 204, Why: "turn-ended"})
		}
		r.w.Clock.Sleep(11 * time.Minute)
		if err := r.d.Ring("", "veto"); err != nil {
			t.Fatal(err)
		}
		p := postings(r, "")
		if len(p) != 1 || p[0].Why != "timer" || p[0].HeldBy != "unheard" || !p[0].At.After(p[0].Due) {
			t.Fatalf("%+v", p)
		}
		adopted(t, r, "S1")
		r.w.Restart()
		r.w.Clock.Sleep(5 * time.Minute)
		if _, err := r.d.DueAll(); err != nil {
			t.Fatal(err)
		}
		if p := postings(r, ""); len(p) != 2 || p[1].Why != "watchdog" {
			t.Fatalf("%+v", p)
		}
	})
}

// The log says it in words, and metrics counts a batch's wakes by it.
func TestAPostsReasonIsPrintedAndCounted(t *testing.T) {
	r := newRig(t, "webhook")
	hook(t, r).answeredBy(r)
	a, b := adopted(t, r, "A"), adopted(t, r, "B")
	a.Busy("work")
	a.Job("gate-replay")
	a.Done("Should run 5 count as a pass?")
	b.Busy("work") // so A's posts have nothing of it to carry
	r.w.Clock.Sleep(2 * time.Minute)
	r.w.Clock.At(r.w.Clock.Now().Add(time.Hour), func() { a.JobDone("Merged.") })
	watch(t, r, a, "A")
	b.Done("finished")
	watch(t, r, b, "B")
	bat, evs, err := r.d.Log(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	out := drive.LogText(bat, evs, r.w.Clock.Now())
	if !strings.Contains(out, "posting      A        asks, due ") || !strings.Contains(out, "posting      B        turn-ended, due ") {
		t.Fatalf("%s", out)
	}
	m, err := r.d.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	if m.ByWhy["asks"] != 1 || m.ByWhy["turn-ended"] != 2 {
		t.Fatalf("%+v", m.ByWhy)
	}
	if !strings.Contains(m.Text(), "wakes by cause: asks 1, turn-ended 2") {
		t.Fatalf("%s", m.Text())
	}
}
