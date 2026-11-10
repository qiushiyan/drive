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
	post    posting
}

// Carried is another session's ending, carried by a wake.
type Carried struct {
	Label string
	Block string
	State State
	View  string
	Next  string
	Jobs  int // the background jobs it still runs
	post  posting
}

// Wait blocks until a session's turn ends and has stayed ended for quiet: it
// is done with no shell this turn started still running, it waits on a
// prompt, or an API error ended it. A turn that ended while its own
// background jobs run usually resumes when they finish but may also have
// ended on a question, so after bgCap the wait returns anyway. A driver
// running this pays for no wake, so what paces a chat driver's (wakeFacts)
// stays out of it.
func (d *Driver) Wait(ref string, quiet, bgCap time.Duration) (*Ending, error) {
	return d.wait(ref, func(_ *Observation, t Turn, stable time.Time) wakePlan {
		switch {
		case t.State.stopped():
			return wakePlan{at: stable} // nothing will happen until someone acts
		case t.State.onPrompt() || t.Shells == 0:
			return wakePlan{at: stable.Add(quiet)}
		}
		return wakePlan{at: stable.Add(bgCap)}
	}, nil)
}

// wait is both: planFor says when a turn that is over ends the wait, and
// mayEnd, when set, can keep it going past that, the session being looked at
// again meanwhile, so a turn that moves on is no ending any more.
func (d *Driver) wait(ref string, planFor func(o *Observation, t Turn, stable time.Time) wakePlan, mayEnd func(*Ending) bool) (*Ending, error) {
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
			e := &Ending{Label: ref, State: Gone, Msg: r.Reason, post: posting{why: whyGone, due: d.now()},
				Next: fmt.Sprintf("%s lists what runs; if %s still runs there, watch it again: %s", d.run("status"), ref, d.run("watch", ref))}
			if m := batch.Find(members, ref); m != nil {
				e.Block = m.Block
				if gone(err) {
					e.Next = d.reopen(b, m, nil)
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
		t := d.turnNow(o)
		state := t.State
		var plan wakePlan
		ended := false
		if t.Over {
			plan = planFor(o, t, stable)
			ended = plan.covered || !d.now().Before(plan.at)
		}
		if ended {
			e := &Ending{Label: o.Label, Block: o.Session.Block, State: state, Msg: st.Msg, Error: o.Frontier.Error,
				Shells: t.Shells, Orphans: t.Orphans, Capped: t.Shells > 0, Pending: o.Frontier.Pending, View: o.View,
				Account: o.Session.Account, Jobs: d.Host.Shells(o.Session.Status.PID)}
			if a, local := o.Frontier.Answer(); a != nil {
				e.Reply = tail(answerText(o.Frontier, a, local), replyTail)
			}
			if state.blocked() {
				if screen, err := d.Term.Capture(o.Session.Block, false); err == nil {
					e.Screen = lastLines(screen, 25)
				}
			}
			d.noteLimit(b, o)
			e.Next = d.nextFor(o, state, e.Jobs)
			e.Covered, e.post = plan.covered, posting{why: plan.why, due: plan.at}
			if plan.covered || mayEnd == nil || mayEnd(e) {
				return e, nil
			}
		}
		d.sleep(5 * d.poll() / 2)
	}
}

// nextFor is what a driver runs about a session whose turn ended in state,
// where the ending decides it: the one answer the wake and drive read give.
// jobs are the background jobs the session runs.
func (d *Driver) nextFor(o *Observation, state State, jobs []session.Shell) string {
	switch {
	case state == BlockedAuth:
		return d.relaunchHint(o)
	case o.Frontier.Error != nil && o.Frontier.Error.Limit:
		return d.limitNext(o, lastStep(o.in, o.Label))
	case state.onPrompt():
		return d.answerNext(o.Label, o.View.String(), state)
	case state == Errored && o.Frontier.Error != nil && o.Frontier.Error.Kind == "rate_limit":
		back := "read it again after the reset to see it under way"
		if webhook, _ := d.wakes(o); webhook {
			back = d.run("watch", "--due") + " arms its wake once it is under way again"
		}
		return fmt.Sprintf("if its screen says it continues automatically once the limit resets (%s), leave it: the turn resumes by itself, and %s; otherwise %s",
			d.run("read", o.Label, "--screen"), back, d.retryHint(o, "rate_limit"))
	case state == Errored:
		kind := ""
		if o.Frontier.Error != nil {
			kind = o.Frontier.Error.Kind
		}
		return d.retryHint(o, kind)
	case state == Interrupted:
		return fmt.Sprintf("its turn was interrupted; read where it stopped (%s) before its next step", d.run("read", o.Label))
	case state == Done && len(jobs) > 0 && replyReads(o) == turn.ReadsAsking:
		return fmt.Sprintf("its reply asks something, whatever its job does: answer with its next step (%s); if the job should have ended, have the session stop it: %s",
			d.run("send", o.View.String(), text("…")), d.stopJob(o.View.String(), jobs[0]))
	case state == Done && len(jobs) > 0:
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
		Done: "its turn ended", BlockedPermission: "it waits on a permission prompt",
		BlockedQuestion: "it waits on a question", BlockedAuth: "its login was rejected",
		Errored: "an API error ended its turn", Gone: "its session is gone",
		Interrupted: "its turn was interrupted",
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
	if e.Msg != "" && e.State == Gone {
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
		fmt.Fprintf(&b, "also ended, in this one wake: %s (%s), whose reply is not shown here: %s; view: %s", c.Label, rex.Short(c.Block), c.State, c.View)
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
