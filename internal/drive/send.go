package drive

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/skill"
	"github.com/qiushiyan/drive/internal/snippet"
	"github.com/qiushiyan/drive/internal/turn"
)

// Step is what to send: a short line typed as is (a slash command too), a
// file the session is told to read, or one of the owner's snippets.
type Step struct {
	Line string
	File string
	Key  string // snippet key
	Text string // what follows the snippet, where the owner would type it
}

func (s Step) empty() bool { return s.Line == "" && s.File == "" && s.Key == "" }

// SendOptions shape a send.
type SendOptions struct {
	Queue bool   // the session is working: let it take this mid-turn
	Hold  string // a resource this step's turn uses that only one session may use at once
	Label string // the label a session outside the batch joins it under
}

// Sent reports a send.
type Sent struct {
	Label   string
	Outcome string // arrived, queued, recovered; beside a failure, garbled, other or unknown
	Typed   string // what went into the input box
	Path    string // the file sent, for a file step
	Notice  bool
	Watched bool
	From    string // the snippet's file
}

// The sentence a file step is typed as. claude-steps matches it word for word
// to show which snippet a file carried.
func pointer(path string) string { return "Read " + path + " and do what it asks." }

const maxLine = 500

// Send sends a session its next step, decided on view.
func (d *Driver) Send(view string, step Step, opt SendOptions) (sent *Sent, err error) {
	v, err := turn.ParseView(view)
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	b, err := d.Batch()
	if err != nil {
		return nil, err
	}
	what := func(label string) string { return fmt.Sprintf("sending %s %q", label, clip(stepSummary(step), 60)) }
	a, err := d.onView(b, v, what, opt.Queue, d.sendCommand(view, step, opt))
	if err != nil {
		return nil, err
	}
	defer func() {
		if watched := a.finish(&err); sent != nil {
			sent.Watched = watched
		}
	}()
	o := a.o
	if err := d.ready(o, opt, step); err != nil {
		return nil, err
	}
	p, err := d.plan(o.Session.Dir, o.Session.Config, d.run("send", o.View.String()), step, o.Member == nil || !o.Member.Noticed && !o.Member.Resumed)
	if err != nil {
		return nil, err
	}
	return a.send(p, opt, v.String())
}

// wakes says whether the driver is a chat bot brought back by the session's
// wake, which cannot hold a command open, and if so whether the session's
// wake is armed. A Claude Code driver waits for a turn itself.
func (d *Driver) wakes(o *Observation) (webhook, armed bool) {
	if o.in == nil || o.in.Meta.Wake != "webhook" {
		return false, false
	}
	return true, d.Watches != nil && d.Watches.Alive(watchName(o.in, o.Session.Block))
}

// whenTurnEnds says how the driver comes back to a session once its
// working turn ends.
func (d *Driver) whenTurnEnds(o *Observation) string {
	switch webhook, armed := d.wakes(o); {
	case !webhook:
		return "wait for its turn to end: " + d.run("wait", o.Label)
	case armed:
		return "its wake comes when its turn ends"
	}
	return "arm its wake, which comes when its turn ends: " + d.run("watch", o.Label)
}

