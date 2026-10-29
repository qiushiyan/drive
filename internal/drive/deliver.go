package drive

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/skill"
	"github.com/qiushiyan/drive/internal/turn"
)

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
		_ = a.press("end", "ctrl+u") // ctrl+u deletes what stands before the cursor
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
	if err := a.insert(); err != nil {
		return "", nil, err
	}
	if err := a.typeInto(keystrokes(line)); err != nil {
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

// keystrokes is what is typed for line. A line ending in an @-mention holds
// Claude Code's file completion open, and Enter would take the completion
// and send nothing; a space after the path closes it.
func keystrokes(line string) string {
	if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[len(f)-1], "@") {
		return strings.TrimRight(line, " ") + " "
	}
	return line
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
