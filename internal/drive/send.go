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
// file typed as its path's @-mention, or one of the owner's snippets.
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
	// Answer is the owner's answer this step carries out, recorded with its
	// intent.
	Answer *AskAnswer
}

// AskAnswer is the owner's answer to an ask.
type AskAnswer struct {
	Ask, Source, Took string
	Explained         bool
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
	AsFile  string // why a line went as a file: what typing it would have lost
	line    string // the step's own line, when the notice rode it
}

// The line a file step is typed as: its path as an @-mention, which Claude
// Code attaches to the prompt, as the owner attaches a file. A mention taken
// mid-turn is only words, so a queued step asks for the file to be read.
// claude-steps matches both word for word to show which snippet a file
// carried.
func pointer(path string, queued bool) string {
	if queued {
		return "Read " + path + " and do what it asks."
	}
	return "@" + path
}

// stepOf is the step a typed send carried: its text without the notice's
// @-mention, which a typed step carries last when the notice rides it.
func stepOf(e batch.Event) string {
	if e.Notice && e.Mode == "typed" {
		if i := strings.LastIndex(e.Text, " @"); i >= 0 {
			return e.Text[:i]
		}
	}
	return e.Text
}

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
	p, err := d.plan(o.Session.Dir, o.Session.Config, step, o.Member == nil || !o.Member.Noticed && !o.Member.Resumed, opt.Queue)
	if err != nil {
		return nil, err
	}
	if p.command != "" && opt.Queue {
		return nil, &Refused{Reason: fmt.Sprintf("%s starts only when typed, and the file it attaches attaches nothing typed mid-turn", strings.Fields(p.command)[0]),
			Next: d.whenTurnEnds(o) + ", and send it then"}
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
	case state == BlockedAuth:
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
	case state == Working && !opt.Queue:
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
			Next: fmt.Sprintf("leave it, and if it waits on the owner, ask them about it (%s) and park the session, as nothing wakes you for it: %s; if it is a step of yours that never went out, clear it: %s",
				d.askAbout(o.Label, o.Label+"'s input box holds a line nobody sent; is it yours?", "the line stays unsent"), d.run("park", view, text("an unsent line in its input box")), d.run("respond", view, "clear"))}
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
	if state == BlockedQuestion {
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
	head   string // the notice, at the head of the file a step attaches
	// cont is the end of a skill's line that did not fit beside the
	// mention, at the head of the step's own text in the file.
	cont  string
	alone bool   // in a turn of its own, ahead of a typed step
	ride  bool   // or as an @-mention after the typed step, when it fits
	text  []byte // the notice, when it goes alone
	typed string // what is typed for the step; a file step's pointer once saved
	// command is a skill's line from the head of a file step or of a
	// snippet, typed with the rest attached, so the session's prompt opens
	// with the skill as the owner's would; typedOnly when the skill starts
	// only when typed.
	command   string
	typedOnly bool
	asFile    string // why a line goes as a file
	line      string // the step's own line, when the notice rides it
	body      string // a file step's text, the notice ahead of it when it rides along
	path      string
	key       string
	from      string
}