// ready refuses a send the session cannot take as it stands. --queue lets a
// working session take a step mid-turn; nothing lets a step through a
// prompt, where Enter would answer it.
func (d *Driver) ready(o *Observation, opt SendOptions, step Step) error {
	state := o.State()
	view := o.View.String()
	switch {
	case state == "blocked:auth":
		msg := ""
		if o.Frontier.Error != nil {
			msg = fmt.Sprintf(": it answered %q", o.Frontier.Error.Text)
		}
		return &Refused{Reason: fmt.Sprintf("%s's login was rejected (account %s)%s", o.Label, o.Session.Account, msg),
			Next: d.relaunch(o, step), account: o.Session.Account}
	case state.onPrompt():
		call := ""
		if p := o.Frontier.Pending; p != nil {
			call = fmt.Sprintf(" for %s: %s", p.Name, clip(p.Input, 120))
		}
		return &Refused{Reason: fmt.Sprintf("%s waits on a %s prompt%s", o.Label, state.promptKind(), call),
			Next: "answer it first; " + d.answerNext(o.Label, view, state)}
	case state == "working" && !opt.Queue:
		queued := opt
		queued.Queue = true
		return &Refused{Reason: fmt.Sprintf("%s is working (for %s)", o.Label, since(d.now(), o.Session.Since())),
			Next: fmt.Sprintf("%s, and its step is decided then; only a step meant to reach it mid-turn goes now: %s", d.whenTurnEnds(o), d.sendCommand(view, step, queued))}
	case !opt.Queue && !o.Frontier.Answered():
		p := o.Frontier.Newest()
		return &Refused{Reason: fmt.Sprintf("%s has a prompt it has not answered yet (%s: %s)", o.Label, p.At.In(o.zone()).Format("15:04"), clip(promptText(*p), 80)),
			Next: "someone already sent its next step; " + d.whenTurnEnds(o)}
	case o.Input == turn.InputTyped:
		return &Refused{Reason: fmt.Sprintf("%s's input box holds typed text nobody sent: %q; it may be the owner's", o.Label, clip(o.Typed, 80)),
			Next: fmt.Sprintf("leave it, and park the session if it waits on the owner, as nothing wakes you for it: %s; if it is a step of yours that never went out, clear it: %s",
				d.run("park", view, text("an unsent line in its input box")), d.run("respond", view, "clear"))}
	case o.Input != turn.InputEmpty && o.Input != turn.InputSuggestion:
		// Typing lands in whatever the screen shows: a dialog, a view, a shell.
		return &Refused{Reason: fmt.Sprintf("%s's screen shows no input box to type into", o.Label),
			Next: "look at what it shows instead: " + d.run("read", o.Label, "--screen")}
	}
	return nil
}

// answerNext says how to answer the prompt a session waits on, decided on
// view: two whole commands, and on a question what each does there.
func (d *Driver) answerNext(label, view string, state State) string {
	approve, deny := d.run("respond", view, "approve"), d.run("respond", view, "deny")
	if state == "blocked:question" {
		return fmt.Sprintf("see the question and its options (%s); %s takes the option ❯ points at, %s dismisses the question and stops the turn, after which a step can answer it in words",
			d.run("read", label, "--screen"), approve, deny)
	}
	return fmt.Sprintf("see what it asks (%s), then answer it: %s, or %s", d.run("read", label, "--pending"), approve, deny)
}

func since(now, t time.Time) string {
	if t.IsZero() {
		return "a while"
	}
	d := now.Sub(t).Round(time.Minute)
	if d < time.Minute {
		return "under a minute"
	}
	return strings.TrimSuffix(d.String(), "0s")
}

func newIntent() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// plan is a step made ready to deliver, checked whole before anything
// reaches the session: a refusal always means nothing was sent.
type plan struct {
	step   Step
	notice bool   // the notice goes ahead of the step
	alone  bool   // in a turn of its own, ahead of a typed step
	text   []byte // the notice, when it goes alone
	typed  string // what is typed for the step; a file step's pointer once saved
	body   string // a file step's text, the notice ahead of it when it rides along
	path   string
	key    string
	from   string
}

func (p *plan) mode() string {
	if p.body != "" {
		return "file"
	}
	return "typed"
}

