package drive

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// Responded reports an answer to a prompt.
type Responded struct {
	Label   string
	Action  string
	Call    string // the tool call the prompt asked about
	After   State  // the state the session reached
	Watched bool   // its wake is armed
}

// Respond answers what a session waits on, decided on view: approve or deny
// a permission prompt (or a held message), interrupt a working turn, or
// clear text left in the input box.
func (d *Driver) Respond(view, response string) (res *Responded, err error) {
	v, err := turn.ParseView(view)
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	doing := map[string]string{"approve": "approving %s's prompt", "deny": "denying %s's prompt", "interrupt": "interrupting %s",
		"clear": "clearing %s's input box"}[response]
	if doing == "" {
		return nil, &Refused{Reason: fmt.Sprintf("unknown response %q: approve, deny, interrupt or clear", response)}
	}
	again := d.run("respond", view, response)
	b, err := d.inBatch()
	if err != nil {
		return nil, unsent(err, again)
	}
	a, err := d.onView(b, v, func(label string) string { return fmt.Sprintf(doing, label) }, response == "interrupt", again)
	if err != nil {
		return nil, err
	}
	defer func() {
		if watched := a.finish(&err); res != nil {
			res.Watched = watched
		}
	}()
	o := a.o
	before := o.State()
	block := o.Session.Block
	call := ""
	if p := o.Frontier.Pending; p != nil {
		call = p.Name + ": " + clip(p.Input, 300)
	}
	var keys []string
	switch response {
	case "approve", "deny":
		if !before.onPrompt() {
			return nil, &Refused{Reason: fmt.Sprintf("%s is %s, not waiting on a prompt", o.Label, before)}
		}
		keys = []string{map[string]string{"approve": "enter", "deny": "escape"}[response]}
	case "interrupt":
		if before != "working" {
			r := &Refused{Reason: fmt.Sprintf("%s is %s; there is no turn to interrupt", o.Label, before)}
			if jobs := d.Host.Shells(o.Session.Status.PID); len(jobs) > 0 {
				r.Reason += fmt.Sprintf(", but a background job of its runs: %s", clip(jobs[0].Command, 120))
				r.Next = "an interrupt stops a turn, not a job; if the job should have ended, have the session stop it: " + d.stopJob(o.View.String(), jobs[0])
			}
			return nil, r
		}
		keys = []string{"ctrl+c"} // once: a second press quits Claude Code
	case "clear":
		if before.onPrompt() {
			// A prompt draws its options behind the box's ❯, which reads as typed text.
			return nil, &Refused{Reason: fmt.Sprintf("%s waits on a %s prompt: what reads as typed text is the prompt's options", o.Label, before.promptKind()),
				Next: d.answerNext(o.Label, o.View.String(), before)}
		}
		if o.Input != turn.InputTyped {
			return nil, &Refused{Reason: fmt.Sprintf("%s's input box holds nothing typed", o.Label)}
		}
	}
	var uncleared *Failed
	if response == "clear" {
		kind, typed := turn.InputUnknown, ""
		for i := 0; i < 5 && kind != turn.InputEmpty && kind != turn.InputSuggestion; i++ {
			if err := a.press("ctrl+u"); err != nil && i == 0 {
				return nil, unsent(err, again)
			} else if err != nil {
				return nil, &Failed{What: fmt.Sprintf("pressed ctrl+u in %s %d times, then Rex failed: %v", o.Label, i, err),
					Next: "look at its box: " + d.run("read", o.Label, "--screen")}
			}
			d.sleep(d.poll() / 4)
			if screen, err := d.Term.Capture(block, true); err == nil {
				kind, typed = turn.InputBox(screen)
			}
		}
		// Cleared only once the box is seen empty.
		if kind != turn.InputEmpty && kind != turn.InputSuggestion {
			what := "its screen shows no input box"
			if kind == turn.InputTyped {
				what = fmt.Sprintf("its input box still holds %q", clip(typed, 80))
			}
			uncleared = &Failed{What: fmt.Sprintf("pressed ctrl+u in %s and %s", o.Label, what),
				Next: "look at its screen: " + d.run("read", o.Label, "--screen")}
		}
	} else if err := a.press(keys...); err != nil {
		return nil, unsent(err, again) // its one key, which Rex did not press
	}
	// Rex hears nothing of a deny or an interrupt, so the session's own record
	// says whether it moved: its turn state, or a new entry in its transcript.
	out := &Responded{Label: o.Label, Action: response, Call: call, After: before}
	moved := response == "clear"
	for end := d.now().Add(10 * time.Second); !moved && d.now().Before(end); {
		d.sleep(d.poll() / 2)
		s, err := session.Locate(d.Term, d.Host, block)
		if err != nil {
			continue
		}
		f, err := s.Frontier()
		if err != nil {
			continue
		}
		out.After = turnState(s, f)
		moved = out.After != before || turn.ViewOf(o.Label, s.ID, f) != o.View
	}
	if b != nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindRespond, Session: o.Label, Action: response, Call: call, View: v.String(), Outcome: string(out.After)})
	}
	if uncleared != nil {
		return out, uncleared
	}
	if !moved {
		f := &Failed{What: fmt.Sprintf("pressed %s in %s, and it still reads %s with nothing new in its transcript", strings.Join(keys, " "), o.Label, out.After),
			Next: "look at its screen: " + d.run("read", o.Label, "--screen")}
		if response == "approve" {
			f.What += "; an approved command that runs long leaves it so until the command ends"
			f.Next = fmt.Sprintf("approving again does nothing; %s tells when it moves on", d.run("wait", o.Label))
			if webhook, _ := d.wakes(block); webhook {
				f.Next = "approving again does nothing; its wake comes when it moves on"
			}
		}
		return out, f
	}
	return out, nil
}

