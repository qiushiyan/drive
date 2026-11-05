package drive

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/turn"
)

// Each post wakes a chat driver for a model run, which is what a driver costs,
// so when a session's wake reaches its driver is decided here, once, for what
// waits (Notify, Ring), what recovers (Watch, Due, postDue) and what shows it
// (status, the page). Three facts stay apart: a turn ended, a post went out,
// and a driver heard it. Only the log says the last, since a webhook answers
// 200 while the bot's usage is out.

const (
	// WakeQuiet is how long a turn stays over before its wake.
	WakeQuiet = time.Minute
	// jobHold is how long a turn whose own job still runs stays quiet,
	// unless its reply asks something: the job's end usually resumes it, and
	// that turn's end is the wake that matters (in the 2026-11-02 batch, ten
	// wakes posted ten minutes into such a turn drew no action, and each
	// turn resumed, 4 minutes to 7 hours on). A job that never ends still
	// reaches its driver, at this mark and at ever longer ones after each it
	// heard.
	jobHold = 2 * time.Hour
	// repostAfter is how long a wake nobody acted on waits before it is posted
	// again: a run acting on it takes minutes.
	repostAfter = 10 * time.Minute
	// stuckAfter is how far past its time a watch that has not posted counts as
	// stuck.
	stuckAfter = 14 * time.Minute

	// wakeGap keeps a run from being followed at once by another about what
	// it has probably just read: a run's actions land within two minutes.
	wakeGap = 3 * time.Minute
	// runQuiet is how long since a driver's last trace before a run counts as
	// over.
	runQuiet = time.Minute
	// mostHeld bounds how long a run under way holds a wake back, as a driver
	// busy on other sessions would otherwise hold it for good.
	mostHeld = 10 * time.Minute
	// postLimit is how long a post can take, with its retries, before one
	// that recorded no result is taken to have died.
	postLimit = 12 * time.Minute
	// stallAfter is how many wakes in a row failed or went unheard before a
	// batch's wakes back off, as while the bot's usage is out.
	stallAfter = 3
)

// What makes a post go, as the log records it.
const (
	whyEnded      = "turn-ended"  // a turn is over and no job runs
	whyPrompt     = "prompt"      // it stopped on a prompt
	whyStopped    = "stopped"     // an API error, a rejected login or an interrupt
	whyGone       = "gone"        // its session is gone
	whyAsks       = "asks"        // a job runs, and the reply asks something
	whyRecap      = "recap"       // a job runs, and the driver sent for this reply
	whyJobRunning = "job-running" // a job runs, the reply neither asks nor says it waits, and the job has run past jobHold
	whyOlderJob   = "older-job"   // only a job from before this turn runs
	whyJobWait    = "job-wait"    // the reply says it waits, and its job has run past jobHold
	whyJobCheck   = "job-check"   // a driver heard this reply, and its job has run on
	whyJobEnded   = "job-ended"   // its job ended with the session idle at a view already told
	whyUnheard    = "unheard"     // no run answered its last wake
	whyRepost     = "repost"      // a run heard its last wake and nothing was done
	whyTimer      = "timer"
	whyWatchdog   = "watchdog"
)

// What holds a post back past when it was due, as the log records it.
const (
	heldStalled = "unheard" // the driver is not answering any wake
	heldPosting = "posting" // another post is under way
	heldGap     = "gap"     // another post has just gone, and its run comes first
	heldRun     = "run"     // a run is under way
)

// posting is what a post's record says of it.
type posting struct {
	why    string
	due    time.Time
	heldBy string
}

// wakeFacts is what decides a session's wake: where its turn stands, what its
// reply says, and what the log says of its wakes.
type wakeFacts struct {
	member *batch.Member // nil outside the batch
	turn   Turn
	view   string
	reads  turn.Reads
	since  time.Time // when the session's state last changed
	stable time.Time // when the watch asking saw it last change
	quiet  time.Duration
}

// turnNow reads where the turn a session's newest prompt started stands.
func (d *Driver) turnNow(o *Observation) Turn {
	var at time.Time
	if p := o.Frontier.Newest(); p != nil {
		at = p.At
	}
	return d.turnOf(o.Session, o.Frontier, at)
}

