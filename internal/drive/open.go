package drive

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/shell"
	"github.com/qiushiyan/drive/internal/turn"
)

// OpenOptions shape a new session.
type OpenOptions struct {
	Dir     string
	Resume  string // a session id to resume instead of starting fresh
	Account string // run on this account (x-<account>); "" lets the launcher choose
	Model   string // such as "fable", for a fresh judge
	Hold    string
	Goal    *string // what the session sets out to do, for the owner; nil leaves the label's goal as it was
}

// Opened reports a new session.
type Opened struct {
	Label   string
	Block   string
	Account string
	Session string // its session id
	Sent    *Sent  // the first step, when one was given
}

// Open starts a session in a new block under label and sends it its first
// step, the notice ahead of it. A label already open in the batch is refused,
// so two drivers deciding at once open one session, not two. The step and
// the hold are checked before anything opens: a refusal means no block was
// opened, and anything that goes wrong after one has is a failure saying so.
func (d *Driver) Open(label string, step Step, opt OpenOptions) (out *Opened, err error) {
	b, err := d.Batch()
	if err != nil {
		return nil, err
	}
	if label == "" || strings.ContainsAny(label, " @/") {
		return nil, &Refused{Reason: fmt.Sprintf("label %q: use a short name without spaces, '@' or '/', such as T1", label)}
	}
	var goal string
	if opt.Goal != nil {
		if goal, err = goalText(*opt.Goal); err != nil {
			return nil, err
		}
		opt.Goal = &goal // as recorded, which a next: carries
	}
	if opt.Account == "primary" && d.Cfg.PrimaryName == "" {
		return nil, &Refused{Reason: "the primary account's launcher is not known: its name is the owner's shell's",
			Next: "open on another account, or tell the owner to set primary_name in ~/.config/drive/config.toml"}
	}
	release, err := d.own(b, label, "open:"+label, "opening "+label)
	if err != nil {
		return nil, d.refuse(b, label, err)
	}
	defer release()
	// A refusal means no block was opened, and is recorded; once one is
	// open, whatever stops the first step is a failure that says so, never a
	// refusal, which would mean nothing was done.
	var block string
	defer func() {
		r, ok := errors.AsType[*Refused](err)
		switch {
		case !ok:
		case block == "":
			err = d.refuse(b, label, err)
		default:
			err = &Failed{What: fmt.Sprintf("opened %s in block %s, but its first step was not sent: %s", label, rex.Short(block), r.Reason), Next: r.Next}
		}
	}()
	again := d.openCommand(label, opt, Step{})
	whole := d.openCommand(label, opt, step) // the open as it was asked for, its step on it
	if !step.empty() {
		// The account is the launcher's choice, so skills are read from the default one.
		if _, err := d.plan(opt.Dir, filepath.Join(d.Cfg.Home, ".claude"), again, step, !d.readNotice(b, opt.Resume)); err != nil {
			return nil, err
		}
	}
	intent := newIntent()
	err = b.Locked(func(evs []batch.Event) error {
		if err := d.settleOpen(b, evs, label); err != nil {
			return err
		}
		if evs, err = b.Events(); err != nil {
			return err
		}
		if m := batch.Find(batch.Members(evs), label); m != nil && !m.Closed {
			if st, err := d.Term.Status(m.Block); err == nil && st != nil {
				return &Refused{Reason: fmt.Sprintf("%s is already open as block %s (since %s)", label, rex.Short(m.Block), m.Joined.In(b.Zone()).Format("15:04")),
					Next: "send to it instead: " + d.run("read", label)}
			}
		}
		if opt.Hold != "" {
			if h, held := d.holder(evs, batch.Members(evs), opt.Hold, label); held {
				return &holdRefused{Refused{Reason: fmt.Sprintf("%s is held by %s since %s, %s", opt.Hold, h.Session, h.Since.In(b.Zone()).Format("15:04"), h.why),
					Next: d.holdNext(b, h, label, "", whole, true)}, opt.Hold, step, whole}
			}
		}
		// The goal rides on the intent, and stands once the block opens.
		return b.Append(batch.Event{At: d.now(), Kind: batch.KindOpenIntent, Session: label, Intent: intent, Dir: opt.Dir, Text: clip(stepSummary(step), 200), Goal: goal})
	})
	if err != nil {
		return nil, unsent(err, whole)
	}

	block, err = d.Term.Run(rex.Open{Session: b.Meta.RexSession, Dir: opt.Dir, Label: label, Command: d.launch(opt)})
	if err != nil {
		f := &Failed{What: fmt.Sprintf("Rex did not open a block for %s: %v", label, err),
			Next: d.run("status") + " lists the blocks; if none opened for it, try it again: " + whole}
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Intent: intent, Reason: f.What})
		return nil, f
	}
	// On record at once: a driver that ends while Claude Code starts leaves the block known.
	if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindOpen, Session: label, Intent: intent, Block: block, Dir: opt.Dir, Outcome: "starting"}); err != nil {
		return nil, d.unrecorded(label, block, err)
	}
	s, err := d.awaitSession(block, 90*time.Second)
	if err != nil {
		screen, _ := d.Term.Capture(block, false)
		f := &Failed{What: fmt.Sprintf("Claude Code did not start in block %s within 90 s; its screen ends:\n%s", rex.Short(block), tail(strings.TrimSpace(screen), 600)),
			// drive closes only a block with Claude Code in it; this one has none.
			Next: fmt.Sprintf("close the block once you have read why: rex block close %s", block)}
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Block: block, Reason: f.What})
		return nil, f
	}
	if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindOpen, Session: label, Block: block,
		SID: s.ID, Account: s.Account, Dir: s.Dir, Ref: opt.Resume}); err != nil {
		return nil, d.unrecorded(label, block, err)
	}
	out = &Opened{Label: label, Block: block, Account: s.Account, Session: s.ID}
	a, err := d.take(b, label, block, fmt.Sprintf("sending %s its first step", label))
	if err != nil {
		return out, err
	}
	a.touched = true // a session drive started is watched like one it typed into
	defer func() {
		if watched := a.finish(&err); out.Sent != nil {
			out.Sent.Watched = watched
		}
	}()
	if step.empty() {
		return out, nil
	}
	f, _ := s.Frontier()
	a.o = &Observation{Label: label, Session: s, Frontier: f, View: turn.ViewOf(label, s.ID, f)}
	// A resumed session read the notice in the conversation it resumes.
	p, err := d.plan(s.Dir, s.Config, d.run("send", a.o.View.String()), step, !d.readNotice(b, opt.Resume))
	if err != nil {
		return out, err
	}
	out.Sent, err = a.send(p, SendOptions{Hold: opt.Hold}, a.o.View.String())
	return out, err
}