// file is what the step's file holds: the notice, the end of a skill's
// line that did not fit, then the step's text.
func (p *plan) file() string {
	var parts []string
	for _, s := range []string{p.head, p.cont, p.body} {
		if s = strings.Trim(s, "\n"); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (p *plan) mode() string {
	if p.file() != "" {
		return "file"
	}
	return "typed"
}

// plan turns a step into what will be typed, for a session working in dir
// on the account whose configuration is config. A queued step keeps a skill
// the model can start in its file, since a mention typed mid-turn attaches
// nothing.
func (d *Driver) plan(dir, config string, step Step, notice, queue bool) (*plan, error) {
	p := &plan{step: step, notice: notice}
	switch {
	case step.Line != "":
		line := strings.TrimSpace(step.Line)
		if line == "" {
			return nil, &Refused{Reason: "the typed step is empty"}
		}
		if why := untypable(line); why != "" {
			// Typing would lose it, so it goes as a file, as a driver would send it.
			p.body, p.asFile = line, why
		} else {
			p.typed = line
		}
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
		if name := skill.Command(first); name != "" {
			// The skill's line is typed, and the rest attached to it, so the
			// prompt opens with the skill as the owner's would; a session told
			// to run a typed-only skill from a file refuses. A snippet goes
			// the same way under its key, as planlab's closeout does with
			// words after it; claude-steps knows it by the skill, and by the
			// snippet's opening where the line typed holds it.
			p.typedOnly = skill.TypedOnly(d.Cfg.Home, config, dir, name)
			if p.typedOnly || !queue && skill.Found(d.Cfg.Home, config, dir, name) {
				p.command, p.body = typedHead(first, p.body)
			}
		}
	}
	if notice {
		text, err := os.ReadFile(d.Cfg.Notice)
		if err != nil {
			return nil, &Failed{What: fmt.Sprintf("the notice a session reads before its first step is missing (%s): %v", d.Cfg.Notice, err),
				Next: "restore it, or set notice in ~/.config/drive/config.toml"}
		}
		if p.body != "" || p.command != "" {
			p.head = string(text)
		} else {
			name := skill.Command(p.typed)
			p.alone, p.text = true, text
			p.ride = name == "" || skill.Found(d.Cfg.Home, config, dir, name)
		}
	}
	return p, nil
}

// untypable says why Claude Code would record a typed line as something
// other than what was typed, "" when it would not: more than one short
// line, which it keeps only the end of; a control character, which is a key
// press; a leading ! or #, which switch its input box to a shell command or
// a memory.
func untypable(line string) string {
	switch {
	case strings.Contains(line, "\n") || len(line) > maxLine:
		return "Claude Code keeps only the end of a line this long"
	case strings.IndexFunc(line, unicode.IsControl) >= 0:
		return fmt.Sprintf("a control character (%q) typed into Claude Code is a key press", line[strings.IndexFunc(line, unicode.IsControl):][:1])
	case line[0] == '!' || line[0] == '#':
		return fmt.Sprintf("Claude Code takes a line starting with %q as a shell command or a memory", line[:1])
	}
	return ""
}

// typedHead splits a file step whose first line, first, starts a skill:
// the line to type and the rest. A first line that typing would garble is
// typed as the command alone, its words going with the rest; one too long
// to sit beside the rest's mention is cut when it is saved (fitHead).
func typedHead(first, body string) (command, rest string) {
	_, after, _ := strings.Cut(strings.TrimLeft(body, "\n\t "), first)
	rest = strings.TrimLeft(after, "\n")
	if len(first) <= maxLine && untypable(first) == "" {
		return first, rest
	}
	command = "/" + skill.Command(first)
	args := strings.TrimSpace(first[strings.Index(first, command)+len(command):])
	return command, strings.TrimSpace(args + "\n\n" + rest)
}

// fitHead cuts a skill's line to room bytes, so it fits beside the
// mention of the file that carries the rest: at the dash a brief's pointer
// puts between the skill's words and the goal, else to the command alone.
// The cut words open the file.
func fitHead(command string, room int) (head, cont string) {
	if len(command) <= room {
		return command, ""
	}
	if i := strings.Index(command, " — "); i > 0 && i <= room {
		return command[:i], strings.TrimSpace(command[i+len(" — "):])
	}
	name := "/" + skill.Command(command)
	return name, strings.TrimSpace(command[strings.Index(command, name)+len(name):])
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
		if opt.Answer != nil {
			if _, refused := d.openAsk(b, evs, opt.Answer.Ask); refused != nil {
				return refused // another run answered it since this one read
			}
		}
		if opt.Hold != "" {
			if h := d.holder(evs, members, opt.Hold, o.Label); h.held() {
				// The refused session is left untouched, so no wake of its own comes: one in the batch is parked.
				park := ""
				if batch.Find(members, o.Label) != nil {
					park = o.View.String()
				}
				return &holdRefused{Refused{Reason: fmt.Sprintf("%s is held by %s since %s, %s", opt.Hold, h.Session, h.Since.In(b.Zone()).Format("15:04"), h.why()),
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
			if line := p.typed + " " + pointer(path, false); p.ride && !opt.Queue && len(line) <= maxLine {
				// The notice rides the step's own line, attached to its turn.
				p.line, p.typed, p.alone = p.typed, line, false
			} else {
				// The notice goes in a turn of its own, under an intent of its own:
				// the step's intent, and any hold it takes, stay pending until the
				// step itself has an outcome.
				noticeLine, noticeIntent = pointer(path, opt.Queue), newIntent()
				add = append(add, batch.Event{At: d.now(), Kind: batch.KindSendIntent, Session: o.Label, Intent: noticeIntent,
					View: view, Text: noticeLine, Mode: "notice", Notice: true})
			}
		}
		if p.command != "" && p.file() != "" {
			p.command, p.cont = fitHead(p.command, maxLine-len(" @")-b.SendPathRoom(o.Label))
		}
		switch {
		case p.command != "" && p.file() == "":
			p.typed = p.command // the file held only the skill's line
		case p.command != "":
			path, err := b.SaveSend(o.Label, []byte(p.file()+"\n"))
			if err != nil {
				return err
			}
			p.path, p.typed = path, p.command+" "+pointer(path, false)
		case p.file() != "":
			path, err := b.SaveSend(o.Label, []byte(p.file()+"\n"))
			if err != nil {
				return err
			}
			p.path, p.typed = path, pointer(path, opt.Queue)
		}
		if an := opt.Answer; an != nil {
			add = append(add, batch.Event{At: d.now(), Kind: batch.KindAnswer, Ref: an.Ask, Source: an.Source, Took: an.Took, Explained: an.Explained, Intent: intent})
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
		if f, ok := errors.AsType[*Failed](err); ok && (f.Login || f.account != "") {
			if f.Login {
				f.Next = d.relaunch(o, p.step) // the relaunch carries this step
			} else {
				f.Next = d.limitNext(o, p.step) // and so does the one from a usage limit
			}
			e.Account = o.Session.Account // and every relaunch and reopen steers clear of this account
			if lim := o.Frontier.Error; lim != nil && lim.Limit {
				e.Due = lim.Resets // until its limit lifts
			}
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
	sent := &Sent{Label: o.Label, Outcome: outcome, Typed: p.typed, Path: p.path, Notice: p.notice, From: p.from, AsFile: p.asFile, line: p.line}
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
