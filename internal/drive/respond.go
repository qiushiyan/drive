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
	After   string // the state the session reached
	Watched bool   // its wake is armed
}

// Respond answers what a session waits on, decided on view: approve or deny
// a permission prompt (or a held message), interrupt a working turn, or
// clear text left in the input box.
func (d *Driver) Respond(view, action string) (res *Responded, err error) {
	v, err := turn.ParseView(view)
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	b, l, o, err := d.ownView(v, action+" "+v.Session, action == "interrupt")
	if err != nil {
		return nil, err
	}
	defer func() {
		watched, werr := l.end()
		if res != nil {
			res.Watched = watched
		}
		if werr != nil && err == nil {
			err = wakeFailed(o.Label, werr)
		}
	}()
	before := o.State()
	block := o.Session.Block
	call := ""
	if p := o.Frontier.Pending; p != nil {
		call = p.Name + ": " + clip(p.Input, 300)
	}
	var keys []string
	switch action {
	case "approve", "deny":
		if !strings.HasPrefix(before, "blocked") || before == "blocked:auth" {
			return nil, d.refuse(b, o.Label, &Refused{Reason: fmt.Sprintf("%s is %s, not waiting on a prompt", o.Label, before)})
		}
		keys = []string{map[string]string{"approve": "enter", "deny": "escape"}[action]}
	case "interrupt":
		if before != "working" {
			return nil, d.refuse(b, o.Label, &Refused{Reason: fmt.Sprintf("%s is %s; there is no turn to interrupt", o.Label, before)})
		}
		keys = []string{"ctrl+c"} // once: a second press quits Claude Code
	case "clear":
		if strings.HasPrefix(before, "blocked") && before != "blocked:auth" {
			// A prompt draws its options behind the box's ❯, which reads as typed text.
			return nil, d.refuse(b, o.Label, &Refused{Reason: fmt.Sprintf("%s waits on a %s prompt: what reads as typed text is the prompt's options", o.Label, strings.TrimPrefix(before, "blocked:")),
				Next: answerNext(o.Label, o.View.String(), before)})
		}
		if o.Input != turn.InputTyped {
			return nil, d.refuse(b, o.Label, &Refused{Reason: fmt.Sprintf("%s's input box holds nothing typed", o.Label)})
		}
	default:
		return nil, &Refused{Reason: fmt.Sprintf("unknown response %q: approve, deny, interrupt or clear", action)}
	}
	var uncleared *Failed
	if action == "clear" {
		kind, typed := turn.InputUnknown, ""
		for i := 0; i < 5 && kind != turn.InputEmpty && kind != turn.InputSuggestion; i++ {
			if err := d.press(block, "ctrl+u"); err != nil {
				return nil, err
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
				Next: fmt.Sprintf("look at its screen: drive read %s --screen", o.Label)}
		}
	} else if err := d.press(block, keys...); err != nil {
		return nil, err
	}
	// Rex hears nothing of a deny or an interrupt, so the session's own record
	// says whether it moved: its turn state, or a new entry in its transcript.
	out := &Responded{Label: o.Label, Action: action, Call: call, After: before}
	moved := action == "clear"
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
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindRespond, Session: o.Label, Action: action, Call: call, View: v.String(), Outcome: out.After})
	}
	if uncleared != nil {
		return out, uncleared
	}
	if !moved {
		f := &Failed{What: fmt.Sprintf("pressed %s in %s, and it still reads %s with nothing new in its transcript", strings.Join(keys, " "), o.Label, out.After),
			Next: fmt.Sprintf("look at its screen: drive read %s --screen", o.Label)}
		if action == "approve" {
			f.What += "; an approved command that runs long leaves it so until the command ends"
			f.Next = fmt.Sprintf("drive wait %s tells when it does; approving again does nothing", o.Label)
		}
		return out, f
	}
	return out, nil
}

// Park stops waking the driver for a session and records why: it waits on
// something only the owner can give. It is decided on a view like any
// action, so a park decided before another driver's send never silences
// the wake that send armed.
func (d *Driver) Park(view, reason string) error {
	v, err := turn.ParseView(view)
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	if strings.TrimSpace(reason) == "" {
		return &Refused{Reason: "say why it is parked; the report and the next driver read it"}
	}
	b, l, o, err := d.ownView(v, "parking "+v.Session, true)
	if err != nil {
		return err
	}
	defer l.end()
	if b == nil || o.Member == nil {
		return &Refused{Reason: fmt.Sprintf("%s is not in the batch", o.Label)}
	}
	if d.Watches != nil {
		_ = d.Watches.Stop(watchName(b, o.Session.Block))
	}
	return b.Append(batch.Event{At: d.now(), Kind: batch.KindPark, Session: o.Label, Reason: reason, View: v.String()})
}

// ownView owns the session a view names and reads it under that ownership,
// refusing the view once the session has moved past it. promptOnly accepts a
// view whose reply has grown since, as long as no prompt came.
func (d *Driver) ownView(v turn.View, what string, promptOnly bool) (*batch.Batch, *lease, *Observation, error) {
	b, _, members, err := d.members()
	if err != nil {
		return nil, nil, nil, err
	}
	label, block, err := d.target(members, v.Session)
	if err != nil {
		return nil, nil, nil, err
	}
	l, err := d.own(label, block, what)
	if err != nil {
		return nil, nil, nil, d.refuse(b, label, err)
	}
	_, _, members, err = d.members()
	if err == nil && b != nil {
		err = d.settle(b, label, block)
	}
	var o *Observation
	if err == nil {
		o, err = d.observe(members, v.Session)
	}
	if err == nil {
		if err = checkView(o, v, promptOnly, d.zone()); err != nil {
			err = d.refuse(b, o.Label, err)
		}
	}
	if err != nil {
		l.release()
		return nil, nil, nil, err
	}
	return b, l, o, nil
}

// Close closes a session's block once its last step has run, decided on
// view: refused while it works, runs a background shell, or holds typed text.
func (d *Driver) Close(view string) error {
	v, err := turn.ParseView(view)
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	b, l, o, err := d.ownView(v, "closing "+v.Session, false)
	if err != nil {
		return err
	}
	defer l.end()
	st := o.Session.Status
	switch {
	case o.State() == "working":
		return d.refuse(b, o.Label, &Refused{Reason: fmt.Sprintf("%s is working", o.Label), Next: "close it once its turn ends"})
	case o.Input == turn.InputTyped:
		return d.refuse(b, o.Label, &Refused{Reason: fmt.Sprintf("%s's input box holds an unsent line: %q", o.Label, clip(o.Typed, 80)),
			Next: "leave the block and name it in the report; it may be the owner's"})
	case len(d.Host.Shells(st.PID)) > 0:
		return d.refuse(b, o.Label, &Refused{Reason: fmt.Sprintf("%s still runs a background shell", o.Label),
			Next: "leave the block and name it in the report, or wait for the job"})
	}
	if d.Watches != nil && b != nil {
		_ = d.Watches.Stop(watchName(b, o.Session.Block))
	}
	if err := d.Term.Close(o.Session.Block); err != nil {
		return err
	}
	if b != nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindClose, Session: o.Label, Block: o.Session.Block, View: v.String()})
	}
	return nil
}
