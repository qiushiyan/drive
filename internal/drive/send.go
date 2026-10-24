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
	"github.com/qiushiyan/drive/internal/session"
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
		return nil, &Refused{Reason: err.Error()}
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
func (d *Driver) wakes(block string) (webhook, armed bool) {
	b, err := d.Batch()
	if err != nil || b.Meta.Wake != "webhook" {
		return false, false
	}
	return true, d.Watches != nil && d.Watches.Alive(watchName(b, block))
}

// whenTurnEnds says how the driver comes back to a session once its
// working turn ends.
func (d *Driver) whenTurnEnds(label, block string) string {
	switch webhook, armed := d.wakes(block); {
	case !webhook:
		return "wait for its turn to end: " + d.run("wait", label)
	case armed:
		return "its wake comes when its turn ends"
	}
	return "arm its wake, which comes when its turn ends: " + d.run("watch", label)
}

// settle gives an outcome to the sends an earlier driver of this session
// admitted and never finished. The caller owns the session, so that driver
// has gone; whether its line arrived is read from the transcript. A session
// that cannot be read for now settles nothing, since a step it took and
// still runs would read as one that never reached it and free its hold; a
// session gone or closed holds nothing, and settles from what it shows.
func (d *Driver) settle(b *batch.Batch, label, block string) error {
	evs, err := b.Events()
	if err != nil {
		return err
	}
	var orphans []batch.Event
	for _, e := range batch.Unresolved(evs, label) {
		if e.Kind == batch.KindSendIntent {
			orphans = append(orphans, e)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	var f turn.Frontier
	s, err := session.Locate(d.Term, d.Host, block)
	if err == nil {
		f, err = s.Frontier()
	}
	if err != nil && !errors.Is(err, session.ErrNoClaude) && d.blockExists(block) {
		return fmt.Errorf("%s cannot be read to settle the step an earlier driver left it: %v", label, err)
	}
	for _, p := range orphans {
		e := batch.Event{At: d.now(), Session: label, Intent: p.Intent, Hold: p.Hold, View: p.View, Key: p.Key,
			Text: p.Text, Path: p.Path, Mode: p.Mode, Notice: p.Notice}
		switch outcome, instead := arrival(f, p); {
		case outcome != "":
			e.Kind, e.Outcome, e.Note = batch.KindSend, outcome, "settled: its driver ended before confirming it"
		case instead != "":
			e.Kind, e.Reason = batch.KindFailure, fmt.Sprintf("settled: its driver ended before the step reached the session, which received %q instead", clip(instead, 120))
		default:
			e.Kind, e.Reason = batch.KindFailure, "settled: its driver ended before the step reached the session"
		}
		if err := b.Append(e); err != nil {
			return err
		}
	}
	return nil
}

// arrival looks for an intent's line among the prompts the session received
// since the intent was recorded: arrived, queued, or garbled when one is the
// line; else the newest prompt that came instead, such as the owner's own
// words.
func arrival(f turn.Frontier, intent batch.Event) (outcome, instead string) {
	ps := f.Prompts
	if f.Queued != nil {
		ps = append(append([]turn.Prompt{}, ps...), *f.Queued)
	}
	for _, p := range ps {
		if p.At.Before(intent.At) {
			continue
		}
		switch got := received(intent.Text, p); got {
		case "":
		case "other":
			instead = strings.TrimSpace(p.Text)
		default:
			return got, ""
		}
	}
	return "", instead
}

// received is what the session made of line, typed into it, in prompt p:
// arrived, or queued behind a running turn; garbled when p is the line with
// its head lost, which comes back as a line break; other when it is
// something else. "" is a prompt that tells nothing: one Claude Code queued
// without its text, which only a slash command is. Whitespace around
// either, a retype's padding or a lost head's line break, does not count.
func received(line string, p turn.Prompt) string {
	want := strings.TrimSpace(line)
	if !p.Known {
		if skill.Command(want) != "" {
			return "queued"
		}
		return ""
	}
	switch got := strings.TrimSpace(p.Text); {
	case got == want && p.Queued:
		return "queued"
	case got == want:
		return "arrived"
	case got != "" && strings.HasSuffix(want, got):
		return "garbled"
	}
	return "other"
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
	case strings.HasPrefix(state, "blocked"):
		call := ""
		if p := o.Frontier.Pending; p != nil {
			call = fmt.Sprintf(" for %s: %s", p.Name, clip(p.Input, 120))
		}
		return &Refused{Reason: fmt.Sprintf("%s waits on a %s prompt%s", o.Label, strings.TrimPrefix(state, "blocked:"), call),
			Next: "answer it first; " + d.answerNext(o.Label, view, state)}
	case state == "working" && !opt.Queue:
		queued := opt
		queued.Queue = true
		return &Refused{Reason: fmt.Sprintf("%s is working (for %s)", o.Label, since(d.now(), o.Session.Since())),
			Next: fmt.Sprintf("%s, and its step is decided then; only a step meant to reach it mid-turn goes now: %s", d.whenTurnEnds(o.Label, o.Session.Block), d.sendCommand(view, step, queued))}
	case !opt.Queue && !o.Frontier.Answered():
		p := o.Frontier.Newest()
		return &Refused{Reason: fmt.Sprintf("%s has a prompt it has not answered yet (%s: %s)", o.Label, p.At.In(d.zone()).Format("15:04"), clip(promptText(*p), 80)),
			Next: "someone already sent its next step; " + d.whenTurnEnds(o.Label, o.Session.Block)}
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
func (d *Driver) answerNext(label, view, state string) string {
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
			after, err = d.observe(batch.Members(evs), o.Label)
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

// awaitTurn waits for the turn the notice started to end.
// queue is the send that queues the step behind that turn, built when the
// wait gives up.
func (d *Driver) awaitTurn(o *Observation, limit time.Duration, queue func() string) error {
	end := d.now().Add(limit)
	for d.now().Before(end) {
		d.sleep(d.poll())
		s, err := session.Locate(d.Term, d.Host, o.Session.Block)
		if err != nil {
			return &Failed{What: fmt.Sprintf("%s's session went away while it read the notice", o.Label)}
		}
		f, err := s.Frontier()
		if err != nil {
			continue
		}
		if d.turnOf(s, f, time.Time{}).Over {
			return nil
		}
	}
	return &Failed{What: fmt.Sprintf("%s did not finish reading the notice within %s", o.Label, limit),
		Next: "it takes the step once that turn ends if the step is queued behind it: " + queue()}
}

const pad = "        "

// deliver types a line, submits it and confirms from the transcript that it
// arrived whole. Typing sometimes drops a line's first characters, which
// come back as a line break; the garbled turn is then taken back and the
// line typed again behind eight spaces, which absorb the loss and change
// nothing a session reads. A slash command is never padded: behind spaces
// it is words, so it is typed again as it is.
func (a *action) deliver(line string) (string, error) {
	d, o := a.d, a.o
	outcome, got, err := a.typeAndConfirm(line)
	if err != nil || got == nil {
		return outcome, err
	}
	// Take the garbled turn back only while it is the one running: Ctrl+C
	// stops whatever turn is, and once that turn ended its work is done.
	block := o.Session.Block
	s, err := session.Locate(d.Term, d.Host, block)
	var f turn.Frontier
	if err == nil {
		f, err = s.Frontier()
	}
	if p := f.Newest(); err != nil || p == nil || p.ID != got.ID || p.Queued || turnState(s, f) != "working" {
		return "garbled", &Failed{What: fmt.Sprintf("%s received the line without its start (%q), and that turn is not one drive can take back", o.Label, clip(got.Text, 120)),
			Next: fmt.Sprintf("read what it made of it (%s) before sending the step again", d.run("read", o.Label))}
	}
	_ = a.press("ctrl+c")
	d.sleep(d.poll())
	for range 5 {
		screen, err := d.Term.Capture(block, true)
		if err != nil {
			break
		}
		if k, _ := turn.InputBox(screen); k != turn.InputTyped {
			break
		}
		_ = a.press("ctrl+u")
	}
	retype := pad + line
	if skill.Command(line) != "" {
		retype = line
	}
	outcome, got, err = a.typeAndConfirm(retype)
	if err != nil {
		return outcome, err
	}
	if got != nil {
		return "garbled", &Failed{What: fmt.Sprintf("%s lost the start of the line twice; it last received %q", o.Label, clip(got.Text, 120)),
			Next: fmt.Sprintf("it is working on that; read it first (%s), and interrupt it if it works on the wrong thing: %s", d.run("read", o.Label), d.run("respond", viewNow(o), "interrupt"))}
	}
	return "recovered", nil
}

// typeAndConfirm types a line and waits for a prompt the session did not
// have before: the line, the outcome; the line with its head lost, the
// prompt that arrived. A prompt it already had, however recent and alike, is
// never taken for this one.
func (a *action) typeAndConfirm(line string) (string, *turn.Prompt, error) {
	d, o := a.d, a.o
	block := o.Session.Block
	before := ""
	if f, err := o.Session.Frontier(); err == nil {
		if p := f.Newest(); p != nil {
			before = p.ID
		}
	}
	if err := a.typeInto(line); err != nil {
		return "", nil, err
	}
	d.sleep(time.Second)
	if err := a.press("enter"); err != nil {
		return "", nil, &Failed{What: fmt.Sprintf("typed the line into %s, but Rex did not press Enter: %v", o.Label, err),
			Next: fmt.Sprintf("the line sits in its input box: clear it (%s), then send the step again", d.run("respond", viewNow(o), "clear"))}
	}
	want := strings.TrimSpace(line)
	limit := time.Minute
	if strings.HasPrefix(want, "/compact") {
		limit = 10 * time.Minute // a Claude Code older than 2.1.308 may record it only once compaction ends
	}
	for end := d.now().Add(limit); d.now().Before(end); {
		d.sleep(d.poll())
		if o.Session.ID == "" {
			// A fresh session registers before its first prompt creates the transcript.
			if s, err := session.Locate(d.Term, d.Host, block); err == nil {
				o.Session = s
			}
		}
		f, err := o.Session.Frontier()
		if err != nil {
			continue
		}
		p := f.Newest()
		if p == nil || p.ID == before {
			continue
		}
		switch received(line, *p) {
		case "":
			continue
		case "arrived":
			return "arrived", nil, d.checkAnswer(o, p.At)
		case "queued":
			return "queued", nil, nil
		case "garbled":
			return "", p, nil
		}
		// Something reached the session, the line among it or not: a send
		// that reached it, so a hold it took stands while that turn runs.
		return "other", nil, &Failed{What: fmt.Sprintf("%s received %q where %q was typed", o.Label, clip(strings.TrimSpace(p.Text), 120), clip(want, 120)),
			Next: fmt.Sprintf("read what it made of it before interrupting anything: %s; only if it works on the wrong thing, interrupt it: %s",
				d.run("read", o.Label), d.run("respond", viewNow(o), "interrupt"))}
	}
	what := fmt.Sprintf("nothing new reached %s's transcript within %s; the send is recorded as unknown", o.Label, limit)
	if screen, err := d.Term.Capture(block, true); err == nil {
		if k, typed := turn.InputBox(screen); k == turn.InputTyped {
			what += fmt.Sprintf("; its input box holds %q, so Enter may not have reached it", clip(typed, 80))
		}
	}
	return "unknown", nil, &Failed{What: what,
		Next: fmt.Sprintf("if the line sits in its input box (%s shows it), clear it and send again: %s", d.run("read", o.Label), d.run("respond", viewNow(o), "clear"))}
}

// viewNow is the session's view as it stands when a message is built, for
// the command the message prints.
func viewNow(o *Observation) string {
	if f, err := o.Session.Frontier(); err == nil {
		return turn.ViewOf(o.Label, o.Session.ID, f).String()
	}
	return o.View.String()
}

// checkAnswer watches the first seconds of the turn a prompt started for an
// API failure, which ends the turn at once: an expired login, most often.
func (d *Driver) checkAnswer(o *Observation, at time.Time) error {
	for end := d.now().Add(15 * time.Second); d.now().Before(end); d.sleep(d.poll()) {
		f, err := o.Session.Frontier()
		if err != nil {
			return nil
		}
		o.Frontier = f
		if f.Error != nil && !f.Error.At.Before(at) {
			return &Failed{Login: f.Error.Kind == "authentication_failed",
				What: fmt.Sprintf("%s took the step and answered %q (%s, account %s)", o.Label, f.Error.Text, f.Error.Kind, o.Session.Account),
				Next: d.retryHint(o, f.Error.Kind)}
		}
		if f.Last != nil && !f.Last.At.Before(at) {
			return nil
		}
		if st, err := d.Term.Status(o.Session.Block); err == nil && st != nil && st.Kind == "auth" {
			return &Failed{Login: true, What: fmt.Sprintf("%s's login was rejected (account %s)", o.Label, o.Session.Account), Next: d.relaunchHint(o)}
		}
	}
	return nil
}

func (d *Driver) retryHint(o *Observation, kind string) string {
	if kind == "authentication_failed" {
		return d.relaunchHint(o)
	}
	if webhook, _ := d.wakes(o.Session.Block); webhook {
		// No wake comes for an API recovering: a timer brings the driver back.
		return "the session is idle again; send the step again once the API recovers, and have a timer bring you back to it: " + d.run("timer", "900", text("retry "+o.Label))
	}
	return fmt.Sprintf("the session is idle again; read it (%s) and send the step again when the API recovers", d.run("read", o.Label))
}
