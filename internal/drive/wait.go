package drive

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// Ending is how a session's turn ended.
type Ending struct {
	Label   string
	Block   string
	State   string // done, blocked:permission, blocked:question, blocked:auth, error, gone
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
}

// Wait blocks until a session's turn ends and has stayed ended for quiet: it
// is done with no shell this turn started still running, it waits on a
// prompt, or an API error ended it. A turn that ended while its own
// background jobs run usually resumes when they finish but may also have
// ended on a question, so after bgCap the wait returns anyway.
func (d *Driver) Wait(ref string, quiet, bgCap time.Duration) (*Ending, error) {
	_, _, members, err := d.members()
	if err != nil {
		return nil, err
	}
	var last string
	stable := d.now()
	misses := 0
	for {
		o, err := d.observe(members, ref)
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
			return &Ending{Label: ref, State: "gone", Msg: r.Reason,
				Next: fmt.Sprintf("%s lists what runs; if %s still runs there, watch it again: %s", d.run("status"), ref, d.run("watch", ref))}, nil
		}
		misses = 0
		st := o.Session.Status
		key := o.State() + o.Session.Since().String()
		if key != last {
			last, stable = key, d.now()
		}
		held := d.now().Sub(stable)
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
		case t.waiting() || t.Shells == 0:
			ended = held >= quiet
		default:
			ended = held >= bgCap
		}
		if ended {
			e := &Ending{Label: o.Label, Block: o.Session.Block, State: state, Msg: st.Msg, Error: o.Frontier.Error,
				Shells: t.Shells, Orphans: t.Orphans, Capped: t.Shells > 0, Pending: o.Frontier.Pending, View: o.View,
				Account: o.Session.Account, Jobs: d.Host.Shells(o.Session.Status.PID)}
			if a, local := o.Frontier.Answer(); a != nil {
				e.Reply = tail(answerText(o.Frontier, a, local), 2500)
			}
			if strings.HasPrefix(state, "blocked") {
				if screen, err := d.Term.Capture(o.Session.Block, false); err == nil {
					e.Screen = lastLines(screen, 25)
				}
			}
			e.Next = d.nextFor(o, state, e.Jobs)
			return e, nil
		}
		d.sleep(5 * d.poll() / 2)
	}
}

// nextFor is what a driver runs about a session whose turn ended in state,
// where the ending decides it: the one answer the wake and drive read give.
// jobs are the background jobs the session runs.
func (d *Driver) nextFor(o *Observation, state string, jobs []session.Shell) string {
	switch {
	case state == "blocked:auth":
		return d.relaunchHint(o)
	case strings.HasPrefix(state, "blocked"):
		return d.answerNext(o.Label, o.View.String(), state)
	case state == "error" && o.Frontier.Error != nil && o.Frontier.Error.Kind == "rate_limit":
		back := "read it again after the reset to see it under way"
		if webhook, _ := d.wakes(o.Session.Block); webhook {
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
		again := "watch it again (" + d.run("watch", o.Label) + ")"
		if webhook, _ := d.wakes(o.Session.Block); !webhook {
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
	what := map[string]string{
		"done": "its turn ended", "blocked:permission": "it waits on a permission prompt",
		"blocked:question": "it waits on a question", "blocked:auth": "its login was rejected",
		"error": "an API error ended its turn", "gone": "its session is gone",
		"interrupted": "its turn was interrupted",
	}[e.State]
	if what == "" {
		what = e.State
	}
	fmt.Fprintf(&b, "%s (%s): %s\n", e.Label, rex.Short(e.Block), what)
	if e.Error != nil {
		fmt.Fprintf(&b, "error: %s — %q (account %s)\n", e.Error.Kind, e.Error.Text, e.Account)
	}
	for _, j := range e.Jobs {
		fmt.Fprintf(&b, "a background job of its runs: %s\n", clip(j.Command, 100))
	}
	if e.Pending != nil && strings.HasPrefix(e.State, "blocked") {
		fmt.Fprintf(&b, "pending %s: %s\n", e.Pending.Name, clip(e.Pending.Input, 400))
	}
	if e.Msg != "" && strings.HasPrefix(e.State, "blocked") {
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
	if e.View.Session != "" {
		fmt.Fprintf(&b, "view: %s\n", e.View)
	}
	if e.Next != "" {
		fmt.Fprintf(&b, "next: %s\n", e.Next)
	}
	return b.String()
}
