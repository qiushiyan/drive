package drive

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// Ending is how a session's turn ended.
type Ending struct {
	Label   string
	Block   string
	State   State  // done, blocked:permission, blocked:question, blocked:auth, error, gone
	Msg     string // what Rex was told, such as the prompt's question
	Error   *turn.APIError
	Shells  int             // background jobs started in this turn, still running
	Orphans int             // background jobs older than this turn's prompt
	Capped  bool            // the wait stopped at its limit while this turn's jobs ran
	Jobs    []session.Shell // every background job the session runs
	Reply   string          // the newest reply's end
	Pending *turn.ToolCall
	Screen  string // the screen's end, for a prompt the transcript does not show
	View    turn.View
	Account string
	Next    string // what the driver does about it, where the ending decides that
	// Waiting names the sessions waiting on a hold it held, each with the
	// send to run once the hold is free: a wake sets it.
	Waiting string
	// Also is the other sessions whose turns ended by the time this wake
	// posts: one post carries them all.
	Also []Carried
	// Covered says another session's wake already carried this ending, so
	// its watch posts nothing.
	Covered bool
	claimed bool // the post's intent is on record, so Wake records none
}

// Carried is another session's ending, carried by a wake.
type Carried struct {
	Label string
	Block string
	State State
	View  string
	Next  string
}

// jobHold is how long a turn that says it waits on its own background job
// stays quiet: such a turn resumes when the job ends, whose wake is the one
// that matters, and the longest real jobs ran about five hours. A job that
// never ends still reaches its driver, at this mark and then at ever longer
// ones, and the page and status show the wait all along.
const jobHold = 2 * time.Hour

// Wait blocks until a session's turn ends and has stayed ended for quiet: it
// is done with no background job still running, it waits on a prompt, or an
// API error ended it. A turn that ended while its jobs run usually resumes
// when they finish, so its reply decides (wakeAt): one that asks ends the wait
// after quiet, one that says it waits holds until jobHold, and anything else
// ends it after bgCap.
func (d *Driver) Wait(ref string, quiet, bgCap time.Duration) (*Ending, error) {
	return d.wait(ref, quiet, bgCap, nil)
}

// wait is Wait with a say in when an ending returns: mayEnd, when set, keeps
// the wait going past an ending it holds back, and the session is looked at
// again meanwhile, so a turn that moves on is no ending any more.
func (d *Driver) wait(ref string, quiet, bgCap time.Duration, mayEnd func(*Ending) bool) (*Ending, error) {
	b, _, members, err := d.members()
	if err != nil {
		return nil, err
	}
	var last string
	stable := d.now()
	misses := 0
	for {
		o, err := d.observe(b, members, ref)
		if errors.Is(err, rex.ErrNoServer) {
			// Rex's server is down, as while it restarts: whether the session
			// outlived it is known once one runs again, which drive never starts.
			misses = 0
			d.sleep(5 * d.poll() / 2)
			continue
		}
		if err != nil {
			// One unreadable look is often Rex busy for a moment; three in a row is a session gone.
			var r *Refused
			if !errors.As(err, &r) {
				return nil, err
			}
			if misses++; misses < 3 {
				d.sleep(d.poll())
				continue
			}
			e := &Ending{Label: ref, State: "gone", Msg: r.Reason,
				Next: fmt.Sprintf("%s lists what runs; if %s still runs there, watch it again: %s", d.run("status"), ref, d.run("watch", ref))}
			if m := batch.Find(members, ref); m != nil {
				e.Block = m.Block
				if gone(err) {
					e.Next = d.reopen(m)
				}
			}
			return e, nil
		}
		misses = 0
		st := o.Session.Status
		key := string(o.State()) + o.Session.Since().String()
		if key != last {
			last, stable = key, d.now()
		}
		var promptAt time.Time
		if p := o.Frontier.Newest(); p != nil {
			promptAt = p.At
		}
		t := d.turnOf(o.Session, o.Frontier, promptAt)
		state := t.State
		ended := false
		switch {
		case !t.Over:
		case state == "blocked:auth" || state == "error" || state == "interrupted":
			ended = true // nothing will happen until someone acts
		default:
			ended = !d.now().Before(d.wakeAt(o, t, d.wakesOf(b, o.Label), stable, quiet, bgCap))
		}
		if ended {
			e := &Ending{Label: o.Label, Block: o.Session.Block, State: state, Msg: st.Msg, Error: o.Frontier.Error,
				Shells: t.Shells, Orphans: t.Orphans, Capped: t.Shells > 0, Pending: o.Frontier.Pending, View: o.View,
				Account: o.Session.Account, Jobs: d.Host.Shells(o.Session.Status.PID)}
			if a, local := o.Frontier.Answer(); a != nil {
				e.Reply = tail(answerText(o.Frontier, a, local), 2500)
			}
			if state.blocked() {
				if screen, err := d.Term.Capture(o.Session.Block, false); err == nil {
					e.Screen = lastLines(screen, 25)
				}
			}
			e.Next = d.nextFor(o, state, e.Jobs)
			if mayEnd == nil || mayEnd(e) {
				return e, nil
			}
		}
		d.sleep(5 * d.poll() / 2)
	}
}

