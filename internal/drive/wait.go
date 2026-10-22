package drive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
	"github.com/qiushiyan/drive/internal/webhook"
)

// Ending is how a session's turn ended.
type Ending struct {
	Label   string
	Block   string
	State   string // done, blocked:permission, blocked:question, blocked:auth, error, gone
	Msg     string // what Rex was told, such as the prompt's question
	Error   *turn.APIError
	Shells  int // background shells started in this turn, still running
	Orphans int // background shells older than this turn's prompt
	Capped  bool
	Reply   string // the newest reply's end
	Pending *turn.ToolCall
	Screen  string // the screen's end, for a prompt the transcript does not show
	View    turn.View
	Account string
	Next    string // what the driver does about it, where the ending decides that
}

// Wait blocks until a session's turn ends and has stayed ended for quiet: it
// is done with no shell this turn started still running, it waits on a
// prompt, or an API error ended it. A turn that ended while its own
// background jobs run usually resumes when they finish but may also have
// ended on a question, so after bgCap the wait returns anyway.
func (d *Driver) Wait(ref string, quiet, bgCap time.Duration) (*Ending, error) {
	_, _, members, err := d.members()
	if err != nil {
		return nil, err
	}
	var last string
	stable := d.now()
	for {
		o, err := d.observe(members, ref)
		if err != nil {
			if _, ok := err.(*Refused); ok {
				return &Ending{Label: ref, State: "gone", Msg: err.Error()}, nil
			}
			return nil, err
		}
		st := o.Session.Status
		key := st.State + st.UpdatedAt.String()
		if key != last {
			last, stable = key, d.now()
		}
		held := d.now().Sub(stable)
		var promptAt time.Time
		if p := o.Frontier.Newest(); p != nil {
			promptAt = p.At
		}
		shells, orphans := 0, 0
		for _, at := range d.Host.Shells(st.PID) {
			if !promptAt.IsZero() && at.Before(promptAt) {
				orphans++
			} else {
				shells++
			}
		}
		state := o.State()
		ended := false
		switch {
		case state == "blocked:auth" || state == "error" || state == "interrupted":
			ended = true // nothing will happen until someone acts
		case strings.HasPrefix(state, "blocked"):
			ended = held >= quiet
		case state == "done" && shells == 0:
			ended = held >= quiet
		case state == "done":
			ended = held >= bgCap
		}
		if ended {
			e := &Ending{Label: o.Label, Block: o.Session.Block, State: state, Msg: st.Msg, Error: o.Frontier.Error,
				Shells: shells, Orphans: orphans, Capped: shells > 0, Pending: o.Frontier.Pending, View: o.View,
				Account: o.Session.Account}
			if o.Frontier.Reply != nil {
				e.Reply = tail(o.Frontier.Reply.Text, 2500)
			}
			if strings.HasPrefix(state, "blocked") {
				if screen, err := d.Term.Capture(o.Session.Block, false); err == nil {
					e.Screen = lastLines(screen, 25)
				}
			}
			switch {
			case state == "blocked:auth":
				e.Next = d.relaunchHint(o)
			case strings.HasPrefix(state, "blocked"):
				e.Next = fmt.Sprintf("see what it asks (drive read %s --pending), then answer it: drive respond %s approve|deny", o.Label, o.View)
			case state == "error":
				e.Next = fmt.Sprintf("read it (drive read %s); send its step again once the API recovers", o.Label)
			case state == "interrupted":
				e.Next = fmt.Sprintf("its turn was interrupted; read where it stopped (drive read %s) before its next step", o.Label)
			case e.Capped:
				e.Next = fmt.Sprintf("if the reply says it waits on its own job, watch it again (drive watch %s); otherwise send its next step", o.Label)
			}
			return e, nil
		}
		d.sleep(5 * d.poll() / 2)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Text renders an ending for a reader deciding the next step.
func (e *Ending) Text() string {
	var b strings.Builder
	what := map[string]string{
		"done": "its turn ended", "blocked:permission": "it waits on a permission prompt",
		"blocked:question": "it waits on a question", "blocked:auth": "its login was rejected",
		"error": "an API error ended its turn", "gone": "its session is gone",
		"interrupted": "its turn was interrupted",
	}[e.State]
	if what == "" {
		what = e.State
	}
	fmt.Fprintf(&b, "%s (%s): %s\n", e.Label, rex.Short(e.Block), what)
	if e.Error != nil {
		fmt.Fprintf(&b, "error: %s — %q (account %s)\n", e.Error.Kind, e.Error.Text, e.Account)
	}
	if e.Capped {
		fmt.Fprintf(&b, "background: %d shell(s) this turn started still run; the turn may resume when they finish, or it ended on a question — read the reply\n", e.Shells)
	}
	if e.Orphans > 0 {
		fmt.Fprintf(&b, "background: %d shell(s) older than this turn's prompt (left over, not waited on)\n", e.Orphans)
	}
	if e.Pending != nil && strings.HasPrefix(e.State, "blocked") {
		fmt.Fprintf(&b, "pending %s: %s\n", e.Pending.Name, clip(e.Pending.Input, 400))
	}
	if e.Msg != "" && strings.HasPrefix(e.State, "blocked") {
		fmt.Fprintf(&b, "prompt: %s\n", e.Msg)
	}
	if e.Reply != "" {
		fmt.Fprintf(&b, "--- reply (end) ---\n%s\n", e.Reply)
	}
	if e.Screen != "" {
		fmt.Fprintf(&b, "--- screen (end) ---\n%s\n", e.Screen)
	}
	if e.View.Session != "" {
		fmt.Fprintf(&b, "view: %s\n", e.View)
	}
	if e.Next != "" {
		fmt.Fprintf(&b, "next: %s\n", e.Next)
	}
	return b.String()
}

func watchName(block string) string { return "drive-" + rex.Short(block) }

// arm starts the wake for a session unless one is already waiting on it. A
// watch takes itself off the books before it posts, so one still running
// waits on a turn that has not ended.
func (d *Driver) arm(b *batch.Batch, label, block string) error {
	if d.Watches == nil {
		return fmt.Errorf("this driver runs no watches")
	}
	name := watchName(block)
	if d.Watches.Alive(name) {
		return nil
	}
	if err := d.Watches.Start(name, []string{d.Self, "--batch", b.Meta.Name, "wait", label, "--notify"}, watchLog(b)); err != nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Reason: "its wake did not start: " + err.Error()})
		return err
	}
	return b.Append(batch.Event{At: d.now(), Kind: batch.KindWatch, Session: label, Block: block})
}