// plan turns a step into what will be typed, for a session working in dir
// on the account whose configuration is config. resend is the command that
// delivers a step, such as "drive send T1@3fa91c07", which a refusal prints
// with the step it can take instead.
func (d *Driver) plan(dir, config, resend string, step Step, notice bool) (*plan, error) {
	p := &plan{step: step, notice: notice}
	switch {
	case step.Line != "":
		line := strings.TrimSpace(step.Line)
		if err := typable(line); err != nil {
			return nil, err
		}
		p.typed = line
	case step.Key != "":
		sn, err := snippet.Expand(d.Cfg.Home, dir, step.Key, step.Text)
		if err != nil {
			return nil, &Refused{Reason: err.Error()}
		}
		p.key, p.from = sn.Key, sn.From
		if !strings.Contains(sn.Text, "\n") && len(sn.Text) <= maxLine && skill.Command(sn.Text) != "" {
			p.typed = sn.Text // a one-line slash command, such as planlab's closeout
		} else {
			p.body = sn.Text
		}
	default:
		data, err := os.ReadFile(step.File)
		if err != nil {
			return nil, &Refused{Reason: fmt.Sprintf("cannot read %s: %v", step.File, err)}
		}
		p.body = string(data)
		if strings.TrimSpace(p.body) == "" {
			return nil, &Refused{Reason: fmt.Sprintf("%s is empty", step.File)}
		}
	}
	if p.body != "" {
		first := firstLine(p.body)
		if name := skill.Command(first); name != "" && skill.TypedOnly(d.Cfg.Home, config, dir, name) {
			return nil, &Refused{
				Reason: fmt.Sprintf("/%s starts only when typed: a session told to run it from a file refuses", name),
				Next: fmt.Sprintf("type the command and put the rest in a file it points at: %s %s",
					resend, join(text(first+" Read <file> for the rest")))}
		}
	}
	if notice {
		text, err := os.ReadFile(d.Cfg.Notice)
		if err != nil {
			return nil, &Failed{What: fmt.Sprintf("the notice a session reads before its first step is missing (%s): %v", d.Cfg.Notice, err),
				Next: "restore it, or set notice in ~/.config/drive/config.toml"}
		}
		if p.body != "" {
			p.body = strings.TrimRight(string(text), "\n") + "\n\n" + p.body
		} else {
			p.alone, p.text = true, text
		}
	}
	return p, nil
}

// typable refuses a typed step Claude Code would record as something other
// than what was typed: more than one short line, which it keeps only the end
// of; a control character, which is a key press; a leading ! or #, which
// switch its input box to a shell command or a memory.
func typable(line string) error {
	toFile := "write it to a file and send it with --file"
	switch {
	case line == "":
		return &Refused{Reason: "the typed step is empty"}
	case strings.Contains(line, "\n") || len(line) > maxLine:
		return &Refused{Reason: "a typed step is one short line: Claude Code keeps only the end of long typing", Next: toFile}
	case strings.IndexFunc(line, unicode.IsControl) >= 0:
		c := line[strings.IndexFunc(line, unicode.IsControl):][:1]
		return &Refused{Reason: fmt.Sprintf("a typed step holds a control character (%q): typed into Claude Code it is a key press, not text", c), Next: toFile}
	case line[0] == '!' || line[0] == '#':
		return &Refused{Reason: fmt.Sprintf("a typed step starting with %q is not a prompt: Claude Code takes ! as a shell command and # as a memory", line[:1]), Next: toFile}
	}
	return nil
}