// wakeRecord is what the batch's log says of a session's wakes.
type wakeRecord struct {
	member *batch.Member // nil for a session outside the batch
	heard  time.Time     // when any of the batch's drivers last left a trace
}

// wakesOf reads the log afresh, since a wait outlasts what it read when it
// began; b is nil outside any batch.
func (d *Driver) wakesOf(b *batch.Batch, label string) wakeRecord {
	if b == nil {
		return wakeRecord{}
	}
	evs, err := b.Events()
	if err != nil {
		return wakeRecord{}
	}
	return wakeRecord{member: batch.Find(batch.Members(evs), label), heard: batch.Hearings(evs, d.now()).Heard}
}

// told says the driver heard of this view while jobs run: a wake came for it,
// and a driver left a trace after that. A wake no run answered told no one.
func (w wakeRecord) told(view string) bool {
	m := w.member
	return m != nil && m.WokenTimes > 0 && m.WokenView == view && w.heard.After(m.Woken)
}

// asked says the driver sent for a reply to read, which is news whatever it
// says and whatever still runs: a recap goes before a park or a close.
func (w wakeRecord) asked() bool {
	if w.member == nil || w.member.LastSend == nil {
		return false
	}
	last := w.member.LastSend
	return last.Key == "recap" || strings.EqualFold(strings.TrimSpace(last.Text), "recap")
}

// wakeAt is when a turn that is over ends the wait. With no job running that
// is after quiet, and so on a prompt. With one running it is the reply's call,
// and a view the driver has heard of waits longer each time, so a job that runs
// for hours costs at most a run or two. stable is when this wait saw the turn
// last change.
func (d *Driver) wakeAt(o *Observation, t Turn, rec wakeRecord, stable time.Time, quiet, bgCap time.Duration) time.Time {
	view := o.View.String()
	m := rec.member
	if t.State.onPrompt() || t.Shells+t.Orphans == 0 {
		return stable.Add(quiet)
	}
	if rec.told(view) {
		return m.Woken.Add(jobHold << min(m.WokenTimes, 8))
	}
	if m != nil && m.WokenTimes > 0 && m.WokenView == view {
		return m.Woken.Add(repostAfter) // a wake no run answered is no wake
	}
	if rec.asked() {
		return stable.Add(quiet)
	}
	switch replyReads(o) {
	case turn.ReadsAsking:
		return stable.Add(quiet)
	case turn.ReadsWaiting:
		return o.Session.Since().Add(max(jobHold, bgCap))
	}
	if t.Shells == 0 {
		return stable.Add(quiet) // only older jobs run, and this turn answered its step
	}
	return stable.Add(bgCap)
}

// holdsUntil is when a session's wake comes at the latest while it waits on
// its own job, as Wait holds it; zero when no such hold stands. Status and the
// page show it, since a wake held back is a wake the owner cannot otherwise
// see coming, and a watch held so long is not stuck.
func (d *Driver) holdsUntil(o *Observation, rec wakeRecord) time.Time {
	var at time.Time
	if p := o.Frontier.Newest(); p != nil {
		at = p.At
	}
	t := d.turnOf(o.Session, o.Frontier, at)
	if !t.Over || t.State != "done" || t.Shells+t.Orphans == 0 {
		return time.Time{}
	}
	if rec.told(o.View.String()) || rec.member != nil && !rec.asked() && replyReads(o) == turn.ReadsWaiting {
		return d.wakeAt(o, t, rec, o.Session.Since(), time.Minute, time.Minute)
	}
	return time.Time{}
}

