package drive

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/turn"
)

// Responded reports an answer to a prompt.
type Responded struct {
	Label  string
	Action string
	Call   string // the tool call the prompt asked about
	After  string // the state the session reached
}

// Respond answers what a session waits on, decided on view: approve or deny
// a permission prompt (or a held message), interrupt a working turn, or
// clear text left in the input box.
func (d *Driver) Respond(view, action string) (*Responded, error) {
	v, err := turn.ParseView(view)
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	b, evs, members, err := d.members()
	if err != nil {
		return nil, err
	}
	o, err := d.observe(members, v.Session)
	if err != nil {
		return nil, err
	}
	_ = evs
	if err := checkView(o, v, action == "interrupt"); err != nil {
		return nil, d.refuse(b, o, err)
	}
	st := o.Session.Status
	block := o.Session.Block
	call := ""
	if p := o.Frontier.Pending; p != nil {
		call = p.Name + ": " + clip(p.Input, 300)
	}
	var keys []string
	switch action {
	case "approve", "deny":
		if st.State != "blocked" || st.Kind == "auth" {
			return nil, d.refuse(b, o, &Refused{Reason: fmt.Sprintf("%s is %s, not waiting on a prompt", o.Label, o.State())})
		}
		keys = []string{map[string]string{"approve": "enter", "deny": "escape"}[action]}
	case "interrupt":
		if st.State != "working" {
			return nil, d.refuse(b, o, &Refused{Reason: fmt.Sprintf("%s is %s; there is no turn to interrupt", o.Label, o.State())})
		}
		keys = []string{"ctrl+c"} // once: a second press quits Claude Code
	case "clear":
		if o.Input != turn.InputTyped {
			return nil, d.refuse(b, o, &Refused{Reason: fmt.Sprintf("%s's input box holds nothing typed", o.Label)})
		}
	default:
		return nil, &Refused{Reason: fmt.Sprintf("unknown response %q: approve, deny, interrupt or clear", action)}
	}
	if action == "clear" {
		for i := 0; i < 5; i++ {
			if err := d.Term.Key(block, "ctrl+u"); err != nil {
				return nil, err
			}
			d.sleep(d.poll() / 4)
			if screen, err := d.Term.Capture(block, true); err == nil {
				if k, _ := turn.InputBox(screen); k != turn.InputTyped {
					break
				}
			}
		}
	} else if err := d.Term.Key(block, keys...); err != nil {
		return nil, err
	}
	out := &Responded{Label: o.Label, Action: action, Call: call}
	for end := d.now().Add(10 * time.Second); d.now().Before(end); {
		d.sleep(d.poll() / 2)
		if now, err := d.Term.Status(block); err == nil && now != nil {
			out.After = now.State
			if now.State != st.State || !now.UpdatedAt.Equal(st.UpdatedAt) || action == "clear" {
				break
			}
		}
	}
	if b != nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindRespond, Session: o.Label, Action: action, Call: call, View: v.String(), Outcome: out.After})
	}
	if out.After == st.State && action != "clear" {
		return out, &Failed{What: fmt.Sprintf("pressed %s in %s, and it still reads %s", strings.Join(keys, " "), o.Label, out.After),
			Next: fmt.Sprintf("look at its screen: rex capture -b %s", o.Session.Block)}
	}
	return out, nil
}

// Park stops waking the driver for a session and records why: it waits on
// something only the owner can give.
func (d *Driver) Park(ref, reason string) error {
	b, err := d.Batch()
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	if strings.TrimSpace(reason) == "" {
		return &Refused{Reason: "say why it is parked; the report and the next driver read it"}
	}
	evs, err := b.Events()
	if err != nil {
		return err
	}
	m := batch.Find(batch.Members(evs), ref)
	if m == nil {
		return &Refused{Reason: fmt.Sprintf("%s is not in batch %s", ref, b.Meta.Name)}
	}
	if d.Watches != nil {
		_ = d.Watches.Stop(watchName(m.Block))
	}
	return b.Append(batch.Event{At: d.now(), Kind: batch.KindPark, Session: m.Label, Reason: reason})
}

// Close closes a session's block once its last step has run, decided on
// view: refused while it works, runs a background shell, or holds typed text.
func (d *Driver) Close(view string) error {
	v, err := turn.ParseView(view)
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	b, _, members, err := d.members()
	if err != nil {
		return err
	}
	o, err := d.observe(members, v.Session)
	if err != nil {
		return err
	}
	if err := checkView(o, v, false); err != nil {
		return d.refuse(b, o, err)
	}
	st := o.Session.Status
	switch {
	case st.State == "working" && o.Frontier.Error == nil:
		return d.refuse(b, o, &Refused{Reason: fmt.Sprintf("%s is working", o.Label), Next: "close it once its turn ends"})
	case o.Input == turn.InputTyped:
		return d.refuse(b, o, &Refused{Reason: fmt.Sprintf("%s's input box holds an unsent line: %q", o.Label, clip(o.Typed, 80)),
			Next: "leave the block and name it in the report; it may be the owner's"})
	case len(d.Host.Shells(st.PID)) > 0:
		return d.refuse(b, o, &Refused{Reason: fmt.Sprintf("%s still runs a background shell", o.Label),
			Next: "leave the block and name it in the report, or wait for the job"})
	}
	if d.Watches != nil {
		_ = d.Watches.Stop(watchName(o.Session.Block))
	}
	if err := d.Term.Close(o.Session.Block); err != nil {
		return err
	}
	if b != nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindClose, Session: o.Label, Block: o.Session.Block, View: v.String()})
	}
	return nil
}
