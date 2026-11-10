package batch

import (
	"strings"
	"time"
)

// What the log says of a session's wake is three things that stand side by
// side, each with its own events: the watch armed for it, the posts made about
// it, and what its driver did since. A post's result can land after a newer
// watch was armed, and a watch that waits on a job posts and runs on, so none
// of the three replaces another. This file is the one reader of those events:
// Posts pairs each posting with its result and says which driver's trace
// answered it, WakeRecord keeps a member's three facts, and every question a
// reader has of them is a method here.

// PostLimit is how long a post can take, with its retries, before one that
// recorded no result is taken to have died with its watch.
const PostLimit = 12 * time.Minute

const watchEnded = "watch ended without a wake"

// WatchEnded is the record of a watch that ended without posting its wake,
// session being the label its wake was for and why what ended it.
func WatchEnded(at time.Time, session, why string) Event {
	return Event{At: at, Kind: KindFailure, Session: session, Reason: watchEnded + ": " + why}
}

// IsWatchEnded says e is that record.
func IsWatchEnded(e Event) bool {
	return e.Kind == KindFailure && strings.HasPrefix(e.Reason, watchEnded)
}

// Post is one attempt to wake the driver: a watch's post about its session,
// an ending another session's post carried (Via), or a timer's or the
// watchdog's (Session ""). It begins at its posting event and ends at its
// wake event; one an older drive wrote with no posting begins where it ends.
type Post struct {
	Session string
	Block   string
	Via     string // the session whose post carried it
	Why     string
	View    string
	Note    string // a timer's
	Jobs    int
	Began   time.Time
	Ended   time.Time // zero while it has no result
	HTTP    int       // the webhook's answer; 0 while it has none, or when it failed
	Reason  string    // why it failed
	// Heard is the first trace a driver left from when the post began until its
	// session's next one did: a run can act before the post that started it
	// returns. Acted is the first thing a driver did to its session in that time.
	Heard time.Time
	Acted time.Time

	lost         bool // it is over with no result on record
	recorded     bool // its posting is on record
	began, ended int  // the positions of its two events, counted from 1; ended is 0 while it has none
}

// Pending says the post has no result yet.
func (p *Post) Pending() bool { return p.ended == 0 }

// Lost says the post is over with no result on record: its session's next
// post overtook it, or the post that carried it failed.
func (p *Post) Lost() bool { return p.lost }

// Delivered says the webhook took it.
func (p *Post) Delivered() bool { return p.HTTP != 0 }

// Posts reads every post from the log, oldest first. A wake is the result of
// its session's oldest posting still without one, as a session has one watch
// and timers return in the order they rang; a posting with nothing after it
// stays pending. A post a session's next one overtakes, or whose carrier
// failed, is over with no result.
func Posts(evs []Event) []Post {
	var out []Post
	pending := map[string][]int{} // a session's posts with no result yet, oldest first
	newest := map[string]int{}    // a session's newest post, whose time to be answered is open
	for at, e := range evs {
		i := at + 1 // a position, counted from 1: 0 is no event
		if Heard(e) {
			for _, j := range newest {
				if out[j].Heard.IsZero() {
					out[j].Heard = e.At
				}
			}
		}
		if Acted(e) && e.Session != "" {
			if j, ok := newest[e.Session]; ok && out[j].Acted.IsZero() {
				out[j].Acted = e.At
			}
		}
		switch e.Kind {
		case KindPosting:
			if e.Session != "" {
				// Its watch is the session's only one, so an older post still
				// without a result will never have one.
				for _, j := range pending[e.Session] {
					out[j].Ended, out[j].ended, out[j].lost = e.At, i, true
				}
				pending[e.Session] = nil
			}
			out = append(out, Post{Session: e.Session, Block: e.Block, Via: e.Via, Why: e.Why, View: e.View, Note: e.Note,
				Began: e.At, recorded: true, began: i})
			pending[e.Session] = append(pending[e.Session], len(out)-1)
			newest[e.Session] = len(out) - 1
		case KindWake:
			var j int
			if q := pending[e.Session]; len(q) > 0 {
				j, pending[e.Session] = q[0], q[1:]
			} else {
				out = append(out, Post{Session: e.Session, Began: e.At, began: i})
				j = len(out) - 1
				newest[e.Session] = j
			}
			p := &out[j]
			p.Ended, p.ended, p.HTTP, p.Reason, p.Jobs, p.Via = e.At, i, e.HTTP, e.Reason, e.Jobs, e.Via
			p.Block, p.Why, p.View, p.Note = orOf(e.Block, p.Block), orOf(e.Why, p.Why), orOf(e.View, p.View), orOf(e.Note, p.Note)
			if e.HTTP == 0 && e.Via == "" && e.Session != "" {
				// The endings it carried went nowhere either: their own watches still run.
				for s, q := range pending {
					kept := q[:0]
					for _, k := range q {
						if out[k].Via == e.Session {
							out[k].Ended, out[k].ended, out[k].lost = e.At, i, true
						} else {
							kept = append(kept, k)
						}
					}
					pending[s] = kept
				}
			}
		}
	}
	return out
}

func orOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// WakeStanding is how a session's wake stands on the record. Whether a watch
// runs now is the process table's to say, not the log's, so a reader asks
// that first.
type WakeStanding int

const (
	// WakeNever: no watch was ever armed for it.
	WakeNever WakeStanding = iota
	// WakeNoResult: a watch was armed and the record shows no wake from it. It
	// still waits, or it ended without one: its post failed, it died, or a post
	// it began never returned.
	WakeNoResult
	// WakeStopped: its watch was stopped by watch --stop-all.
	WakeStopped
	// WakePosting: a post about it is under way.
	WakePosting
	// WakeUnhandled: a wake reached the driver, which has not acted on the
	// session since.
	WakeUnhandled
	// WakeActed: a wake reached the driver, which acted on the session since.
	WakeActed
)

// WakeRecord is what the log says of one member's wake, for the block it runs
// in now: a reopened label starts over.
type WakeRecord struct {
	armed   time.Time // when a watch was last armed
	armedAt int       // that event's position, counted from 1; 0 when none was
	stopped time.Time // when its watch was last stopped by watch --stop-all
	ended   time.Time // when a watch of its last ended without a wake
	acted   time.Time // the latest thing its driver did to it
	last    *Post     // its newest post
	told    *Post     // its newest post that reached the driver
	times   int       // how many in a row reached the driver for that view
	acked   int       // how many of those a driver left a trace after
}

// Told is the newest wake that reached the driver about a session.
type Told struct {
	At    time.Time
	View  string
	Times int // wakes posted for that view, one after another
	Acked int // how many of those a driver left a trace after
	Jobs  int // the background jobs the session still ran
	// Heard says a driver left a trace after its post began, with no post
	// about the session begun since.
	Heard bool
}

// Armed is when a watch was last armed for it; zero when none was.
func (w *WakeRecord) Armed() time.Time { return w.armed }

// Told is the newest wake that reached the driver; ok is false when none has.
func (w *WakeRecord) Told() (t Told, ok bool) {
	if w.told == nil {
		return Told{}, false
	}
	p := w.told
	return Told{At: p.Ended, View: p.View, Times: w.times, Acked: w.acked, Jobs: p.Jobs, Heard: w.last == p && !p.Heard.IsZero()}, true
}

// ToldOf is Told when that wake carried view.
func (w *WakeRecord) ToldOf(view string) (Told, bool) {
	t, ok := w.Told()
	return t, ok && t.View == view
}

// toldSinceArmed says a wake reached the driver from a post begun after the
// watch now on record was armed. A result that lands after a newer arming
// belongs to the watch before it.
func (w *WakeRecord) toldSinceArmed() bool { return w.told != nil && w.told.began > w.armedAt }

// PostedSinceArmed says the watch now armed has nothing left to say of a turn
// that wake carried.
func (w *WakeRecord) PostedSinceArmed() bool { return w.toldSinceArmed() }

// Unhandled says a wake reached the driver and nothing was done to the session
// since.
func (w *WakeRecord) Unhandled() bool { return w.told != nil && w.told.Ended.After(w.acted) }

// Standing reads how the wake stands as of now, and since when: the stop, the
// post's start, or the wake.
func (w *WakeRecord) Standing(now time.Time) (WakeStanding, time.Time) {
	if !w.stopped.IsZero() && !w.stopped.Before(w.armed) {
		return WakeStopped, w.stopped
	}
	if p := w.last; p != nil && p.Pending() && p.began > w.armedAt && p.Began.After(w.ended) {
		if now.Sub(p.Began) < PostLimit {
			return WakePosting, p.Began
		}
		return WakeNoResult, p.Began // it died posting
	}
	switch {
	case w.toldSinceArmed() && w.Unhandled():
		return WakeUnhandled, w.told.Ended
	case w.toldSinceArmed():
		return WakeActed, w.told.Ended
	case !w.armed.IsZero():
		return WakeNoResult, w.armed
	}
	return WakeNever, time.Time{}
}

// add takes the next post about the member, oldest first.
func (w *WakeRecord) add(p *Post) {
	w.last = p
	if !p.Delivered() {
		return
	}
	if w.told == nil || w.told.View != p.View {
		w.times, w.acked = 0, 0
	}
	w.told = p
	w.times++
	if !p.Heard.IsZero() {
		w.acked++
	}
}