func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// send admits, records, delivers and confirms one planned step, decided on
// view, on the session the action read.
func (a *action) send(p *plan, opt SendOptions, view string) (*Sent, error) {
	d, b, o := a.d, a.b, a.o
	intent := newIntent()
	var noticeLine, noticeIntent string
	err := b.Locked(func(evs []batch.Event) error {
		members := batch.Members(evs)
		if opt.Hold != "" {
			if h, held := d.holder(evs, members, opt.Hold, o.Label); held {
				// The refused session is left untouched, so no wake of its own comes: one in the batch is parked.
				park := ""
				if batch.Find(members, o.Label) != nil {
					park = o.View.String()
				}
				return &holdRefused{Refused{Reason: fmt.Sprintf("%s is held by %s since %s, %s", opt.Hold, h.Session, h.Since.In(b.Zone()).Format("15:04"), h.why),
					Next: d.holdNext(b, h, o.Label, park, d.sendCommand(o.View.String(), p.step, opt), false)}, opt.Hold, p.step, ""}
			}
		}
		var add []batch.Event
		if batch.Find(members, o.Label) == nil {
			label := o.Label
			if opt.Label != "" {
				if other := batch.Find(members, opt.Label); other != nil && !other.Closed {
					return &Refused{Reason: fmt.Sprintf("the label %s is taken by block %s", opt.Label, rex.Short(other.Block))}
				}
				label = opt.Label
			}
			o.Label = label
			add = append(add, batch.Event{At: d.now(), Kind: batch.KindAdopt, Session: label, Block: o.Session.Block,
				SID: o.Session.ID, Account: o.Session.Account, Dir: o.Session.Dir})
		}
		// What the session will read is saved before it is told to read it.
		if p.alone {
			path, err := b.SaveSend(o.Label, p.text)
			if err != nil {
				return err
			}
			// The notice goes in a turn of its own, under an intent of its own:
			// the step's intent, and any hold it takes, stay pending until the
			// step itself has an outcome.
			noticeLine, noticeIntent = pointer(path), newIntent()
			add = append(add, batch.Event{At: d.now(), Kind: batch.KindSendIntent, Session: o.Label, Intent: noticeIntent,
				View: view, Text: noticeLine, Mode: "notice", Notice: true})
		}
		if p.body != "" {
			path, err := b.SaveSend(o.Label, []byte(strings.TrimRight(p.body, "\n")+"\n"))
			if err != nil {
				return err
			}
			p.path, p.typed = path, pointer(path)
		}
		add = append(add, batch.Event{At: d.now(), Kind: batch.KindSendIntent, Session: o.Label, Intent: intent,
			Hold: opt.Hold, View: view, Key: p.key, Text: p.typed, Path: p.path, Mode: p.mode(), Notice: p.notice && !p.alone})
		return b.Append(add...)
	})
	if err != nil {
		return nil, unsent(err, d.sendCommand(view, p.step, opt))
	}
	record := func(e batch.Event) {
		e.At, e.Session, e.Intent, e.Hold, e.View = d.now(), o.Label, intent, opt.Hold, view
		_ = b.Append(e)
	}
	failed := func(err error) {
		var e batch.Event
		if f := (*Failed)(nil); errors.As(err, &f) && f.Login {
			f.Next = d.relaunch(o, p.step) // the relaunch carries this step
			e.Account = o.Session.Account  // and steers clear of this account
		}
		e.Kind, e.Reason = batch.KindFailure, err.Error()
		record(e)
	}

	if noticeLine != "" {
		// A typed first step follows the notice once the notice's turn ends.
		outcome, err := a.deliver(noticeLine)
		notice := batch.Event{At: d.now(), Kind: batch.KindSend, Session: o.Label, Intent: noticeIntent, View: view,
			Outcome: outcome, Mode: "notice", Text: noticeLine, Notice: true}
		if outcome == "" && err != nil {
			notice.Kind, notice.Reason = batch.KindFailure, err.Error()
		}
		_ = b.Append(notice)
		if err != nil {
			failed(err)
			return nil, err
		}
		// Still running, the notice's turn takes the step queued behind it.
		queued := opt
		queued.Queue = true
		if err := d.awaitTurn(o, 5*time.Minute, func() string { return d.sendCommand(viewNow(o), p.step, queued) }); err != nil {
			failed(err)
			return nil, err
		}
		evs, err := b.Events()
		var after *Observation
		if err == nil {
			after, err = d.observe(b, batch.Members(evs), o.Label)
		}
		if err == nil {
			err = d.ready(after, SendOptions{}, p.step)
		}
		if err != nil {
			f := &Failed{What: fmt.Sprintf("%s read the notice; its step was not sent: %s", o.Label, err.Error()),
				Next: "read it, then send the step: " + d.run("read", o.Label)}
			var r *Refused
			switch {
			case !errors.As(err, &r):
			case r.account != "":
				f.What, f.Next = fmt.Sprintf("%s read the notice; its step was not sent: %s", o.Label, r.Reason), r.Next // the relaunch carries the step
			default:
				f.What = fmt.Sprintf("%s read the notice; its step was not sent: %s", o.Label, r.Reason)
				f.Next = fmt.Sprintf("%s; then its step goes, decided on a fresh read: %s", r.Next, d.run("read", o.Label))
			}
			failed(f)
			return nil, f
		}
		o, a.o = after, after
	}
	outcome, err := a.deliver(p.typed)
	sent := &Sent{Label: o.Label, Outcome: outcome, Typed: p.typed, Path: p.path, Notice: p.notice, From: p.from}
	if outcome != "" {
		record(batch.Event{Kind: batch.KindSend, Outcome: outcome, Mode: p.mode(), Text: p.typed, Path: p.path, Key: p.key, Notice: p.notice && !p.alone})
	}
	if err != nil {
		failed(err)
		return sent, err
	}
	return sent, nil
}

func stepSummary(s Step) string {
	switch {
	case s.Line != "":
		return s.Line
	case s.Key != "":
		return "snippet " + s.Key + " " + s.Text
	default:
		return "file " + s.File
	}
}
