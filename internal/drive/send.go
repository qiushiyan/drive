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
func (d *Driver) Send(view string, step Step, opt SendOptions) (*Sent, error) {
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
	o, err := d.observe(batch.Members(evs), v.Session)
	if err != nil {
		return nil, err
	}
	if err := checkView(o, v, opt.Queue, b.Zone()); err != nil {
		return nil, d.refuse(b, o, err)
	}
	if err := d.ready(o, opt.Queue); err != nil {
		return nil, d.refuse(b, o, err)
	}
	return d.send(b, o, step, opt, v.String())
}

// ready refuses a send the session cannot take as it stands.
func (d *Driver) ready(o *Observation, queue bool) error {
	st := o.Session.Status
	switch {
	case o.LoginRejected():
		msg := ""
		if o.Frontier.Error != nil {
			msg = fmt.Sprintf(": it answered %q", o.Frontier.Error.Text)
		}
		return &Refused{Reason: fmt.Sprintf("%s's login was rejected (account %s)%s", o.Label, o.Session.Account, msg),
			Next: d.relaunchHint(o)}
	case st.State == "blocked" && !queue:
		call := ""
		if p := o.Frontier.Pending; p != nil {
			call = fmt.Sprintf(" for %s: %s", p.Name, clip(p.Input, 120))
		}
		return &Refused{Reason: fmt.Sprintf("%s waits on a %s prompt%s", o.Label, orWord(st.Kind, "permission"), call),
			Next: fmt.Sprintf("answer it first: drive respond %s approve|deny", o.View)}
	case st.State == "working" && o.Frontier.Error == nil && !queue:
		return &Refused{Reason: fmt.Sprintf("%s is working (for %s)", o.Label, since(d.now(), st.UpdatedAt)),
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

func (d *Driver) refuse(b *batch.Batch, o *Observation, err error) error {
	var r *Refused
	if b != nil && errors.As(err, &r) {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindRefused, Session: o.Label, Reason: r.Reason})
	}
	return err
}

func newIntent() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// payload is a step made ready to type.
type payload struct {
	typed string // the line typed into the session
	path  string
	mode  string // typed or file
	key   string
	from  string
}

// compose turns a step into what is typed, writing a file step under the
// batch so what the session read stays as it was.
func (d *Driver) compose(b *batch.Batch, o *Observation, step Step, notice bool) (payload, error) {
	var body, key, from string
	switch {
	case step.Line != "":
		if strings.Contains(step.Line, "\n") || len(step.Line) > maxLine {
			return payload{}, &Refused{Reason: "a typed step is one short line: Claude Code keeps only the end of long typing",
				Next: "write it to a file and send it with --file"}
		}
		if !notice {
			return payload{typed: step.Line, mode: "typed"}, nil
		}
		body = step.Line
	case step.Key != "":
		sn, err := snippet.Expand(d.Cfg.Home, o.Session.Dir, step.Key, step.Text)
		if err != nil {
			return payload{}, &Refused{Reason: err.Error()}
		}
		key, from, body = sn.Key, sn.From, sn.Text
		if !strings.Contains(body, "\n") && len(body) <= maxLine && skill.Command(body) != "" {
			// A one-line slash command, such as planlab's closeout: typed.
			if !notice {
				return payload{typed: body, mode: "typed", key: key, from: from}, nil
			}
		}
	default:
		data, err := os.ReadFile(step.File)
		if err != nil {
			return payload{}, &Refused{Reason: fmt.Sprintf("cannot read %s: %v", step.File, err)}
		}
		body = string(data)
	}
	first := firstLine(body)
	if name := skill.Command(first); name != "" && skill.TypedOnly(d.Cfg.Home, o.Session.Config, o.Session.Dir, name) {
		return payload{}, &Refused{
			Reason: fmt.Sprintf("/%s starts only when typed: a session told to run it from a file refuses", name),
			Next: fmt.Sprintf("type the command and point it at the rest: drive send %s %q",
				o.View, clip(first, 120)+" — read <file> first")}
	}
	if notice {
		text, err := os.ReadFile(d.Cfg.Notice)
		if err != nil {
			return payload{}, &Failed{What: fmt.Sprintf("the notice a session reads before its first step is missing (%s): %v", d.Cfg.Notice, err),
				Next: "restore it, or set notice in ~/.config/drive/config.toml"}
		}
		body = strings.TrimRight(string(text), "\n") + "\n\n" + body
	}
	path, err := b.SaveSend(o.Label, []byte(strings.TrimRight(body, "\n")+"\n"))
	if err != nil {
		return payload{}, err
	}
	return payload{typed: pointer(path), path: path, mode: "file", key: key, from: from}, nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// send admits, records, delivers and confirms one step. The session has
// passed its checks; o is what they read.
func (d *Driver) send(b *batch.Batch, o *Observation, step Step, opt SendOptions, view string) (*Sent, error) {
	intent := newIntent()
	var notice bool
	err := b.Locked(func(evs []batch.Event) error {
		members := batch.Members(evs)
		m := batch.Find(members, o.Label)
		if p := batch.Pending(evs, o.Label); p != nil && d.now().Sub(p.At) < batch.InFlight {
			return &Refused{Reason: fmt.Sprintf("another driver is sending to %s (since %s): %s", o.Label, p.At.In(b.Zone()).Format("15:04:05"), clip(p.Text, 80)),
				Next: fmt.Sprintf("let it finish; drive status shows how it went, then drive read %s", o.Label)}
		}
		if opt.Hold != "" {
			if h, held := d.holder(evs, members, opt.Hold, o.Label); held {
				return &Refused{Reason: fmt.Sprintf("%s is held by %s since %s, whose turn is still running", opt.Hold, h.Session, h.Since.In(b.Zone()).Format("15:04")),
					Next: fmt.Sprintf("send this once %s's turn ends: drive wait %s", h.Session, h.Session)}
			}
		}
		notice = m == nil || m.Sends == 0
		var add []batch.Event
		if m == nil {
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
		add = append(add, batch.Event{At: d.now(), Kind: batch.KindSendIntent, Session: o.Label, Intent: intent,
			Hold: opt.Hold, View: view, Key: step.Key, Text: clip(stepSummary(step), 200)})
		return b.Append(add...)
	})
	if err != nil {
		return nil, d.refuse(b, o, err)
	}
	sent, err := d.deliverStep(b, o, step, opt, notice)
	ev := batch.Event{At: d.now(), Kind: batch.KindSend, Session: o.Label, Intent: intent, Hold: opt.Hold, View: view, Notice: notice}
	if sent != nil {
		ev.Outcome, ev.Mode, ev.Text, ev.Path, ev.Key = sent.Outcome, sentMode(sent), sent.Typed, sent.Path, step.Key
	}
	if err != nil {
		var f *Failed
		if errors.As(err, &f) && sent != nil && sent.Outcome != "" {
			// It reached the session and the session failed: a send that happened.
			_ = b.Append(ev)
		}
		reason := err.Error()
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: o.Label, Intent: intent, Reason: reason})
		return sent, err
	}
	if err := b.Append(ev); err != nil {
		return sent, err
	}
	if !opt.NoWatch && b.Meta.Wake == "webhook" {
		if werr := d.arm(b, o.Label, o.Session.Block); werr == nil {
			sent.Watched = true
		}
	}
	return sent, nil
}

func sentMode(s *Sent) string {
	if s.Path != "" {
		return "file"
	}
	return "typed"
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

// deliverStep types the step, sending the notice first when a typed step is
// the first a session gets, and confirms it.
func (d *Driver) deliverStep(b *batch.Batch, o *Observation, step Step, opt SendOptions, notice bool) (*Sent, error) {
	if notice && step.Line != "" || notice && step.Key != "" && d.typedSnippet(o, step) {
		// The notice goes alone as a file; the typed step follows once its turn ends.
		pre, err := d.compose(b, o, Step{File: d.Cfg.Notice}, false)
		if err != nil {
			return nil, err
		}
		if _, err := d.deliver(o, pre.typed, opt.Queue); err != nil {
			return nil, err
		}
		if err := d.awaitTurn(o, 5*time.Minute); err != nil {
			return nil, err
		}
		notice = false
	}
	p, err := d.compose(b, o, step, notice)
	if err != nil {
		return nil, err
	}
	outcome, err := d.deliver(o, p.typed, opt.Queue)
	sent := &Sent{Label: o.Label, Outcome: outcome, Typed: p.typed, Path: p.path, Notice: notice, From: p.from}
	return sent, err
}

func (d *Driver) typedSnippet(o *Observation, step Step) bool {
	sn, err := snippet.Expand(d.Cfg.Home, o.Session.Dir, step.Key, step.Text)
	return err == nil && !strings.Contains(sn.Text, "\n") && skill.Command(sn.Text) != ""
}

// awaitTurn waits for the session's current turn to end.
func (d *Driver) awaitTurn(o *Observation, limit time.Duration) error {
	end := d.now().Add(limit)
	for d.now().Before(end) {
		d.sleep(d.poll())
		st, err := d.Term.Status(o.Session.Block)
		if err != nil || st == nil {
			return &Failed{What: fmt.Sprintf("%s's session went away while it read the notice", o.Label)}
		}
		if st.State == "done" || st.State == "blocked" || st.State == "error" {
			return nil
		}
	}
	return &Failed{What: fmt.Sprintf("%s did not finish reading the notice within %s", o.Label, limit),
		Next: fmt.Sprintf("drive read %s, then send the step when its turn ends", o.Label)}
}

const pad = "        "

// deliver types a line, submits it and confirms from the transcript that it
// arrived whole. Typing sometimes drops a line's first characters; the line
// is then taken back and typed again behind eight spaces, which absorb the
// loss and change nothing a session reads.
func (d *Driver) deliver(o *Observation, line string, queue bool) (string, error) {
	outcome, got, err := d.typeAndConfirm(o, line, queue)
	if err != nil || got == "" {
		return outcome, err
	}
	// got is the line without its head: take it back and type it again.
	_ = d.Term.Key(o.Session.Block, "ctrl+c")
	d.sleep(d.poll())
	for i := 0; i < 5; i++ {
		screen, err := d.Term.Capture(o.Session.Block, true)
		if err != nil {
			break
		}
		if k, _ := turn.InputBox(screen); k != turn.InputTyped {
			break
		}
		_ = d.Term.Key(o.Session.Block, "ctrl+u")
	}
	outcome, got, err = d.typeAndConfirm(o, pad+line, queue)
	if err != nil {
		return outcome, err
	}
	if got != "" {
		return "", &Failed{What: fmt.Sprintf("%s lost the start of the line twice; it last received %q", o.Label, clip(got, 120)),
			Next: fmt.Sprintf("it is working on that; interrupt it (drive respond %s interrupt) and send again", o.Label)}
	}
	return "recovered", nil
}

// typeAndConfirm returns the outcome, or the text that arrived when it is
// the line with its head lost.
func (d *Driver) typeAndConfirm(o *Observation, line string, queue bool) (string, string, error) {
	start := d.now()
	block := o.Session.Block
	if err := d.Term.Type(block, line); err != nil {
		return "", "", err
	}
	d.sleep(time.Second)
	if err := d.Term.Key(block, "enter"); err != nil {
		return "", "", err
	}
	want := strings.TrimLeft(line, " ")
	limit := time.Minute
	if strings.HasPrefix(want, "/compact") {
		limit = 10 * time.Minute // it records itself only once compaction ends
	}
	for end := start.Add(limit); d.now().Before(end); {
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
		if p == nil || p.At.Before(start.Add(-2*time.Second)) {
			continue
		}
		if !p.Known {
			// Claude Code queues a slash command typed while busy without its text.
			if skill.Command(want) != "" {
				return "queued", "", nil
			}
			continue
		}
		got := strings.TrimLeft(p.Text, " ")
		switch {
		case got == want:
			if p.Queued {
				return "queued", "", nil
			}
			return "arrived", "", d.checkAnswer(o, p.At)
		case got != "" && strings.HasSuffix(want, got):
			return "", got, nil
		default:
			return "", "", &Failed{What: fmt.Sprintf("%s received %q instead of what was typed", o.Label, clip(got, 120)),
				Next: fmt.Sprintf("it is working on that now; interrupt it (drive respond %s interrupt) and send again", o.Label)}
		}
	}
	return "", "", &Failed{What: fmt.Sprintf("nothing reached %s's transcript within %s; the send is recorded as unknown", o.Label, limit),
		Next: fmt.Sprintf("drive read %s --user shows what it last received", o.Label)}
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
