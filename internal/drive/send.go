package drive

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
	Queue   bool   // the session is working: let it take this mid-turn
	Hold    string // a resource this step's turn uses that only one session may use at once
	Label   string // the label a session outside the batch joins it under
	NoWatch bool
}

// Sent reports a send.
type Sent struct {
	Label   string
	Outcome string // arrived, queued, recovered
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
	evs, err := b.Events()
	if err != nil {
		return nil, err
	}
	label, block, err := d.target(batch.Members(evs), v.Session)
	if err != nil {
		return nil, err
	}
	l, err := d.own(label, block, fmt.Sprintf("sending %s %q", label, clip(stepSummary(step), 60)))
	if err != nil {
		return nil, d.refuse(b, label, err)
	}
	l.noWake = opt.NoWatch
	defer func() { sent, err = endSend(l, sent, err) }()
	if err := d.settle(b, label, block); err != nil {
		return nil, err
	}
	if evs, err = b.Events(); err != nil {
		return nil, err
	}
	members := batch.Members(evs)
	o, err := d.observe(members, v.Session)
	if err != nil {
		return nil, err
	}
	if err := checkView(o, v, opt.Queue, b.Zone()); err != nil {
		return nil, d.refuse(b, o.Label, err)
	}
	if err := d.ready(o, opt.Queue); err != nil {
		return nil, d.refuse(b, o.Label, err)
	}
	m := batch.Find(members, o.Label)
	p, err := d.plan(o.Session.Dir, o.Session.Config, o.View, step, m == nil || m.Sends == 0 && !m.Resumed)
	if err != nil {
		return nil, d.refuse(b, o.Label, err)
	}
	return d.send(b, o, p, opt, v.String())
}

// settle gives an outcome to the sends an earlier driver of this session
// admitted and never finished. The caller owns the session, so that driver
// has gone; whether its line arrived is read from the transcript.
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
	var newest *turn.Prompt
	if s, err := session.Locate(d.Term, d.Host, block); err == nil {
		if f, err := s.Frontier(); err == nil {
			newest = f.Newest()
		}
	}
	for _, p := range orphans {
		e := batch.Event{At: d.now(), Session: label, Intent: p.Intent, Hold: p.Hold, View: p.View, Key: p.Key,
			Text: p.Text, Path: p.Path, Mode: p.Mode, Notice: p.Notice}
		if newest != nil && !newest.At.Before(p.At) {
			e.Kind, e.Outcome, e.Note = batch.KindSend, "arrived", "settled: its driver ended before confirming it"
		} else {
			e.Kind, e.Reason = batch.KindFailure, "settled: its driver ended before the step reached the session"
		}
		if err := b.Append(e); err != nil {
			return err
		}
	}
	return nil
}

// ready refuses a send the session cannot take as it stands. --queue lets a
// working session take a step mid-turn; nothing lets a step through a
// prompt, where Enter would answer it.
func (d *Driver) ready(o *Observation, queue bool) error {
	state := o.State()
	switch {
	case state == "blocked:auth":
		msg := ""
		if o.Frontier.Error != nil {
			msg = fmt.Sprintf(": it answered %q", o.Frontier.Error.Text)
		}
		return &Refused{Reason: fmt.Sprintf("%s's login was rejected (account %s)%s", o.Label, o.Session.Account, msg),
			Next: d.relaunchHint(o), account: o.Session.Account}
	case strings.HasPrefix(state, "blocked"):
		call := ""
		if p := o.Frontier.Pending; p != nil {
			call = fmt.Sprintf(" for %s: %s", p.Name, clip(p.Input, 120))
		}
		return &Refused{Reason: fmt.Sprintf("%s waits on a %s prompt%s", o.Label, strings.TrimPrefix(state, "blocked:"), call),
			Next: fmt.Sprintf("answer it first: drive respond %s approve|deny", o.View)}
	case state == "working" && !queue:
		return &Refused{Reason: fmt.Sprintf("%s is working (for %s)", o.Label, since(d.now(), o.Session.Since())),
			Next: fmt.Sprintf("wait for its turn to end (drive wait %s), or add --queue to have it take this mid-turn", o.Label)}
	case !queue && !o.Frontier.Answered():
		p := o.Frontier.Newest()
		return &Refused{Reason: fmt.Sprintf("%s has a prompt it has not answered yet (%s: %s)", o.Label, p.At.In(d.zone()).Format("15:04"), clip(promptText(*p), 80)),
			Next: fmt.Sprintf("someone already sent its next step; drive wait %s", o.Label)}
	case o.Input == turn.InputTyped:
		return &Refused{Reason: fmt.Sprintf("%s's input box holds typed text nobody sent: %q; it may be the owner's", o.Label, clip(o.Typed, 80)),
			Next: fmt.Sprintf("leave it; if it is a step of yours that never went out, clear it: drive respond %s clear", o.View)}
	}
	return nil
}