// Watched reports a watch.
type Watched struct {
	Label     string
	Already   bool
	Restarted bool // the watch ran long after the turn ended and was started again
}

// Watch arms the wake for a session in a batch that wakes its driver by webhook.
func (d *Driver) Watch(ref string) (*Watched, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	if b.Meta.Wake != "webhook" {
		return nil, &Refused{Reason: fmt.Sprintf("batch %s wakes no one (wake: %s)", b.Meta.Name, b.Meta.Wake),
			Next: "run `drive wait SESSION` in the background instead"}
	}
	if d.Cfg.Webhook.URL == "" {
		return nil, &Refused{Reason: "no webhook is configured", Next: "write URL= and AUTH= lines to " + d.Cfg.WebhookPath()}
	}
	evs, err := b.Events()
	if err != nil {
		return nil, err
	}
	o, err := d.observe(batch.Members(evs), ref)
	if err != nil {
		return nil, err
	}
	name := watchName(o.Session.Block)
	if d.Watches.Alive(name) {
		r := Row{State: o.State(), For: d.now().Sub(o.Session.Status.UpdatedAt)}
		if !stuck(r) {
			return &Watched{Label: o.Label, Already: true}, nil
		}
		if err := d.Watches.Stop(name); err != nil {
			return nil, err
		}
		return &Watched{Label: o.Label, Restarted: true}, d.arm(b, o.Label, o.Session.Block)
	}
	return &Watched{Label: o.Label}, d.arm(b, o.Label, o.Session.Block)
}

// Timer arms a wake after a delay, such as the five-minute veto on a plan.
func (d *Driver) Timer(after time.Duration, note string) error {
	b, err := d.Batch()
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	name := "drive-timer-" + safeName(note)
	if d.Watches.Alive(name) {
		return &Refused{Reason: fmt.Sprintf("a timer named %q is already running", note), Next: "give this one another note"}
	}
	argv := []string{d.Self, "--batch", b.Meta.Name, "wait", "--sleep", fmt.Sprint(int(after.Seconds())), "--note", note, "--notify"}
	if err := d.Watches.Start(name, argv, watchLog(b)); err != nil {
		return err
	}
	return b.Append(batch.Event{At: d.now(), Kind: batch.KindWatch, Note: "timer " + note})
}

func watchLog(b *batch.Batch) string { return filepath.Join(b.Dir, "watches.log") }

// WatchFailed records a watch that ended without posting its wake.
func (d *Driver) WatchFailed(label string, err error) {
	if b, berr := d.Batch(); berr == nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Reason: "watch ended without a wake: " + firstLine(err.Error())})
	}
}

// Notify is a watch's own run: wait for the session's turn to end, take the
// watch off the books so a step sent from now on arms a fresh one, then wake
// the driver. watch is the watch's name, "" when run by hand.
func (d *Driver) Notify(watch, ref string, quiet, bgCap time.Duration) (*Ending, error) {
	e, err := d.Wait(ref, quiet, bgCap)
	if watch != "" && d.Watches != nil {
		d.Watches.Retire(watch)
	}
	if err != nil {
		d.WatchFailed(ref, err)
		return nil, err
	}
	if err := d.Wake(e.Label, e, ""); err != nil {
		d.WatchFailed(e.Label, err)
		return e, err
	}
	return e, nil
}