// replyReads is what the session's newest reply says of its turn.
func replyReads(o *Observation) turn.Reads {
	if a, local := o.Frontier.Answer(); a != nil && !local {
		return turn.ReadReply(a.Text)
	}
	return turn.ReadsEnded
}

// nextFor is what a driver runs about a session whose turn ended in state,
// where the ending decides it: the one answer the wake and drive read give.
// jobs are the background jobs the session runs.
func (d *Driver) nextFor(o *Observation, state State, jobs []session.Shell) string {
	switch {
	case state == "blocked:auth":
		return d.relaunchHint(o)
	case state.onPrompt():
		return d.answerNext(o.Label, o.View.String(), state)
	case state == "error" && o.Frontier.Error != nil && o.Frontier.Error.Kind == "rate_limit":
		back := "read it again after the reset to see it under way"
		if webhook, _ := d.wakes(o); webhook {
			back = d.run("watch", "--due") + " arms its wake once it is under way again"
		}
		return fmt.Sprintf("if its screen says it continues automatically once the limit resets (%s), leave it: the turn resumes by itself, and %s; otherwise %s",
			d.run("read", o.Label, "--screen"), back, d.retryHint(o, "rate_limit"))
	case state == "error":
		kind := ""
		if o.Frontier.Error != nil {
			kind = o.Frontier.Error.Kind
		}
		return d.retryHint(o, kind)
	case state == "interrupted":
		return fmt.Sprintf("its turn was interrupted; read where it stopped (%s) before its next step", d.run("read", o.Label))
	case state == "done" && len(jobs) > 0:
		again := "run only " + d.run("watch", o.Label) + ", which tells drive this run heard the wake: it stays armed and comes again when that turn ends"
		if webhook, _ := d.wakes(o); !webhook {
			again = "wait for it again (" + d.run("wait", o.Label) + ")"
		}
		return fmt.Sprintf("if the reply says it waits on its job, %s; if the job should have ended, have the session stop it: %s; otherwise send its next step",
			again, d.stopJob(o.View.String(), jobs[0]))
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Text renders an ending for a reader deciding the next step.
func (e *Ending) Text() string {
	var b strings.Builder
	what := map[State]string{
		"done": "its turn ended", "blocked:permission": "it waits on a permission prompt",
		"blocked:question": "it waits on a question", "blocked:auth": "its login was rejected",
		"error": "an API error ended its turn", "gone": "its session is gone",
		"interrupted": "its turn was interrupted",
	}[e.State]
	if what == "" {
		what = string(e.State)
	}
	fmt.Fprintf(&b, "%s (%s): %s\n", e.Label, rex.Short(e.Block), what)
	if e.Error != nil {
		fmt.Fprintf(&b, "error: %s — %q (account %s)\n", e.Error.Kind, e.Error.Text, e.Account)
	}
	for _, j := range e.Jobs {
		fmt.Fprintf(&b, "a background job of its runs: %s\n", clip(j.Command, 100))
	}
	if e.Pending != nil && e.State.blocked() {
		fmt.Fprintf(&b, "pending %s: %s\n", e.Pending.Name, clip(e.Pending.Input, 400))
	}
	if e.Msg != "" && e.State.blocked() {
		fmt.Fprintf(&b, "prompt: %s\n", e.Msg)
	}
	if e.Msg != "" && e.State == "gone" {
		fmt.Fprintf(&b, "why: %s\n", e.Msg)
	}
	if e.Reply != "" {
		fmt.Fprintf(&b, "--- reply (end) ---\n%s\n", e.Reply)
	}
	if e.Screen != "" {
		fmt.Fprintf(&b, "--- screen (end) ---\n%s\n", e.Screen)
	}
	b.WriteString(e.Waiting)
	for _, c := range e.Also {
		fmt.Fprintf(&b, "also ended, in this one wake: %s (%s): %s; view: %s", c.Label, rex.Short(c.Block), c.State, c.View)
		if c.Next != "" {
			fmt.Fprintf(&b, "; next for it: %s", c.Next)
		}
		b.WriteString("\n")
	}
	if e.View.Session != "" {
		fmt.Fprintf(&b, "view: %s\n", e.View)
	}
	if e.Next != "" {
		fmt.Fprintf(&b, "next: %s\n", e.Next)
	}
	return b.String()
}
