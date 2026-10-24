package drive

import (
	"errors"
	"fmt"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/turn"
)

// An action is one change a driver makes to a session: the batch it runs
// in, fixed for the whole run, and the session it owns from the read its
// decision rests on to the record of how it went. A second driver acting on
// the session meanwhile is refused, never queued behind the first: whatever
// it decided, it decided on what the first is changing.
type action struct {
	d       *Driver
	b       *batch.Batch // nil for a session outside any batch
	label   string
	block   string
	release func()
	o       *Observation  // the session as the action read it
	evs     []batch.Event // the batch's log as of that read
	touched bool          // the action typed or pressed into the session
}

// take owns block, which b calls label, for an action described by what.
func (d *Driver) take(b *batch.Batch, label, block, what string) (*action, error) {
	release, err := d.own(b, label, block, what)
	if err != nil {
		return nil, d.refuse(b, label, err)
	}
	return &action{d: d, b: b, label: label, block: block, release: release}, nil
}

// onView takes the session view v names in b, for an action described by
// what given the session's label, and reads it: what an earlier driver left
// is settled first, and the view is refused once the session has moved
// past it. promptOnly accepts a view whose reply has grown since, as long as
// no prompt came. again is the action's own command, which a read that
// failed before anything was done says to run again. A read that fails
// finishes the action.
func (d *Driver) onView(b *batch.Batch, v turn.View, what func(label string) string, promptOnly bool, again string) (*action, error) {
	var members []*batch.Member
	if b != nil {
		evs, err := b.Events()
		if err != nil {
			return nil, unsent(err, again)
		}
		members = batch.Members(evs)
	}
	label, block, err := d.target(members, v.Session)
	if err != nil {
		return nil, unsent(err, again)
	}
	a, err := d.take(b, label, block, what(label))
	if err != nil {
		return nil, err
	}
	if err := a.read(v, promptOnly); err != nil {
		err = unsent(err, again)
		a.finish(&err)
		return nil, err
	}
	return a, nil
}

// read settles what an earlier driver of the session left, since settling
// may record what it sent, then reads the session and checks the view.
func (a *action) read(v turn.View, promptOnly bool) error {
	var members []*batch.Member
	if a.b != nil {
		if err := a.d.settle(a.b, a.label, a.block); err != nil {
			return err
		}
		var err error
		if a.evs, err = a.b.Events(); err != nil {
			return err
		}
		members = batch.Members(a.evs)
	}
	o, err := a.d.observe(a.b, members, v.Session)
	if err != nil {
		return err
	}
	a.o = o
	return a.d.checkView(o, v, promptOnly, a.evs)
}

// typeInto and press are how an action reaches the session's keyboard;
// finish arms the wake of whatever they reached. What Rex did not take
// touched nothing.
func (a *action) typeInto(text string) error {
	if err := a.d.Term.Type(a.block, text); err != nil {
		return err
	}
	a.touched = true
	return nil
}

func (a *action) press(keys ...string) error {
	if err := a.d.Term.Key(a.block, keys...); err != nil {
		return err
	}
	a.touched = true
	return nil
}

// finish ends the action. A refusal it ends on is recorded once, while the
// session is still owned, so the log has it before another driver acts.
// Then the session is released, and in a batch that wakes its driver the
// wake of a session the action touched is armed first, whatever came of it:
// a turn it started, a prompt it answered, a line it lost, each ends in a
// wake. A wake that did not start is the action's failure when nothing else
// was. It says whether the session is watched.
func (a *action) finish(err *error) (watched bool) {
	defer a.release()
	label := a.label
	if a.o != nil {
		label = a.o.Label
	}
	*err = a.d.refuse(a.b, label, *err)
	if !a.touched || a.b == nil || a.b.Meta.Wake != "webhook" {
		return false
	}
	evs, werr := a.b.Events()
	if werr == nil {
		m := batch.Find(batch.Members(evs), a.block)
		if m == nil {
			return false // not the batch's to wake for
		}
		label, werr = m.Label, a.d.arm(a.b, m.Label, m.Block)
	}
	if werr != nil {
		if *err == nil {
			*err = a.d.wakeFailed(label, werr)
		}
		return false
	}
	return true
}

// refuse records a refusal in the batch, a hold's with the step it refused
// so the holder's wake can name it, and returns it; any other error passes
// through unrecorded.
func (d *Driver) refuse(b *batch.Batch, label string, err error) error {
	if b == nil {
		return err
	}
	if h, ok := errors.AsType[*holdRefused](err); ok {
		e := batch.Event{At: d.now(), Kind: batch.KindRefused, Session: label, Reason: h.Reason, Hold: h.hold,
			Key: h.step.Key, Text: h.step.Line, Path: h.step.File}
		if h.step.Key != "" {
			e.Text = h.step.Text
		}
		if h.open != "" {
			e.Mode, e.Key, e.Text, e.Path = "open", "", h.open, ""
		}
		_ = b.Append(e)
		return &h.Refused
	}
	if r, ok := errors.AsType[*Refused](err); ok {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindRefused, Session: label, Reason: r.Reason, Account: r.account})
	}
	return err
}

// unsent is an error met before anything reached the session, such as a
// transcript or the batch's log that could not be read, as the refusal it
// is: nothing was done, so the action can be tried again as it was.
func unsent(err error, again string) error {
	var r *Refused
	var f *Failed
	if err == nil || errors.As(err, &r) || errors.As(err, &f) {
		return err
	}
	return &Refused{Reason: err.Error(), Next: "nothing was done; try it again: " + again}
}

// wakeFailed is an action that did what it was asked whose wake did not start.
func (d *Driver) wakeFailed(label string, err error) *Failed {
	f := &Failed{What: fmt.Sprintf("%s's wake did not start: %v", label, err), Next: "start it: " + d.run("watch", label)}
	if nw, ok := errors.AsType[*noWebhook](err); ok {
		f.Next = nw.next(d.run("watch", label))
	}
	return f
}