// unrecorded is a block Rex opened that the batch's log did not take.
func (d *Driver) unrecorded(label, block string, err error) *Failed {
	return &Failed{What: fmt.Sprintf("Rex opened block %s for %s, but the batch's log did not take it: %v", rex.Short(block), label, err),
		Next: d.run("status") + " lists it among the sessions outside the batch; close it there if it is not wanted: rex block close " + block}
}

// launch is the command a new block runs: the owner's launcher in a login
// zsh, so its functions exist, then a shell that keeps the block open after
// Claude Code exits.
func (d *Driver) launch(opt OpenOptions) []string {
	var env []string
	for k, v := range d.Cfg.LaunchEnv {
		env = append(env, k+"="+shell.Quote(v))
	}
	sort.Strings(env)
	launcher := d.Cfg.Launcher
	switch {
	case opt.Account == "primary":
		launcher += "-" + d.Cfg.PrimaryName // the shell names the primary's launcher; Open refuses it unknown
	case opt.Account != "":
		launcher += "-" + opt.Account
	}
	cmd := strings.TrimSpace(strings.Join(env, " ") + " " + launcher)
	if opt.Model != "" {
		cmd += " --model " + shell.Quote(opt.Model)
	}
	if opt.Resume != "" {
		cmd += " --resume " + shell.Quote(opt.Resume)
	}
	return []string{"/bin/zsh", "-lic", cmd + "; exec zsh -l"}
}

// awaitSession waits for Claude Code to start in a block and register its session.
func (d *Driver) awaitSession(block string, limit time.Duration) (*session.Session, error) {
	for end := d.now().Add(limit); d.now().Before(end); d.sleep(d.poll()) {
		s, err := session.Locate(d.Term, d.Host, block)
		if err == nil && s.ID != "" && s.Status.State != "" && s.Status.State != "working" {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no session")
}

// readNotice says whether the session an open resumes read the notice in
// this batch; a session whose first step never reached it gets the notice
// with its relaunch.
func (d *Driver) readNotice(b *batch.Batch, sid string) bool {
	if sid == "" {
		return false
	}
	evs, err := b.Events()
	if err != nil {
		return false
	}
	for _, m := range batch.Members(evs) {
		if m.SID == sid && (m.Noticed || m.Resumed) {
			return true
		}
	}
	return false
}