// factsOf gathers a session's wake facts, m being its member as the log says
// now.
func factsOf(o *Observation, t Turn, m *batch.Member, stable time.Time, quiet time.Duration) wakeFacts {
	return wakeFacts{member: m, turn: t, view: o.View.String(), reads: replyReads(o), since: o.Session.Since(), stable: stable, quiet: quiet}
}

// replyReads is what the session's newest reply says of its turn.
func replyReads(o *Observation) turn.Reads {
	if a, local := o.Frontier.Answer(); a != nil && !local {
		return turn.ReadReply(a.Text)
	}
	return turn.ReadsEnded
}

// wakePlan is what a watch on the session does.
type wakePlan struct {
	at      time.Time // when it posts; zero while its turn has not ended
	covered bool      // a post since it was armed said all there is, so it posts nothing
	held    string    // why a job holds the wake past the turn's end; "" when none does
	why     string    // what makes it post
}

// plan is when a turn that ended posts. With no job running that is after
// quiet, at once where nothing moves until someone acts, and never again for
// what a post since this watch was armed already said; a job that has ended
// since its last wake is news. With a job running the reply decides, then what
// its driver has heard of it.
func (f wakeFacts) plan() wakePlan {
	t, m := f.turn, f.member
	if !t.Over {
		return wakePlan{}
	}
	told := m != nil && m.WokenTimes > 0 && m.WokenView == f.view
	if t.State != "done" || t.Shells+t.Orphans == 0 {
		why := map[bool]string{true: whyPrompt, false: whyEnded}[t.State.onPrompt()]
		if t.State != "done" && !t.State.onPrompt() {
			why = whyStopped
		}
		if told {
			switch {
			case m.Woken.After(m.Watched) && m.WokenJobs == 0:
				return wakePlan{covered: true}
			case m.WokenJobs > 0:
				why = whyJobEnded
			case !m.WokenHeard():
				why = whyUnheard
			default:
				why = whyRepost
			}
		}
		if t.State == "done" || t.State.onPrompt() {
			return wakePlan{at: f.stable.Add(f.quiet), why: why}
		}
		return wakePlan{at: f.stable, why: why}
	}
	switch {
	case told && m.WokenAcked > 0:
		return wakePlan{at: m.Woken.Add(jobHold << min(m.WokenAcked, 8)), held: "its driver has heard its reply, and a job of its still runs", why: whyJobCheck}
	case told:
		return wakePlan{at: m.Woken.Add(repostAfter), why: whyUnheard} // a wake no run answered is no wake
	case f.sentForReply():
		return wakePlan{at: f.stable.Add(f.quiet), why: whyRecap}
	case f.reads == turn.ReadsAsking:
		return wakePlan{at: f.stable.Add(f.quiet), why: whyAsks}
	case f.reads == turn.ReadsWaiting:
		return wakePlan{at: f.since.Add(jobHold), held: "it says it waits on its own job", why: whyJobWait}
	case t.Shells == 0:
		return wakePlan{at: f.stable.Add(f.quiet), why: whyOlderJob} // only older jobs run, and this turn answered its step
	}
	return wakePlan{at: f.since.Add(jobHold), held: "a job it started still runs, and its end usually resumes the turn", why: whyJobRunning}
}

// sentForReply says the driver sent for a reply to read, which is news whatever
// it says and whatever still runs: a recap goes before a park or a close.
func (f wakeFacts) sentForReply() bool {
	if f.member == nil || f.member.LastSend == nil {
		return false
	}
	last := f.member.LastSend
	return last.Key == "recap" || strings.EqualFold(strings.TrimSpace(last.Text), "recap")
}

// stuck says a watch has run long past when it should have posted.
func (p wakePlan) stuck(now time.Time) bool {
	return !p.at.IsZero() && now.After(p.at.Add(stuckAfter))
}

// repost is what becomes of a wake nobody acted on, with no watch running.
type repost struct {
	due   string    // why it goes again now; "" when it does not
	again time.Time // when it goes again, while that is still to come
	done  bool      // it has gone twice, heard, and goes no more for this view
}