func orWord(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
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

// holdRefused is a send refused because another session holds what it needs.
type holdRefused struct {
	Refused
	hold string
	step Step
}

func (d *Driver) refuse(b *batch.Batch, label string, err error) error {
	if b == nil {
		return err
	}
	var h *holdRefused
	if errors.As(err, &h) {
		text := h.step.Line
		if h.step.Key != "" {
			text = h.step.Text
		}
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindRefused, Session: label, Reason: h.Reason, Hold: h.hold,
			Key: h.step.Key, Text: text, Path: h.step.File})
		return &h.Refused
	}
	var r *Refused
	if errors.As(err, &r) {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindRefused, Session: label, Reason: r.Reason, Account: r.account})
	}
	return err
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
// on the account whose configuration is config.
func (d *Driver) plan(dir, config string, view turn.View, step Step, notice bool) (*plan, error) {
	p := &plan{step: step, notice: notice}
	switch {
	case step.Line != "":
		if strings.Contains(step.Line, "\n") || len(step.Line) > maxLine {
			return nil, &Refused{Reason: "a typed step is one short line: Claude Code keeps only the end of long typing",
				Next: "write it to a file and send it with --file"}
		}
		p.typed = step.Line
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
				Next: fmt.Sprintf("type the command and put the rest in a file it points at: drive send %s '%s Read <file> for the rest'",
					view, clip(first, 120))}
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

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// send admits, records, delivers and confirms one planned step. The caller
// owns the session, and o is what its checks read.
func (d *Driver) send(b *batch.Batch, o *Observation, p *plan, opt SendOptions, view string) (*Sent, error) {
	intent := newIntent()
	var noticeLine string
	err := b.Locked(func(evs []batch.Event) error {
		members := batch.Members(evs)
		if opt.Hold != "" {
			if h, held := d.holder(evs, members, opt.Hold, o.Label); held {
				what := "whose turn is still running"
				if h.Pending {
					what = "whose send is still under way"
				}
				next := fmt.Sprintf("send this once %s's turn ends: drive wait %s", h.Session, h.Session)
				if b.Meta.Wake == "webhook" {
					next = fmt.Sprintf("%s's wake comes when its turn ends; send this to %s then (drive status lists %s as waiting for %s)",
						h.Session, o.Label, o.Label, opt.Hold)
				}
				return &holdRefused{Refused{Reason: fmt.Sprintf("%s is held by %s since %s, %s", opt.Hold, h.Session, h.Since.In(b.Zone()).Format("15:04"), what),
					Next: next}, opt.Hold, p.step}
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
			noticeLine = pointer(path)
		}
		if p.body != "" {
			path, err := b.SaveSend(o.Label, []byte(strings.TrimRight(p.body, "\n")+"\n"))
			if err != nil {
				return err
			}
			p.path, p.typed = path, pointer(path)
		}
		add = append(add, batch.Event{At: d.now(), Kind: batch.KindSendIntent, Session: o.Label, Intent: intent,
			Hold: opt.Hold, View: view, Key: p.key, Text: p.typed, Path: p.path, Mode: p.mode(), Notice: p.notice})
		return b.Append(add...)
	})
	if err != nil {
		return nil, d.refuse(b, o.Label, err)
	}
	record := func(e batch.Event) {
		e.At, e.Session, e.Intent, e.Hold, e.View = d.now(), o.Label, intent, opt.Hold, view
		_ = b.Append(e)
	}
	failed := func(err error) {
		e := batch.Event{Kind: batch.KindFailure, Reason: err.Error()}
		if f := (*Failed)(nil); errors.As(err, &f) && f.Login {
			e.Account = o.Session.Account // the relaunch hint steers clear of it
		}
		record(e)
	}

	if noticeLine != "" {
		// A typed first step follows the notice once the notice's turn ends.
		outcome, err := d.deliver(o, noticeLine)
		if outcome != "" {
			record(batch.Event{Kind: batch.KindSend, Outcome: outcome, Mode: "notice", Text: noticeLine, Notice: true})
		}
		if err != nil {
			failed(err)
			return nil, err
		}
		if err := d.awaitTurn(o, 5*time.Minute); err != nil {
			failed(err)
			return nil, err
		}
		evs, err := b.Events()
		if err != nil {
			return nil, err
		}
		after, err := d.observe(batch.Members(evs), o.Label)
		if err == nil {
			err = d.ready(after, false)
		}
		if err != nil {
			f := &Failed{What: fmt.Sprintf("%s read the notice; its step was not sent: %s", o.Label, err.Error())}
			var r *Refused
			if errors.As(err, &r) {
				f.What, f.Next = fmt.Sprintf("%s read the notice; its step was not sent: %s", o.Label, r.Reason), r.Next+", then send the step"
			}
			failed(f)
			return nil, f
		}
		o = after
	}
	outcome, err := d.deliver(o, p.typed)
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

// endSend ends a send's lease and says whether its wake is armed.
func endSend(l *lease, sent *Sent, err error) (*Sent, error) {
	watched, werr := l.end()
	if sent != nil {
		sent.Watched = watched
	}
	if werr != nil && err == nil {
		label := ""
		if sent != nil {
			label = sent.Label
		}
		return sent, wakeFailed(label, werr)
	}
	return sent, err
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
func (d *Driver) awaitTurn(o *Observation, limit time.Duration) error {
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
		if state := turnState(s, f); state != "working" && (f.Answered() || strings.HasPrefix(state, "blocked")) {
			return nil
		}
	}
	return &Failed{What: fmt.Sprintf("%s did not finish reading the notice within %s", o.Label, limit),
		Next: fmt.Sprintf("drive read %s, then send the step when its turn ends", o.Label)}
}

const pad = "        "

// deliver types a line, submits it and confirms from the transcript that it
// arrived whole. Typing sometimes drops a line's first characters; the
// garbled turn is then taken back and the line typed again behind eight
// spaces, which absorb the loss and change nothing a session reads.
func (d *Driver) deliver(o *Observation, line string) (string, error) {
	outcome, got, err := d.typeAndConfirm(o, line)
	if err != nil || got == nil {
		return outcome, err
	}
	// Take the garbled turn back only while it is the one running: Ctrl+C
	// stops whatever turn is, and once that turn ended its work is done.
	block := o.Session.Block
	f, ferr := o.Session.Frontier()
	st, serr := d.Term.Status(block)
	if p := f.Newest(); ferr != nil || serr != nil || st == nil || p == nil || p.ID != got.ID || p.Queued || st.State != "working" {
		return "garbled", &Failed{What: fmt.Sprintf("%s received the line without its start (%q), and that turn is not one drive can take back", o.Label, clip(got.Text, 120)),
			Next: fmt.Sprintf("read what it made of it (drive read %s) before sending the step again", o.Label)}
	}
	_ = d.press(block, "ctrl+c")
	d.sleep(d.poll())
	for i := 0; i < 5; i++ {
		screen, err := d.Term.Capture(block, true)
		if err != nil {
			break
		}
		if k, _ := turn.InputBox(screen); k != turn.InputTyped {
			break
		}
		_ = d.press(block, "ctrl+u")
	}
	outcome, got, err = d.typeAndConfirm(o, pad+line)
	if err != nil {
		return outcome, err
	}
	if got != nil {
		return "garbled", &Failed{What: fmt.Sprintf("%s lost the start of the line twice; it last received %q", o.Label, clip(got.Text, 120)),
			Next: fmt.Sprintf("it is working on that; read it (drive read %s), then interrupt it with the view that prints: drive respond VIEW interrupt", o.Label)}
	}
	return "recovered", nil
}

// typeAndConfirm types a line and waits for a prompt the session did not
// have before: the line, the outcome; the line with its head lost, the
// prompt that arrived. A prompt it already had, however recent and alike, is
// never taken for this one.
func (d *Driver) typeAndConfirm(o *Observation, line string) (string, *turn.Prompt, error) {
	block := o.Session.Block
	before := ""
	if f, err := o.Session.Frontier(); err == nil {
		if p := f.Newest(); p != nil {
			before = p.ID
		}
	}
	if err := d.typeInto(block, line); err != nil {
		return "", nil, err
	}
	d.sleep(time.Second)
	if err := d.press(block, "enter"); err != nil {
		return "", nil, err
	}
	want := strings.TrimLeft(line, " ")
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
		if !p.Known {
			// Claude Code queues a slash command typed while busy without its text.
			if skill.Command(want) != "" {
				return "queued", nil, nil
			}
			continue
		}
		got := strings.TrimLeft(p.Text, " ")
		switch {
		case got == want:
			if p.Queued {
				return "queued", nil, nil
			}
			return "arrived", nil, d.checkAnswer(o, p.At)
		case got != "" && strings.HasSuffix(want, got):
			return "", p, nil
		default:
			return "", nil, &Failed{What: fmt.Sprintf("%s received %q instead of what was typed", o.Label, clip(got, 120)),
				Next: fmt.Sprintf("it is working on that now; read it (drive read %s), then interrupt it with the view that prints: drive respond VIEW interrupt", o.Label)}
		}
	}
	what := fmt.Sprintf("nothing new reached %s's transcript within %s; the send is recorded as unknown", o.Label, limit)
	if screen, err := d.Term.Capture(block, true); err == nil {
		if k, typed := turn.InputBox(screen); k == turn.InputTyped {
			what += fmt.Sprintf("; its input box holds %q, so Enter may not have reached it", clip(typed, 80))
		}
	}
	return "unknown", nil, &Failed{What: what,
		Next: fmt.Sprintf("drive read %s: if the line sits in its input box, clear it (drive respond VIEW clear) and send again", o.Label)}
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
	return fmt.Sprintf("the session is idle again; read it (drive read %s) and send the step again when the API recovers", o.Label)
}
