package drive

import (
	"fmt"
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
// so two drivers deciding at once open one session, not two.
func (d *Driver) Open(label string, step Step, opt OpenOptions) (*Opened, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	if label == "" || strings.ContainsAny(label, " @/") {
		return nil, &Refused{Reason: fmt.Sprintf("label %q: use a short name without spaces, '@' or '/', such as T1", label)}
	}
	intent := newIntent()
	err = b.Locked(func(evs []batch.Event) error {
		if m := batch.Find(batch.Members(evs), label); m != nil && !m.Closed {
			if st, err := d.Term.Status(m.Block); err == nil && st != nil {
				return &Refused{Reason: fmt.Sprintf("%s is already open as block %s (since %s)", label, rex.Short(m.Block), m.Joined.Local().Format("15:04")),
					Next: fmt.Sprintf("send to it instead: drive read %s", label)}
			}
		}
		if p := batch.Pending(evs, label); p != nil && d.now().Sub(p.At) < batch.InFlight {
			return &Refused{Reason: fmt.Sprintf("another driver is opening %s (since %s)", label, p.At.Local().Format("15:04:05")),
				Next: "let it finish; drive status shows the session once it is up"}
		}
		return b.Append(batch.Event{At: d.now(), Kind: batch.KindOpenIntent, Session: label, Intent: intent, Dir: opt.Dir, Text: clip(stepSummary(step), 200)})
	})
	if err != nil {
		return nil, err
	}
	fail := func(f *Failed) (*Opened, error) {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Intent: intent, Reason: f.What})
		return nil, f
	}

	block, err := d.Term.Run(rex.Open{Session: b.Meta.RexSession, Dir: opt.Dir, Label: label, Command: d.launch(opt)})
	if err != nil {
		return fail(&Failed{What: fmt.Sprintf("Rex did not open a block for %s: %v", label, err)})
	}
	s, err := d.awaitSession(block, 90*time.Second)
	if err != nil {
		screen, _ := d.Term.Capture(block, false)
		return fail(&Failed{What: fmt.Sprintf("Claude Code did not start in block %s within 90 s; its screen ends:\n%s", rex.Short(block), tail(strings.TrimSpace(screen), 600)),
			Next: fmt.Sprintf("close the block (rex block close %s) once you have read why", rex.Short(block))})
	}
	if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindOpen, Session: label, Intent: intent, Block: block,
		SID: s.ID, Account: s.Account, Dir: s.Dir}); err != nil {
		return nil, err
	}
	out := &Opened{Label: label, Block: block, Account: s.Account, Session: s.ID}
	if step.empty() {
		if b.Meta.Wake == "webhook" {
			_ = d.arm(b, label, block)
		}
		return out, nil
	}
	f, _ := s.Frontier()
	o := &Observation{Label: label, Session: s, Frontier: f, View: turn.ViewOf(label, s.ID, f)}
	sent, err := d.send(b, o, step, SendOptions{Hold: opt.Hold}, o.View.String())
	out.Sent = sent
	return out, err
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
