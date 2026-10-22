package drive

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// OpenOptions shape a new session.
type OpenOptions struct {
	Dir     string
	Resume  string // a session id to resume instead of starting fresh
	Account string // run on this account (x-<account>); "" lets the launcher choose
	Model   string // such as "fable", for a fresh judge
	Hold    string
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
// so two drivers deciding at once open one session, not two. The step is
// checked before anything opens: a refusal means no block was opened.
func (d *Driver) Open(label string, step Step, opt OpenOptions) (out *Opened, err error) {
	b, err := d.Batch()
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	if label == "" || strings.ContainsAny(label, " @/") {
		return nil, &Refused{Reason: fmt.Sprintf("label %q: use a short name without spaces, '@' or '/', such as T1", label)}
	}
	opening, err := d.own(label, "open:"+label, "opening "+label)
	if err != nil {
		return nil, d.refuse(b, label, err)
	}
	defer opening.release()
	if !step.empty() {
		// The account is the launcher's choice, so skills are read from the default one.
		if _, err := d.plan(opt.Dir, filepath.Join(d.Cfg.Home, ".claude"), turn.View{Session: label}, step, opt.Resume == ""); err != nil {
			return nil, d.refuse(b, label, err)
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
					Next: fmt.Sprintf("send to it instead: drive read %s", label)}
			}
		}
		return b.Append(batch.Event{At: d.now(), Kind: batch.KindOpenIntent, Session: label, Intent: intent, Dir: opt.Dir, Text: clip(stepSummary(step), 200)})
	})
	if err != nil {
		return nil, d.refuse(b, label, err)
	}

	block, err := d.Term.Run(rex.Open{Session: b.Meta.RexSession, Dir: opt.Dir, Label: label, Command: d.launch(opt)})
	if err != nil {
		f := &Failed{What: fmt.Sprintf("Rex did not open a block for %s: %v", label, err)}
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Intent: intent, Reason: f.What})
		return nil, f
	}
	// On record at once: a driver that ends while Claude Code starts leaves the block known.
	if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindOpen, Session: label, Intent: intent, Block: block, Dir: opt.Dir, Outcome: "starting"}); err != nil {
		return nil, err
	}
	s, err := d.awaitSession(block, 90*time.Second)
	if err != nil {
		screen, _ := d.Term.Capture(block, false)
		f := &Failed{What: fmt.Sprintf("Claude Code did not start in block %s within 90 s; its screen ends:\n%s", rex.Short(block), tail(strings.TrimSpace(screen), 600)),
			Next: fmt.Sprintf("close the block (rex block close %s) once you have read why", rex.Short(block))}
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Block: block, Reason: f.What})
		return nil, f
	}
	if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindOpen, Session: label, Block: block,
		SID: s.ID, Account: s.Account, Dir: s.Dir, Ref: opt.Resume}); err != nil {
		return nil, err
	}
	out = &Opened{Label: label, Block: block, Account: s.Account, Session: s.ID}
	l, err := d.own(label, block, fmt.Sprintf("sending %s its first step", label))
	if err != nil {
		return out, err
	}
	d.touch(block) // a session drive started is watched like one it typed into
	defer func() {
		watched, werr := l.end()
		if out.Sent != nil {
			out.Sent.Watched = watched
		}
		if werr != nil && err == nil {
			err = wakeFailed(label, werr)
		}
	}()
	if step.empty() {
		return out, nil
	}
	f, _ := s.Frontier()
	o := &Observation{Label: label, Session: s, Frontier: f, View: turn.ViewOf(label, s.ID, f)}
	// A resumed session read the notice in the conversation it resumes.
	p, err := d.plan(s.Dir, s.Config, o.View, step, opt.Resume == "")
	if err != nil {
		return out, &Failed{What: fmt.Sprintf("opened %s, but its first step was refused: %v", label, err),
			Next: fmt.Sprintf("the session is open and idle; send a step it can take: drive read %s", label)}
	}
	sent, err := d.send(b, o, p, SendOptions{Hold: opt.Hold}, o.View.String())
	out.Sent = sent
	return out, err
}

// settleOpen gives an outcome to the opens of label an earlier driver
// admitted and never finished. The caller owns the label, so that driver has
// gone; a block Rex opened under the label is the one it opened.
func (d *Driver) settleOpen(b *batch.Batch, evs []batch.Event, label string) error {
	members := batch.Members(evs)
	for _, e := range batch.Unresolved(evs, label) {
		if e.Kind != batch.KindOpenIntent {
			continue
		}
		ev := batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Intent: e.Intent,
			Reason: "settled: its driver ended before Rex opened a block"}
		if blocks, err := d.Term.Blocks(b.Meta.RexSession); err == nil {
			for _, bl := range blocks {
				if bl.Label == label && batch.Find(members, bl.ID) == nil {
					ev = batch.Event{At: d.now(), Kind: batch.KindOpen, Session: label, Intent: e.Intent, Block: bl.ID, Dir: e.Dir,
						Outcome: "settled", Note: "its driver ended before Claude Code started"}
					break
				}
			}
		}
		if err := b.Append(ev); err != nil {
			return err
		}
	}
	return nil
}

// launch is the command a new block runs: the owner's launcher in a login
// zsh, so its functions exist, then a shell that keeps the block open after
// Claude Code exits.
func (d *Driver) launch(opt OpenOptions) []string {
	var env []string
	for k, v := range d.Cfg.LaunchEnv {
		env = append(env, k+"="+shellQuote(v))
	}
	sort.Strings(env)
	launcher := d.Cfg.Launcher
	if opt.Account != "" {
		launcher += "-" + opt.Account
	}
	cmd := strings.TrimSpace(strings.Join(env, " ") + " " + launcher)
	if opt.Model != "" {
		cmd += " --model " + shellQuote(opt.Model)
	}
	if opt.Resume != "" {
		cmd += " --resume " + shellQuote(opt.Resume)
	}
	return []string{"/bin/zsh", "-lic", cmd + "; exec zsh -l"}
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@._-/=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