// Ring is a timer's own run once its delay is over.
func (d *Driver) Ring(watch, note string) error {
	if watch != "" && d.Watches != nil {
		d.Watches.Retire(watch)
	}
	if err := d.Wake("", nil, note); err != nil {
		d.WatchFailed("", err)
		return err
	}
	return nil
}

// Killed records a watch ended by a signal, unless drive stopped it: Stop
// takes a watch off the books before signalling it. It says whether the
// signal came from elsewhere.
func (d *Driver) Killed(watch, label string, sig os.Signal) bool {
	if d.Watches == nil || !d.Watches.Retire(watch) {
		return false
	}
	d.WatchFailed(label, fmt.Errorf("killed by %s", sig))
	return true
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// Wake posts an ending or a timer to the driver's webhook and records how
// the post went: an empty log line is never taken to mean the wake arrived.
func (d *Driver) Wake(label string, e *Ending, note string) error {
	b, err := d.Batch()
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	payload := map[string]any{"batch": b.Meta.Name, "host": host}
	if e != nil {
		payload["kind"], payload["session"], payload["state"], payload["view"] = "turn", e.Label, e.State, e.View.String()
		text := e.Text()
		if evs, err := b.Events(); err == nil {
			for _, h := range batch.Holds(evs) {
				if h.Session != e.Label {
					continue
				}
				for _, w := range batch.Waiting(evs) {
					if w.Name == h.Name {
						text += fmt.Sprintf("waiting for %s, which %s held: %s, refused %s; send it once the hold is free (drive status)\n",
							h.Name, e.Label, w.Session, clip(w.Step, 80))
					}
				}
			}
		}
		payload["output"] = tail(text, 6000)
	} else {
		payload["kind"], payload["output"] = "timer", "timer done: "+note
	}
	body, _ := json.Marshal(payload)
	poster := webhook.Poster{URL: d.Cfg.Webhook.URL, Auth: d.Cfg.Webhook.Auth, Sleep: d.Sleep}
	var last webhook.Attempt
	err = poster.Post(body, func(a webhook.Attempt) { last = a })
	ev := batch.Event{At: d.now(), Kind: batch.KindWake, Session: label, HTTP: last.HTTP, Try: last.Try, Note: note}
	if e != nil {
		ev.Block, ev.Outcome, ev.View = e.Block, e.State, e.View.String()
	}
	if err != nil {
		ev.HTTP, ev.Reason = 0, clip(err.Error(), 200)
		if last.Try > 0 {
			ev.Reason = fmt.Sprintf("http %d %s", last.HTTP, clip(last.Err, 200))
		}
	}
	_ = b.Append(ev)
	for _, c := range append(append([]string{}, b.Meta.AfterWake...), d.Cfg.AfterWake...) {
		if out, cerr := exec.Command("/bin/sh", "-c", c).CombinedOutput(); cerr != nil {
			_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindNote, Note: fmt.Sprintf("after_wake %q failed: %s", c, clip(string(out), 200))})
		}
	}
	return err
}

// holder says whether a hold is taken by a session other than label: its
// send is still under way, or the turn that send started has not ended. A
// holder that cannot be read holds; only one that is gone does not.
func (d *Driver) holder(evs []batch.Event, members []*batch.Member, name, label string) (batch.Hold, bool) {
	for _, h := range batch.Holds(evs) {
		if h.Name != name || h.Session == label {
			continue
		}
		m := batch.Find(members, h.Session)
		if m == nil || m.Closed {
			return h, false
		}
		if h.Pending && d.Store.Busy(m.Block) != nil {
			return h, true
		}
		s, err := session.Locate(d.Term, d.Host, m.Block)
		if errors.Is(err, session.ErrNoClaude) || err != nil && !d.blockExists(m.Block) {
			return h, false
		}
		if err != nil {
			return h, true
		}
		f, err := s.Frontier()
		if err != nil {
			return h, true
		}
		if state := turnState(s.Status, f); state == "working" || strings.HasPrefix(state, "blocked") && state != "blocked:auth" {
			return h, true
		}
		if p := f.Newest(); p == nil || p.At.Before(h.Since) {
			return h, false // the send never reached it
		}
		if !f.Answered() {
			return h, true
		}
		// A turn that ended while a shell it started runs, such as a wait on
		// checks before a merge, resumes when the shell ends.
		for _, at := range d.Host.Shells(s.Status.PID) {
			if !at.Before(h.Since) {
				return h, true
			}
		}
		return h, false
	}
	return batch.Hold{}, false
}

func (d *Driver) blockExists(block string) bool {
	blocks, err := d.Term.Blocks("")
	if err != nil {
		return true
	}
	for _, b := range blocks {
		if b.ID == block {
			return true
		}
	}
	return false
}