// repost decides it. A session that moved on by itself since, such as one a
// usage limit's reset continued, is woken again when that turn ends. A wake no
// run answered told no one, so it goes again however often it has gone. A wake a
// run heard and did nothing about goes once more, so one lost to a run that died
// is not lost, and never again for one view.
func (f wakeFacts) repost(now time.Time) repost {
	m := f.member
	switch {
	case f.view != "" && m.WokenView != "" && f.view != m.WokenView:
		return repost{due: "it moved on by itself since its last wake"}
	case !m.WokenHeard() && now.Sub(m.Woken) > repostAfter:
		return repost{due: "woken " + ago(now.Sub(m.Woken)) + " ago, and no driver has run since"}
	case m.WokenView == m.PrevView:
		return repost{done: true}
	case now.Sub(m.Woken) > repostAfter:
		return repost{due: "woken " + ago(now.Sub(m.Woken)) + " ago with nothing done since"}
	}
	return repost{again: m.Woken.Add(repostAfter)}
}

// noticeDue says the watchdog should tell the driver something about m:
// it has not, or it did and no run answered.
func noticeDue(m *batch.Member, what string, now time.Time) bool {
	n, ok := m.Notices[what]
	return !ok || !n.Heard && now.Sub(n.At) >= repostAfter
}

// stallRetry is how long a stalled batch waits before its next try: a
// quarter of an hour, doubling to two hours.
func stallRetry(strikes int) time.Duration {
	return min(15*time.Minute<<min(max(strikes-stallAfter, 0), 3), 2*time.Hour)
}

// stalled says when a batch's next post may go while its driver is not
// answering; zero while it is.
func stalled(h batch.Hearing) time.Time {
	if h.Strikes < stallAfter {
		return time.Time{}
	}
	return h.Last.Add(stallRetry(h.Strikes))
}

// wakeHeld says what holds a session's wake, ready since ready, "" when nothing
// does: the driver not answering any wake, another post under way, a post that
// has yet to have its run, and a run under way, up to mostHeld. Nothing is
// dropped: the watch that holds a wake still runs and looks again.
func wakeHeld(evs []batch.Event, now, ready time.Time) string {
	if next := stalled(batch.Hearings(evs, now)); now.Before(next) {
		return heldStalled
	}
	var posted, posting, heard time.Time
	for _, e := range evs {
		switch {
		case batch.Heard(e):
			heard = e.At
		case e.Kind == batch.KindPosting:
			posting = e.At
		case e.Kind == batch.KindWake && e.Via == "":
			posting, posted = time.Time{}, e.At
		}
	}
	switch {
	case !posting.IsZero() && now.Sub(posting) < postLimit:
		return heldPosting
	case !posted.IsZero() && now.Sub(posted) < wakeGap:
		return heldGap
	case !heard.IsZero() && now.Sub(heard) < runQuiet && now.Sub(ready) < mostHeld:
		return heldRun
	}
	return ""
}

// timersHeld lists the timers due and waiting for the driver to answer, as
// status and the page show them. One that has rung, or whose watch was stopped,
// no longer waits.
func (d *Driver) timersHeld(b *batch.Batch, evs []batch.Event) []string {
	var out []string
	for i, e := range evs {
		if e.Kind != batch.KindHeld {
			continue
		}
		rang := slices.ContainsFunc(evs[i:], func(w batch.Event) bool { return w.Kind == batch.KindWake && w.Session == "" && w.Note == e.Note })
		if !rang && d.Watches != nil && d.Watches.Alive(timerName(b, e.Note)) {
			out = append(out, fmt.Sprintf("timer %q is due, and waits until the driver answers", e.Note))
		}
	}
	return out
}

// unheardLine says a batch's driver is not answering its wakes: the bot's
// usage ran out, most often. "" while it answers.
func unheardLine(h batch.Hearing, zone *time.Location) string {
	next := stalled(h)
	if next.IsZero() {
		return ""
	}
	return "the driver has not answered its last " + strconv.Itoa(h.Strikes) + " wakes, since " + h.Since.In(zone).Format("15:04") +
		": wakes wait, none is lost, and the next try is at " + next.In(zone).Format("15:04")
}