// Park stops waking the driver for a session and records why: it waits on
// something only the owner can give. It is decided on a view like any
// action, so a park decided before another driver's send never silences
// the wake that send armed.
func (d *Driver) Park(view, reason string) (err error) {
	v, err := turn.ParseView(view)
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	if strings.TrimSpace(reason) == "" {
		return &Refused{Reason: "say why it is parked; the report and the next driver read it"}
	}
	again := d.run("park", view, text(reason))
	b, err := d.inBatch()
	if err != nil {
		return unsent(err, again)
	}
	a, err := d.onView(b, v, func(label string) string { return "parking " + label }, true, again)
	if err != nil {
		return err
	}
	defer a.finish(&err)
	o := a.o
	if b == nil || o.Member == nil {
		return &Refused{Reason: fmt.Sprintf("%s is not in the batch", o.Label)}
	}
	if d.Watches != nil {
		_ = d.Watches.Stop(watchName(b, o.Session.Block))
	}
	if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindPark, Session: o.Label, Reason: reason, View: v.String()}); err != nil {
		return &Failed{What: fmt.Sprintf("stopped %s's wake, but the park was not recorded: %v", o.Label, err), Next: "park it again: " + again}
	}
	return nil
}

// Close closes a session's block once its last step has run, decided on
// view: refused while it works, waits on a prompt, runs a background job,
// or holds typed text.
func (d *Driver) Close(view string) (err error) {
	v, err := turn.ParseView(view)
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	again := d.run("close", view)
	b, err := d.inBatch()
	if err != nil {
		return unsent(err, again)
	}
	a, err := d.onView(b, v, func(label string) string { return "closing " + label }, false, again)
	if err != nil {
		return err
	}
	defer a.finish(&err)
	o := a.o
	st, state := o.Session.Status, o.State()
	// A session left open is left untouched, so no wake of its own comes:
	// one that waits on something is parked with why.
	park := func(why string) string { return d.run("park", view, text(why)) }
	switch {
	case state == "working":
		return &Refused{Reason: fmt.Sprintf("%s is working", o.Label),
			Next: d.whenTurnEnds(o.Label, o.Session.Block) + "; close it then, on the view that comes with it"}
	case state.onPrompt():
		return &Refused{Reason: fmt.Sprintf("%s waits on a %s prompt: its turn has not ended", o.Label, state.promptKind()),
			Next: d.answerNext(o.Label, view, state)}
	case o.Input == turn.InputTyped:
		return &Refused{Reason: fmt.Sprintf("%s's input box holds an unsent line: %q", o.Label, clip(o.Typed, 80)),
			Next: "leave the block, as the line may be the owner's, and park it so the report names it: " + park("an unsent line in its input box")}
	case len(d.Host.Shells(st.PID)) > 0:
		job := d.Host.Shells(st.PID)[0]
		next := "if the job should have ended, have the session stop it, and close it once that turn ends: " + d.stopJob(view, job) +
			"; if it should run on, park the session with why, as nothing wakes you when the job ends: " + park("a background job still runs")
		if webhook, _ := d.wakes(o.Session.Block); !webhook {
			next = "if the job should have ended, have the session stop it, and close it once that turn ends: " + d.stopJob(view, job) +
				fmt.Sprintf("; if it should run on, wait for it (%s) and close the session then", d.run("wait", o.Label))
		}
		return &Refused{Reason: fmt.Sprintf("%s still runs a background job: %s", o.Label, clip(job.Command, 120)), Next: next}
	}
	if err := d.Term.Close(o.Session.Block); err != nil {
		return &Refused{Reason: fmt.Sprintf("Rex did not close %s's block: %v", o.Label, err), Next: "try it again: " + d.run("close", view)}
	}
	if d.Watches != nil && b != nil {
		_ = d.Watches.Stop(watchName(b, o.Session.Block))
	}
	if b != nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindClose, Session: o.Label, Block: o.Session.Block, View: v.String()})
	}
	return nil
}
